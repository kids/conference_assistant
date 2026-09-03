"""Qwen3-ASR HTTP 实时客户端:WS 流式体验由客户端拼装(无需服务端封装层)。

底层: vLLM OpenAI 兼容转写 POST /v1/audio/transcriptions(整段 WAV → 文本,单段 ≤30s)。
策略(与 qwen3_asr_ws 封装层同源,线上实测依据):
- partial:每 partial_interval 秒音频,把自上一确认句起的累积缓冲整段重推一次,
  草稿单调增长(实测前缀稳定、词中截断自动修正,RTF 0.03~0.07);
- final  :webrtcvad 检测句尾静音(vad_silence_ms)后推理确认句,清空缓冲;
- 保护   :单段达 max_segment_s 强制切句;纯静音段不推理(避免整段静音识别出「嗯。」)。

接口与 FunAsrNanoStreamClient 一致;配合 pipeline 的 server_vad=True(持续透传),
VAD 切句与草稿调度都在本类内部完成。
"""
from __future__ import annotations

import io
import json
import threading
import time
import wave
from collections.abc import Callable

import httpx
import webrtcvad

OnText = Callable[[str], None]

SAMPLE_RATE = 16000
FRAME_MS = 30
FRAME_BYTES = SAMPLE_RATE * 2 * FRAME_MS // 1000  # 960 samples * 2B

# 语言别名 → ISO 639-1(vLLM transcriptions 仅接受 ISO 码)
LANG_MAP = {"中文": "zh", "汉语": "zh", "英语": "en", "英文": "en", "日语": "ja", "韩语": "ko"}


def _pcm_to_wav(pcm: bytes, sr: int = SAMPLE_RATE) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm)
    return buf.getvalue()


class Qwen3AsrHttpClient:
    """Qwen3-ASR HTTP 转写客户端(模拟流式)。接口与 FunAsrNanoStreamClient 保持一致。"""

    def __init__(
        self,
        base_url: str,
        model: str = "qwen3asr17b",
        language: str = "中文",
        hotwords: list[str] | None = None,
        partial_interval_s: float = 1.0,
        vad_silence_ms: int = 600,
        vad_aggressiveness: int = 2,
        max_segment_s: float = 15.0,
        infer_timeout_s: float = 30.0,
        on_online: OnText | None = None,
        on_offline: OnText | None = None,
        on_status: Callable[[str], None] | None = None,
        on_revise: Callable[[str, str], None] | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.language = LANG_MAP.get(language.strip().lower(), language if len(language) == 2 else "zh")
        self.hotwords = hotwords or []
        self.partial_interval_s = partial_interval_s
        self.vad_silence_ms = vad_silence_ms
        self.max_segment_s = max_segment_s
        self.infer_timeout_s = infer_timeout_s

        self.on_online = on_online
        self.on_offline = on_offline
        self.on_status = on_status
        self.on_revise = on_revise  # 预留:HTTP 整段推理无回退修正,当前不触发

        self._http = httpx.Client(timeout=infer_timeout_s)
        self._lock = threading.Lock()
        self._buf = bytearray()            # 自上一确认句起的音频
        self._frame_buf = bytearray()      # 任意 chunk → 30ms 帧重组
        self._stop = threading.Event()
        self._wake = threading.Event()     # 唤醒调度线程
        self._force_final = False          # mark_end() 请求强制切句

        # VAD 状态机
        self._vad = webrtcvad.Vad(vad_aggressiveness)
        self._in_speech = False
        self._speech_frames = 0
        self._silence_frames = 0
        self._speech_seen = False

        # 调度状态
        self._last_partial_sec = 0.0
        self._round = 0
        self.last_sentence_key = ""        # 与其他客户端一致的句键(main.py 映射用)
        self.status = "idle"
        self.status_detail = ""
        self.last_active = 0.0
        self._infer_fail = 0

        self._thread = threading.Thread(target=self._scheduler, daemon=True, name="qwen3-asr-http")
        self._thread.start()

    # ---- 对外接口(与 FunAsrNanoStreamClient 对齐) ----
    def status_info(self) -> dict:
        idle = 0.0
        if self.last_active > 0:
            idle = time.time() - self.last_active
        return {"state": self.status, "detail": self.status_detail, "idle_sec": round(idle, 1)}

    def update_hotwords(self, words: dict[str, int]) -> None:
        self.hotwords = list(words.keys())  # 下次推理即生效

    def send_audio(self, pcm: bytes) -> None:
        if self._stop.is_set():
            return
        with self._lock:
            self._frame_buf.extend(pcm)
            while len(self._frame_buf) >= FRAME_BYTES:
                frame = bytes(self._frame_buf[:FRAME_BYTES])
                del self._frame_buf[:FRAME_BYTES]
                self._feed_frame(frame)
        self._wake.set()

    def mark_end(self) -> None:
        """外部要求立即切句(pipeline 本地 VAD 场景);本类自管 VAD 时一般不触发。"""
        self._force_final = True
        self._wake.set()

    def close(self) -> None:
        self._stop.set()
        self._wake.set()
        try:
            self._thread.join(timeout=3)
        except Exception:
            pass
        try:
            self._http.close()
        except Exception:
            pass
        self._status("closed")

    # ---- 内部:VAD 与调度 ----
    def _status(self, s: str) -> None:
        if s == "connected":
            self.status, self.status_detail = "connected", ""
        elif s.startswith("error:"):
            self.status, self.status_detail = "error", s
        elif s == "closed":
            self.status, self.status_detail = "closed", ""
        else:
            self.status = s
        if self.on_status:
            self.on_status(s)

    @property
    def _buf_sec(self) -> float:
        return len(self._buf) / (SAMPLE_RATE * 2)

    def _feed_frame(self, frame: bytes) -> None:
        self._buf.extend(frame)
        try:
            is_speech = self._vad.is_speech(frame, SAMPLE_RATE)
        except Exception:  # noqa: BLE001
            is_speech = False
        if is_speech:
            self._speech_frames += 1
            self._silence_frames = 0
            if not self._in_speech and self._speech_frames >= 3:
                self._in_speech = True
        else:
            self._silence_frames += 1
            self._speech_frames = 0
            self._in_speech = False
        if self._in_speech or self._speech_frames > 0:
            self._speech_seen = True

    def _infer(self, pcm: bytes) -> str:
        data = {"model": self.model, "language": self.language}
        if self.hotwords:
            data["hotwords"] = ",".join(self.hotwords)
        try:
            r = self._http.post(
                f"{self.base_url}/v1/audio/transcriptions",
                files={"file": ("a.wav", _pcm_to_wav(pcm), "audio/wav")},
                data=data,
            )
            r.raise_for_status()
            self._infer_fail = 0
            return (r.json().get("text") or "").strip()
        except Exception as e:  # noqa: BLE001
            self._infer_fail += 1
            self._status(f"error:infer:{e}")
            return ""

    def _finalize(self) -> None:
        with self._lock:
            snap = bytes(self._buf)
        if not snap or not self._speech_seen:
            with self._lock:
                self._buf.clear()
                self._speech_seen = False
                self._last_partial_sec = 0.0
            return
        text = self._infer(snap)
        with self._lock:
            self._buf.clear()
            self._frame_buf.clear()
            self._speech_seen = False
            self._in_speech = False
            self._last_partial_sec = 0.0
            self._round += 1
        if text:
            self.last_sentence_key = f"h{self._round}"
            self.last_active = time.time()
            if self.on_offline:
                self.on_offline(text)
        self._status("connected")

    def _partial(self) -> None:
        with self._lock:
            snap = bytes(self._buf)
        if not snap:
            return
        text = self._infer(snap)
        with self._lock:
            expired = len(self._buf) < len(snap)  # 推理期间已被 final 清空
        if expired or not text:
            return
        self.last_active = time.time()
        if self.on_online:
            self.on_online(text)
        self._status("connected")

    def _scheduler(self) -> None:
        """调度线程:partial 周期重推 + VAD 句尾 final + 超长强制切句。"""
        while not self._stop.is_set():
            self._wake.wait(timeout=0.2)
            self._wake.clear()
            if self._stop.is_set():
                break
            with self._lock:
                buf_sec = self._buf_sec
                speech_seen = self._speech_seen
                silence_ms = self._silence_frames * FRAME_MS
                force = self._force_final
            if not speech_seen and not force:
                continue
            # 1) 句尾静音 / mark_end / 超长 → final
            if (speech_seen and silence_ms >= self.vad_silence_ms and not self._in_speech) \
                    or force or buf_sec >= self.max_segment_s:
                self._force_final = False
                self._finalize()
                continue
            # 2) 周期草稿重推
            if speech_seen and buf_sec - self._last_partial_sec >= self.partial_interval_s:
                self._last_partial_sec = buf_sec
                self._partial()
