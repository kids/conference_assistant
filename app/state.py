"""AI 展示状态机 + 全局 Kill Switch。

状态流转（本版无 TTS，AI 输出仅投屏展示）：
    IDLE → GENERATING → PENDING_APPROVAL → SHOWING → IDLE
"""
from __future__ import annotations

import threading
from enum import Enum


class AIState(str, Enum):
    IDLE = "IDLE"
    GENERATING = "GENERATING"
    PENDING_APPROVAL = "PENDING_APPROVAL"
    SHOWING = "SHOWING"


class DisplayStateMachine:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._state: AIState = AIState.IDLE
        self._kill = threading.Event()
        self._current: str | None = None  # invocation_id
        self._task: str | None = None

    @property
    def state(self) -> AIState:
        with self._lock:
            return self._state

    @property
    def current(self) -> str | None:
        with self._lock:
            return self._current

    @property
    def task(self) -> str | None:
        with self._lock:
            return self._task

    def begin_generate(self, invocation_id: str, task: str) -> None:
        with self._lock:
            self._kill.clear()
            self._state = AIState.GENERATING
            self._current = invocation_id
            self._task = task

    def ready(self) -> None:
        with self._lock:
            self._state = AIState.PENDING_APPROVAL

    def show(self) -> None:
        with self._lock:
            self._state = AIState.SHOWING

    def reset(self) -> None:
        with self._lock:
            self._kill.clear()
            self._state = AIState.IDLE
            self._current = None
            self._task = None

    def kill(self) -> None:
        """立即中止生成 / 下屏。幂等。"""
        self._kill.set()

    @property
    def killed(self) -> bool:
        return self._kill.is_set()


display = DisplayStateMachine()
