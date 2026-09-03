"""内存 PCM 环形缓冲：保留最近 N 秒原始音频，用于"刚才那个术语"回溯重识别。"""
from __future__ import annotations

import threading


class RingBuffer:
    def __init__(self, seconds: float = 120.0, sample_rate: int = 16000) -> None:
        # 16bit mono 每秒 = sample_rate * 2 字节
        self._cap = int(seconds * sample_rate * 2)
        self._buf = bytearray()
        self._lock = threading.Lock()

    def push(self, data: bytes) -> None:
        with self._lock:
            self._buf.extend(data)
            if len(self._buf) > self._cap:
                del self._buf[: len(self._buf) - self._cap]

    def tail(self, seconds: float) -> bytes:
        n = int(seconds * 16000 * 2)
        with self._lock:
            if n >= len(self._buf):
                return bytes(self._buf)
            return bytes(self._buf[len(self._buf) - n:])

    def clear(self) -> None:
        with self._lock:
            self._buf.clear()
