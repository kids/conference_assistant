"""现场音频采集（sounddevice）、远端浏览器收音（WebSocket 推流）与本地文件回放（离线彩排）。"""
from __future__ import annotations

import queue
import threading
import time

import numpy as np


class MicrophoneCapture:
    """从声卡/调音台/USB 麦采集，输出 16kHz/mono/int16 的 20ms PCM 帧。"""

    def __init__(
        self,
        device: int | None = None,
        sample_rate: int = 16000,
        block_ms: int = 20,
        channels: int = 1,
    ) -> None:
        import sounddevice as sd  # 延迟导入，避免未安装时影响其他模块

        self.sd = sd
        self.sample_rate = sample_rate
        self.block = sample_rate * block_ms // 1000
        self.channels = channels
        self.device = device
        self._q: queue.Queue[bytes] = queue.Queue()
        self._stream = None
        self._closed = threading.Event()

    def _callback(self, indata, frames, time_info, status) -> None:  # noqa: ANN001
        mono = indata[:, 0]
        pcm = (np.clip(mono, -1.0, 1.0) * 32767).astype(np.int16).tobytes()
        self._q.put(pcm)

    def start(self) -> None:
        self._stream = self.sd.InputStream(
            samplerate=self.sample_rate,
            device=self.device,
            channels=self.channels,
            dtype="float32",
            blocksize=self.block,
            callback=self._callback,
        )
        self._stream.start()

    def read(self, timeout: float = 1.0) -> bytes | None:
        try:
            return self._q.get(timeout=timeout)
        except queue.Empty:
            return None

    def stop(self) -> None:
        self._closed.set()
        if self._stream is not None:
            try:
                self._stream.stop()
                self._stream.close()
            except Exception:
                pass
            self._stream = None


class BrowserCapture:
    """远端浏览器收音：前端 getUserMedia 采集 PCM，经 /ws/audio 推入本类队列。

    与 MicrophoneCapture 同样的 read 接口，输出固定 block_ms（20ms）帧；
    浏览器推来的音频可以是任意块大小，内部重切（webrtcvad 只接受 10/20/30ms 帧）。
    keep_alive=True：流水线无限等待浏览器连接，不因无帧自动结束（远端打开服务场景）。
    """

    keep_alive = True

    def __init__(self, sample_rate: int = 16000, block_ms: int = 20) -> None:
        self.sample_rate = sample_rate
        # 每帧字节数：int16 单声道（如 20ms @16k = 320 采样 = 640 字节）
        self.block_bytes = sample_rate * block_ms // 1000 * 2
        self._q: queue.Queue[bytes] = queue.Queue()
        self._buf = bytearray()
        self.connected = threading.Event()
        self._closed = threading.Event()

    def feed(self, pcm: bytes) -> None:
        """供 /ws/audio 端点调用：追加任意大小 PCM 块，重切成固定帧入队。"""
        if not pcm or self._closed.is_set():
            return
        self._buf.extend(pcm)
        n = self.block_bytes
        while len(self._buf) >= n:
            self._q.put(bytes(self._buf[:n]))
            del self._buf[:n]

    def mark_connected(self) -> None:
        self.connected.set()

    def mark_disconnected(self) -> None:
        self.connected.clear()

    def read(self, timeout: float = 1.0) -> bytes | None:
        try:
            return self._q.get(timeout=timeout)
        except queue.Empty:
            return None

    def stop(self) -> None:
        self._closed.set()
        self.connected.clear()


class FileReplayCapture:
    """按真实时间轴回放本地 WAV/RAW PCM，用于离线彩排（阶段 0）。"""

    def __init__(self, path: str, sample_rate: int = 16000, block_ms: int = 20,
                 tail_silence_s: float = 3.0) -> None:
        self.path = path
        self.sample_rate = sample_rate
        self.block = sample_rate * block_ms // 1000
        self._raw = self._load()
        self._pos = 0
        self._start = time.monotonic()
        # 尾静音：音频结束后继续发静音帧，让服务端 VAD 检测句子端点
        self._tail_silence = np.zeros(int(sample_rate * tail_silence_s), dtype=np.int16).tobytes()

    def _load(self) -> bytes:
        p = self.path.lower()
        if p.endswith(".wav"):
            import wave

            with wave.open(self.path, "rb") as w:
                if w.getframerate() != self.sample_rate:
                    raise ValueError(f"回放文件采样率需为 {self.sample_rate}")
                return w.readframes(w.getnframes())
        # 视为 RAW PCM int16 mono
        with open(self.path, "rb") as f:
            return f.read()

    def read(self, timeout: float = 1.0) -> bytes | None:
        # 按真实时间节流：回放进度不得快于真实时间，否则会话行为失真、易触发发送超时
        elapsed = time.monotonic() - self._start
        played = self._pos / 2 / self.sample_rate  # 已回放秒数
        if played > elapsed:
            time.sleep(min(played - elapsed, 0.1))
        step = self.block * 2  # bytes
        if self._pos < len(self._raw):
            chunk = self._raw[self._pos:self._pos + step]
            self._pos += step
            return chunk
        # 音频播完，先吐尾静音帧
        if self._tail_silence:
            chunk = self._tail_silence[:step]
            self._tail_silence = self._tail_silence[step:]
            time.sleep(self.block / self.sample_rate)
            return chunk
        return None
