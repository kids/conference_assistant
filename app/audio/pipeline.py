"""音频主流水线线程：采集 → VAD 断句 → FunASR 流式 → 事件总线。"""
from __future__ import annotations

import collections
import threading
import time

from app.audio.capture import FileReplayCapture, MicrophoneCapture
from app.audio.ringbuffer import RingBuffer
from app.audio.vad import VadSegmenter
from app.asr.funasr_ws import FunAsrStreamClient
from app.events import bus
from app.state import display


class AudioPipeline(threading.Thread):
    def __init__(
        self,
        capture,
        vad: VadSegmenter | None,
        asr,
        ring: RingBuffer,
        pre_roll_ms: int = 300,
        server_vad: bool = False,
    ) -> None:
        super().__init__(daemon=True, name="audio-pipeline")
        self.capture = capture
        self.vad = vad
        self.asr = asr
        self.ring = ring
        self.server_vad = server_vad  # True：服务端自动断句（Fun-ASR-Nano vLLM），持续透传
        # pre-roll：保留起句前 300ms 的帧，避免吞掉句首
        self._pre_roll = collections.deque(maxlen=pre_roll_ms // 20)
        self._stop = threading.Event()

        self._seg_id = 0
        self._t_start = 0.0
        self._in_speech = False

    def stop(self) -> None:
        self._stop.set()

    def _now(self) -> float:
        return time.time()

    def run(self) -> None:
        empty_run = 0
        try:
            while not self._stop.is_set():
                frame = self.capture.read(timeout=0.5)
                if self._stop.is_set():
                    break
                if frame is None:
                    # 浏览器收音：等待远端浏览器连接期间无限等待，不因无帧自动结束
                    if getattr(self.capture, "keep_alive", False):
                        continue
                    # 回放结束等无帧输入时，空转超过 15 秒自动结束流水线，
                    # 避免 ASR 长连接悬挂到服务端超时（health 卡在 error）
                    empty_run += 1
                    if empty_run > 30:
                        break
                    continue
                empty_run = 0
                self.ring.push(frame)

                if self.server_vad:
                    # 服务端自动断句：持续透传（含静音帧）
                    self.asr.send_audio(frame)
                    continue

                self._pre_roll.append(frame)
                ev = self.vad.process(frame)
                if ev == "start":
                    self._begin_segment()
                elif self._in_speech:
                    self.asr.send_audio(frame)
                    if ev == "end":
                        self._end_segment()
        finally:
            self.asr.close()
            self.capture.stop()

    def _begin_segment(self) -> None:
        self._in_speech = True
        self._seg_id += 1
        self._t_start = self._now()
        # 回放 pre-roll（含触发 start 的那几帧）
        for f in list(self._pre_roll):
            self.asr.send_audio(f)
        self._pre_roll.clear()

    def _end_segment(self) -> None:
        self._in_speech = False
        self.asr.mark_end()
