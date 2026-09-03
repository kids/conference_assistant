"""FunASR 标准流式推理客户端（本项目独立实现）。

协议（对齐 FunASR 官方 websocket_protocol，服务地址参考 tan-asr/batch_asr.py）：
    1. 建立 WebSocket 连接 ws://host:port/
    2. 发送首条配置 JSON（mode/2pass、wav_name、is_speaking、wav_format=pcm、
       chunk_size、audio_fs、hotwords、itn）
    3. 持续发送 PCM 二进制帧（16k/mono/int16，无 WAV 头）
    4. 一句结束发送 {"is_speaking": false}
    5. 服务端返回 {"mode":"2pass-online","text":...,"is_final":false}  实时结果
               {"mode":"2pass-offline","text":...,"is_final":true}   句尾纠错终稿
"""
from __future__ import annotations

import json
import threading
import time
from collections.abc import Callable

import websocket  # websocket-client

OnText = Callable[[str], None]
OnError = Callable[[str], None]


class FunAsrStreamSession:
    """单条 WebSocket 长连接会话。发送线程与接收线程分离。"""

    def __init__(
        self,
        url: str,
        mode: str = "2pass",
        chunk_size: list[int] | None = None,
        hotwords: str = "",
        audio_fs: int = 16000,
        on_online: OnText | None = None,
        on_offline: OnText | None = None,
        on_error: OnError | None = None,
        on_close: Callable[[], None] | None = None,
    ) -> None:
        self.url = url
        self.mode = mode
        self.chunk_size = list(chunk_size) if chunk_size else [5, 10, 5]
        self.hotwords = hotwords
        self.audio_fs = audio_fs
        self.on_online = on_online
        self.on_offline = on_offline
        self.on_error = on_error
        self.on_close = on_close

        self._ws: websocket.WebSocket | None = None
        self._recv_thread: threading.Thread | None = None
        self._send_lock = threading.Lock()
        self._closed = False
        self._seq = 0

    # ---- 连接 ----
    def connect(self) -> None:
        ws = websocket.create_connection(self.url, timeout=10)
        self._ws = ws
        cfg: dict = {
            "mode": self.mode,
            "wav_name": self._next_name(),
            "is_speaking": True,
            "wav_format": "pcm",
            "chunk_size": self.chunk_size,
            "audio_fs": self.audio_fs,
            "itn": True,
        }
        if self.hotwords:
            cfg["hotwords"] = self.hotwords
        ws.send(json.dumps(cfg, ensure_ascii=False))
        self._recv_thread = threading.Thread(target=self._recv_loop, daemon=True)
        self._recv_thread.start()

    def _next_name(self) -> str:
        self._seq += 1
        return f"seg_{self._seq:04d}"

    # ---- 发送 ----
    def send_audio(self, pcm: bytes) -> None:
        if self._ws is None or self._closed:
            return
        with self._send_lock:
            try:
                self._ws.send_binary(pcm)
            except Exception as e:
                self._fail(str(e))

    def mark_end(self) -> None:
        if self._ws is None or self._closed:
            return
        with self._send_lock:
            try:
                self._ws.send(json.dumps({"is_speaking": False}))
            except Exception as e:
                self._fail(str(e))

    def mark_start(self) -> None:
        if self._ws is None or self._closed:
            return
        with self._send_lock:
            try:
                self._ws.send(json.dumps({"is_speaking": True, "wav_name": self._next_name()}))
            except Exception as e:
                self._fail(str(e))

    # ---- 接收 ----
    def _recv_loop(self) -> None:
        try:
            while not self._closed:
                msg = self._ws.recv()
                if msg is None or msg == "":
                    continue
                if isinstance(msg, (bytes, bytearray)):
                    continue  # 协议为 JSON 文本帧，忽略二进制
                self._handle(json.loads(msg))
        except Exception as e:
            self._fail(str(e))
        finally:
            if self.on_close:
                try:
                    self.on_close()
                except Exception:
                    pass

    def _handle(self, data: dict) -> None:
        mode = data.get("mode", "")
        text = data.get("text", "")
        is_final = bool(data.get("is_final", False))
        if mode.endswith("online"):
            if self.on_online and text:
                self.on_online(text)
        elif mode.endswith("offline") or is_final:
            if self.on_offline and text:
                self.on_offline(text)

    def _fail(self, msg: str) -> None:
        if self._closed:
            return
        self._closed = True
        if self.on_error:
            self.on_error(msg)

    def close(self) -> None:
        self._closed = True
        ws = self._ws
        self._ws = None
        if ws is not None:
            try:
                ws.close()
            except Exception:
                pass


class FunAsrStreamClient:
    """带自动重连的流式客户端，对外屏蔽连接细节。

    - send_audio()：先确保连接，必要时自动 mark_start 开启新句。
    - mark_end()：断句，发 is_speaking=false。
    """

    def __init__(
        self,
        url: str,
        mode: str = "2pass",
        chunk_size: list[int] | None = None,
        hotwords: str = "",
        audio_fs: int = 16000,
        on_online: OnText | None = None,
        on_offline: OnText | None = None,
        on_status: Callable[[str], None] | None = None,
    ) -> None:
        self.url = url
        self.mode = mode
        self.chunk_size = chunk_size
        self.hotwords = hotwords
        self.audio_fs = audio_fs
        self.on_online = on_online
        self.on_offline = on_offline
        self.on_status = on_status

        self._lock = threading.Lock()
        self._session: FunAsrStreamSession | None = None
        self._speaking = False
        self._retry = 0
        self.status = "idle"          # 归一化状态：idle/connected/connecting/error/closed
        self.status_detail = ""
        self.last_active = 0.0        # 最近一次收到服务端消息的时间戳

    def status_info(self) -> dict:
        """归一化状态信息，供 /api/health 与前端展示。"""
        idle = 0.0
        if self.last_active > 0:
            idle = time.time() - self.last_active
        return {
            "state": self.status,
            "detail": self.status_detail,
            "idle_sec": round(idle, 1),
        }

    def _wrap_activity(self, cb):
        """包装回调：收到服务端文本即记录活跃时间。"""

        def wrapper(text: str) -> None:
            self.last_active = time.time()
            if cb:
                cb(text)

        return wrapper

    def _status(self, s: str) -> None:
        # 归一化：把原始状态串映射为稳定状态机
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

    def _on_close(self) -> None:
        self._status("closed")
        # 连接断开，等待下次 send 时重建
        self._speaking = False

    def _ensure_session(self) -> bool:
        if self._session is not None and not self._session._closed:
            return True
        # 退避重连（上限 15s，避免触发网关限流雪崩）
        delay = min(2 ** max(self._retry - 2, 0), 15.0)
        self._retry += 1
        time.sleep(delay)
        try:
            self._speaking = False
            self._session = FunAsrStreamSession(
                self.url,
                mode=self.mode,
                chunk_size=self.chunk_size,
                hotwords=self.hotwords,
                audio_fs=self.audio_fs,
                on_online=self._wrap_activity(self.on_online),
                on_offline=self._wrap_activity(self.on_offline),
                on_error=lambda e: self._status(f"error:{e}"),
                on_close=self._on_close,
            )
            self._session.connect()
            self._retry = 0
            self._status("connected")
            return True
        except Exception as e:
            self._status(f"connect_failed:{e}")
            return False

    def update_hotwords(self, words: dict[str, int]) -> None:
        """更新热词并关闭当前会话（下次 send 自动重连并携带新热词）。"""
        from app.asr.hotwords import to_stream_json

        self.hotwords = to_stream_json(words)
        with self._lock:
            if self._session is not None:
                self._session.close()
                self._session = None
            self._speaking = False

    def send_audio(self, pcm: bytes) -> None:
        with self._lock:
            if not self._ensure_session():
                return
            if not self._speaking:
                self._session.mark_start()
                self._speaking = True
            self._session.send_audio(pcm)

    def mark_end(self) -> None:
        with self._lock:
            if self._session is not None and self._speaking:
                self._session.mark_end()
                self._speaking = False

    def close(self) -> None:
        with self._lock:
            if self._session is not None:
                self._session.close()
                self._session = None
            self._speaking = False
