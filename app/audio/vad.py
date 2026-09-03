"""基于 webrtcvad 的断句器：能量门 + 语音起止 + 句尾静音 + 超长强切。"""
from __future__ import annotations

import webrtcvad


class VadSegmenter:
    def __init__(
        self,
        sample_rate: int = 16000,
        frame_ms: int = 20,
        silence_ms: int = 600,
        aggressiveness: int = 2,
        max_segment_s: int = 15,
    ) -> None:
        self.vad = webrtcvad.Vad(aggressiveness)
        self.sample_rate = sample_rate
        self.frame_ms = frame_ms
        # 校验：webrtcvad 仅支持 8k/16k/32k/48k 且 10/20/30ms
        assert frame_ms in (10, 20, 30), "frame_ms 仅支持 10/20/30"

        self.silence_thresh = max(1, silence_ms // frame_ms)   # 句尾静音帧数
        self.start_thresh = 3                                   # 起句连续语音帧数
        self.max_frames = int(max_segment_s * 1000 / frame_ms)  # 超长强切

        self.in_speech = False
        self.silent_run = 0
        self.speech_run = 0
        self.seg_frames = 0

    def is_voice(self, frame: bytes) -> bool:
        try:
            return self.vad.is_speech(frame, self.sample_rate)
        except Exception:
            return False  # 静音/异常帧视为非语音

    def process(self, frame: bytes) -> str | None:
        """处理一帧，返回 'start' / 'end' / None。"""
        voice = self.is_voice(frame)
        if not self.in_speech:
            if voice:
                self.speech_run += 1
                if self.speech_run >= self.start_thresh:
                    self.in_speech = True
                    self.silent_run = 0
                    self.seg_frames = self.speech_run
                    return "start"
            else:
                self.speech_run = 0
        else:
            self.seg_frames += 1
            if voice:
                self.silent_run = 0
            else:
                self.silent_run += 1
            if self.silent_run >= self.silence_thresh or self.seg_frames >= self.max_frames:
                self.in_speech = False
                self.silent_run = 0
                self.speech_run = 0
                return "end"
        return None

    def reset(self) -> None:
        self.in_speech = False
        self.silent_run = 0
        self.speech_run = 0
        self.seg_frames = 0
