"""线程安全的事件总线：采集/ASR/LLM 线程发布，FastAPI 的 WebSocket 订阅消费。"""
from __future__ import annotations

import asyncio
import threading
import time
from typing import Any


class EventBus:
    def __init__(self, history_max: int = 800) -> None:
        self._subs: set[asyncio.Queue] = set()
        self._loop: asyncio.AbstractEventLoop | None = None
        self._lock = threading.Lock()
        self._history: list[dict[str, Any]] = []
        self._history_max = history_max

    def bind_loop(self, loop: asyncio.AbstractEventLoop) -> None:
        self._loop = loop

    def subscribe(self) -> asyncio.Queue:
        q: asyncio.Queue = asyncio.Queue()
        with self._lock:
            self._subs.add(q)
        return q

    def unsubscribe(self, q: asyncio.Queue) -> None:
        with self._lock:
            self._subs.discard(q)

    def publish(self, event: dict[str, Any]) -> None:
        event.setdefault("t", time.time())
        with self._lock:
            self._history.append(event)
            if len(self._history) > self._history_max:
                self._history = self._history[-self._history_max:]
            subs = list(self._subs)
        if self._loop is not None:
            for q in subs:
                try:
                    self._loop.call_soon_threadsafe(q.put_nowait, event)
                except RuntimeError:
                    pass  # loop 已关闭

    def snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            return list(self._history)


bus = EventBus()
