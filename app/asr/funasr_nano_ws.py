"""Fun-ASR-Nano vLLM 实时流式客户端（协议：START/STOP 文本命令 + PCM 二进制帧）。

服务：Fun-ASR-Nano vLLM Server（asr.example.com，Kong 网关 /s2/ 前缀 → 内部 /ws）。
协议（对齐 FunASR 官方 serve_realtime_ws.py / client_python.py）：

    客户端 → 服务端：
        "START"                    文本  初始化会话
        "LANGUAGE:中文"            文本  设置语种（可选）
        "HOTWORDS:词1,词2"         文本  设置热词（可选，逗号分隔）
        [binary PCM]               二进制 16k/mono/int16 音频帧
        "STOP"                     文本  结束并 flush 最终结果

    服务端 → 客户端：
        {"event":"started"}
        {"event":"language_set","language":"中文"}
        {"event":"hotwords_set","hotwords":["词1","词2"]}
        {"sentences":[{text,start,end,spk}...],"partial":"...","is_final":false}  中间结果
        {"sentences":[...],"partial":"","is_final":true}                           最终结果
        {"event":"stopped"}

关键特性：**服务端自带动态 VAD 断句**，客户端只需持续透传 PCM（含静音），
服务端自动确认句子（累积到 sentences）并返回临时 partial。
"""
from __future__ import annotations

import json
import threading
from collections.abc import Callable

import websocket  # websocket-client

OnText = Callable[[str], None]
OnError = Callable[[str], None]


class FunAsrNanoStreamClient:
    """Fun-ASR-Nano vLLM 流式客户端。接口与 FunAsrStreamClient 保持一致。"""

    def __init__(
        self,
        url: str,
        language: str = "中文",
        hotwords: list[str] | None = None,
        on_online: OnText | None = None,
        on_offline: OnText | None = None,
        on_status: Callable[[str], None] | None = None,
        on_revise: Callable[[int, str], None] | None = None,
    ) -> None:
        self.url = url
        self.language = language
        self.hotwords = hotwords or []
        self.on_online = on_online
        self.on_offline = on_offline
        self.on_status = on_status
        # 服务端修正已确认句时回调：(本轮句子序号, 新文本)
        self.on_revise = on_revise

        self._ws: websocket.WebSocket | None = None
        self._recv_thread: threading.Thread | None = None
        self._lock = threading.Lock()
        self._closed = False
        self._started = False
        self._sent_count = 0  # 已处理的 confirmed 句子数（用于去重累积 sentences）
        self._sent_texts: list[str] = []  # 已发出的句子文本（用于检测服务端回退修正）
        self._round = 0       # START..STOP 会话轮次（本地 VAD 切句下每句一轮）
        self.last_sentence_key = ""  # 最近回调对应的句子键 r{轮次}i{句内序号}
        self._retry = 0
        self.status = "idle"          # 归一化状态：idle/connected/connecting/error/closed
        self.status_detail = ""
        self.last_active = 0.0        # 最近一次收到服务端消息的时间戳

    # ---- 连接 ----
    def status_info(self) -> dict:
        """归一化状态信息，供 /api/health 与前端展示。"""
        import time

        idle = 0.0
        if self.last_active > 0:
            idle = time.time() - self.last_active
        return {
            "state": self.status,
            "detail": self.status_detail,
            "idle_sec": round(idle, 1),
        }

    # 切句产生的会话事件属噪声，不向前端广播
    _QUIET_EVENTS = ("event:started", "event:stopped", "event:language_set", "event:hotwords_set")

    def _status(self, s: str) -> None:
        # 归一化：把原始状态串映射为稳定状态机
        if s == "connected":
            self.status, self.status_detail = "connected", ""
        elif s.startswith("connect_failed:"):
            self.status, self.status_detail = "connecting", s[len("connect_failed:"):]
        elif s.startswith("error:"):
            self.status, self.status_detail = "error", s[len("error:"):]
        elif s.startswith("event:"):
            self.status, self.status_detail = "connected", s
        elif s == "closed":
            self.status, self.status_detail = "closed", ""
        else:
            self.status = s
        # 切句造成的 started/stopped 等事件不广播，避免状态栏刷屏
        if self.on_status and s not in self._QUIET_EVENTS:
            self.on_status(s)

    def _send_session_init(self, ws: websocket.WebSocket) -> None:
        """发送 START 及语种/热词配置，开启一轮识别会话。"""
        ws.send("START")
        if self.language:
            ws.send(f"LANGUAGE:{self.language}")
        if self.hotwords:
            ws.send("HOTWORDS:" + ",".join(self.hotwords))
        self._started = True
        self._sent_count = 0
        self._sent_texts = []
        self._round += 1

    def _ensure_connected(self) -> bool:
        if self._ws is not None and not self._closed:
            return True
        # 自适应退避：连续失败越多退避越长（上限 15s），避免触发网关限流雪崩
        delay = min(2 ** max(self._retry - 2, 0), 15.0)
        self._retry += 1
        if delay > 0.5:
            import time
            time.sleep(delay)
        try:
            ws = websocket.create_connection(self.url, timeout=10)
            self._ws = ws
            self._closed = False
            self._send_session_init(ws)
            self._retry = 0
            self._status("connected")
            self._recv_thread = threading.Thread(target=self._recv_loop, daemon=True)
            self._recv_thread.start()
            return True
        except Exception as e:  # noqa: BLE001
            self._status(f"connect_failed:{e}")
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
                # 上一句已 STOP，本句开始前复用连接重开会话（无需重连，开销极低）
                if not self._started:
                    self._send_session_init(self._ws)
                self._ws.send_binary(pcm)
            except Exception as e:  # noqa: BLE001
                self._closed = True
                self._status(f"error:{e}")

    def mark_end(self) -> None:
        """本地 VAD 判定句尾：发 STOP 让服务端立即 flush 出终稿（约 0.2s）。

        不阻塞等待结果（结果由接收线程异步回调）；下次 send_audio 会自动重开会话。
        """
        with self._lock:
            if self._ws is None or self._closed or not self._started:
                return
            try:
                self._ws.send("STOP")
                self._started = False
            except Exception as e:  # noqa: BLE001
                self._closed = True
                self._status(f"error:{e}")

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
        import time

        self.last_active = time.time()  # 收到服务端消息即视为活跃
        if "event" in data:
            self._status(f"event:{data['event']}")
            return

        sentences = data.get("sentences", [])
        partial = data.get("partial", "")
        # 本轮已处理过的句子若文本发生变化 → 服务端回退修正（同一句更新）
        if self.on_revise and sentences:
            upper = min(self._sent_count, len(sentences), len(self._sent_texts))
            for i in range(upper):
                new_text = (sentences[i].get("text", "") or "").strip()
                if new_text and new_text != self._sent_texts[i]:
                    self._sent_texts[i] = new_text
                    self.last_sentence_key = f"r{self._round}i{i}"
                    self.on_revise(self.last_sentence_key, new_text)

        # 本轮新增的确认句 → 新句子
        if len(sentences) > self._sent_count:
            for i in range(self._sent_count, len(sentences)):
                text = (sentences[i].get("text", "") or "").strip()
                self._sent_texts.append(text)
                if text and self.on_offline:
                    self.last_sentence_key = f"r{self._round}i{i}"
                    self.on_offline(text)
            self._sent_count = len(sentences)

        # partial 临时文本 → 实时字幕草稿
        if self.on_online:
            self.on_online(partial)

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
                ws.send("STOP")
            except Exception:
                pass
            try:
                ws.close()
            except Exception:
                pass
