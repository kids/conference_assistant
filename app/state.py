"""AI 展示状态机 + 全局 Kill Switch。

状态流转（本版无 TTS，AI 输出仅投屏展示）：
    IDLE → GENERATING → PENDING_APPROVAL → SHOWING → IDLE
"""
from __future__ import annotations

import threading
from collections.abc import Callable
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
        # 当前在飞生成的中止回调。
        # 必需：hy3 思考阶段只流 reasoning_content，正文增量一个都没有，
        # 靠"每个增量查一次急停标志"在思考期间完全失效 —— 实测按下急停后
        # 仍要等 10~30s 思考结束才真正中止。用回调中断请求才能真正立即生效。
        self._cancel_gen: Callable[[], None] | None = None

    def set_cancel(self, fn: Callable[[], None] | None) -> None:
        """注册当前生成的中止回调；传 None 表示生成已结束。"""
        with self._lock:
            self._cancel_gen = fn

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

    def clear(self) -> None:
        """只清展示状态（state/current/task → IDLE），**不动急停标志**。

        用于急停/丢弃：既要把卡片从大屏撤下、让 state/current 不再指向那张卡，
        又必须保留急停标志 —— 若在这里把它清掉，正在飞的那次生成就永远不会中止，
        急停的主功能反而失效。
        """
        with self._lock:
            self._state = AIState.IDLE
            self._current = None
            self._task = None

    def reset(self) -> None:
        with self._lock:
            self._kill.clear()
            self._state = AIState.IDLE
            self._current = None
            self._task = None

    def kill(self) -> None:
        """立即中止生成 / 下屏。幂等。

        除了置标志，还会直接中断在飞的 LLM 请求 —— 只置标志的话，
        思考阶段没有正文增量可查，要等思考结束才生效。
        """
        self._kill.set()
        with self._lock:
            fn = self._cancel_gen
        if fn is not None:
            try:
                fn()
            except Exception:  # noqa: BLE001 —— 中断失败不影响急停语义
                pass

    @property
    def killed(self) -> bool:
        return self._kill.is_set()


display = DisplayStateMachine()
