"""HY-ASR-3-Stream 流式识别客户端（asr-gateway /tencent/asr/recognize/stream）。

协议（与 /tencent/asr/recognize/stream 对齐）：
    客户端 → 服务端：
        {"event":"asr.start","voice_id","session_id","user_id","model",
         "audio":{"format":"pcm","sample_rate":16000,"channel":1},
         "options":{"enable_semantic_vad":true}}   文本  开启会话
        [binary PCM]                                 二进制 16k/mono/int16 帧
    服务端 → 客户端：
        {"event":"asr.started"}                                      会话就绪
        {"event":"asr.result","data":{                               增量结果
            "text":"累计全文",
            "sentences":[{"index":0,"text":"当前句","begin_time":0,
                          "end_time":0,"is_final":false}]}}          逐词增量
        {"event":"asr.done","code":0,"data":{"text":"最终全文"}}      会话结束

关键特性：
- **逐词增量**：asr.result 的 sentences[last].text 逐字/逐词更新 → 实时草稿；
- **语义 VAD 自动断句**：句子说完（is_final=true）即确认，无需客户端切句；
- **回退修正**：已确认句文本变化 → on_revise 回调，可覆盖更新显示。
"""
from __future__ import annotations

import json
import threading
import time
import uuid
from collections.abc import Callable

import websocket  # websocket-client

OnText = Callable[[str], None]


class HyAsrStreamClient:
    """HY-ASR-3-Stream 流式客户端。接口与 FunAsrNanoStreamClient 保持一致。"""

    def __init__(
        self,
        url: str,
        token: str,
        model: str = "HY-ASR-3-Stream",
        hotwords: list[str] | None = None,
        on_online: OnText | None = None,
        on_offline: OnText | None = None,
        on_status: Callable[[str], None] | None = None,
        on_revise: Callable[[str, str], None] | None = None,
    ) -> None:
        self.base_url = url.rstrip("/")  # 不含 query，连接时拼 model/token
        self.token = token
        self.model = model
        self.hotwords = hotwords or []
        self.on_online = on_online
        self.on_offline = on_offline
        self.on_status = on_status
        self.on_revise = on_revise

        self._ws: websocket.WebSocket | None = None
        self._recv_thread: threading.Thread | None = None
        self._lock = threading.Lock()
        self._closed = False
        self._sent_count = 0  # 已确认(final)句子数
        self._sent_texts: list[str] = []  # 已确认句子文本（回退修正检测）
        self._round = 0
        self.last_sentence_key = ""  # 最近回调对应的句子键 r{轮次}i{序号}
        self._retry = 0
        self.status = "idle"          # idle/connected/connecting/error/closed
        self.status_detail = ""
        self.last_active = 0.0

    # ---- 连接 ----
    def status_info(self) -> dict:
        idle = 0.0
        if self.last_active > 0:
            idle = time.time() - self.last_active
        return {"state": self.status, "detail": self.status_detail, "idle_sec": round(idle, 1)}

    def _status(self, s: str) -> None:
        if s == "connected":
            self.status, self.status_detail = "connected", ""
        elif s.startswith("connect_failed:"):
            self.status, self.status_detail = "connecting", s[len("connect_failed:"):]
        elif s.startswith("error:"):
            self.status, self.status_detail = "error", s[len("error:"):]
        elif s == "closed":
            self.status, self.status_detail = "closed", ""
        else:
            self.status = s
        if self.on_status:
            self.on_status(s)

    def _url(self) -> str:
        return f"{self.base_url}?model={self.model}&token={self.token}"

    def _start_msg(self) -> dict:
        opt: dict = {"enable_semantic_vad": True}
        if self.hotwords:
            opt["hotwords"] = list(self.hotwords)
        return {
            "event": "asr.start",
            "voice_id": str(uuid.uuid4()),
            "session_id": str(uuid.uuid4()),
            "user_id": "translation-seat",
            "model": self.model,
            "audio": {"format": "pcm", "sample_rate": 16000, "channel": 1},
            "options": opt,
        }

    def _ensure_connected(self) -> bool:
        if self._ws is not None and not self._closed:
            return True
        # 自适应退避：连续失败越多退避越长（上限 15s），
        # 避免高频重试触发 asr-gateway 网关的限流（实测会雪崩式握手失败）
        delay = min(2 ** max(self._retry - 2, 0), 15.0)
        self._retry += 1
        if delay > 0.5:
            time.sleep(delay)
        try:
            ws = websocket.create_connection(self._url(), timeout=10)
            ws.send(json.dumps(self._start_msg(), ensure_ascii=False))
            first = json.loads(ws.recv())
            if first.get("event") != "asr.started":
                raise RuntimeError(f"未收到 asr.started: {str(first)[:120]}")
            self._ws = ws
            self._closed = False
            self._sent_count = 0
            self._sent_texts = []
            self._round += 1
            self._retry = 0
            self._status("connected")
            self._recv_thread = threading.Thread(target=self._recv_loop, daemon=True)
            self._recv_thread.start()
            return True
        except Exception as e:  # noqa: BLE001
            self._status(f"connect_failed:{e} (第{self._retry}次)")
            return False

    def update_hotwords(self, words: dict[str, int]) -> None:
        """更新热词并断开当前连接（下次 send 自动重连并携带新热词）。"""
        self.hotwords = list(words.keys())
        with self._lock:
            ws = self._ws
            self._ws = None
            self._closed = True
        if ws is not None:
            try:
                ws.close()
            except Exception:
                pass

    # ---- 发送 ----
    def send_audio(self, pcm: bytes) -> None:
        with self._lock:
            if not self._ensure_connected():
                return
            try:
                self._ws.send_binary(pcm)
            except Exception as e:  # noqa: BLE001
                self._closed = True
                self._status(f"error:{e}")

    def mark_end(self) -> None:
        # 语义 VAD 自动断句；会话进行中无需 asr.end（保持长连接持续转写）。
        pass

    # ---- 接收 ----
    def _recv_loop(self) -> None:
        try:
            while not self._closed:
                msg = self._ws.recv()
                if msg is None or msg == "":
                    continue
                if isinstance(msg, (bytes, bytearray)):
                    continue
                self._handle(json.loads(msg))
        except Exception as e:  # noqa: BLE001
            self._fail(str(e))

    def _handle(self, data: dict) -> None:
        self.last_active = time.time()
        event = data.get("event")
        if event == "asr.result":
            d = data.get("data") or {}
            self._handle_sentences(d.get("sentences") or [])
            # 草稿：当前句（最后一个未确认句）的增量文本
            sentences = d.get("sentences") or []
            if self.on_online and sentences:
                draft = (sentences[-1].get("text") or "").strip()
                self.on_online(draft)
        elif event in ("asr.done", "asr.error", "asr.failed"):
            code = data.get("code")
            if code not in (0, None):
                self._status(f"error:{event}:{data.get('message', '')}")

    def _handle_sentences(self, sentences: list[dict]) -> None:
        """按 index 处理：已确认句文本变化 → 修正；新确认句 → 终稿。"""
        if not sentences:
            return
        # 1) 已确认句（index < _sent_count）文本变化 → 回退修正
        if self.on_revise:
            upper = min(self._sent_count, len(sentences), len(self._sent_texts))
            for i in range(upper):
                new_text = (sentences[i].get("text") or "").strip()
                if new_text and new_text != self._sent_texts[i]:
                    self._sent_texts[i] = new_text
                    self.last_sentence_key = f"r{self._round}i{i}"
                    self.on_revise(self.last_sentence_key, new_text)
        # 2) 新确认句（is_final=true 且序号 >= _sent_count）→ 终稿
        for s in sentences[self._sent_count:]:
            if not s.get("is_final"):
                break
            text = (s.get("text") or "").strip()
            self._sent_texts.append(text)
            if text and self.on_offline:
                self.last_sentence_key = f"r{self._round}i{s.get('index', self._sent_count)}"
                self.on_offline(text)
            self._sent_count += 1

    def _fail(self, msg: str) -> None:
        if self._closed:
            return
        self._closed = True
        self._status(f"error:{msg}")

    def close(self) -> None:
        self._closed = True
        ws = self._ws
        self._ws = None
        if ws is not None:
            try:
                ws.close()
            except Exception:
                pass
