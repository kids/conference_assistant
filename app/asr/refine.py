"""转写终稿的 LLM 顺句改写（后台队列，失败静默降级）。

用途：ASR 终稿往往有错别字、缺标点、口语赘词、术语识别偏差。本模块在句子
确认后异步送 LLM 做**保守修正**（不改内容、不加解释），完成后回调更新前端显示。

设计要点：
- 独立后台线程 + 有界队列：不阻塞 ASR 接收线程，队列满则丢弃（保实时性优先）；
- 只改写达到长度门槛的句子，短句（如"好的"）不值得花 LLM 调用；
- 附带会前热词作为参考，帮助纠正专业术语；
- 任何异常都静默跳过，绝不影响原始转写显示。
"""
from __future__ import annotations

import queue
import threading
from collections.abc import Callable

_PROMPT = """你是会议速记校对员。请修正下面这句会议语音识别（ASR）转写文本中的明显错误。

严格要求：
1. 只做修正，不做改写：不得增删内容、不得补充解释、不得改变说话人原意。
2. 修正范围仅限：同音错别字、缺失或错误的标点、明显的识别错词。
3. 删除无意义口语赘词（呃、啊、那个、就是说 等），但保留正常语气。
4. 若参考术语表中的术语被识别成了近音词，请改回术语表中的写法。
5. 直接输出修正后的句子本身，不要输出任何解释、引号或前后缀。
6. 若原句已无明显错误，原样输出。

{hotwords_block}原句：{text}"""


class TranscriptRefiner:
    """后台 LLM 顺句改写器。"""

    def __init__(
        self,
        llm,
        on_refined: Callable[[str, str], None],
        min_chars: int = 12,
        max_queue: int = 8,
        timeout: float = 8.0,
    ) -> None:
        """
        Args:
            llm: OpenAICompatClient 实例（None 则整体禁用）
            on_refined: 回调 (seg_id, refined_text)，仅在文本确有变化时触发
            min_chars: 低于此长度的句子不送改写
            max_queue: 队列上限，满则丢弃新任务（保实时性）
            timeout: 单次 LLM 调用超时
        """
        self.llm = llm
        self.on_refined = on_refined
        self.min_chars = min_chars
        self.timeout = timeout
        self._q: queue.Queue[tuple[str, str, dict]] = queue.Queue(maxsize=max_queue)
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self.dropped = 0
        self.refined_count = 0

    def start(self) -> None:
        if self.llm is None or self._thread is not None:
            return
        self._thread = threading.Thread(target=self._loop, daemon=True, name="refiner")
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()

    def submit(self, seg_id: str, text: str, hotwords: dict[str, int] | None = None) -> None:
        """提交一句待改写文本（非阻塞；队列满或不满足门槛则跳过）。"""
        if self.llm is None or self._thread is None:
            return
        if len(text) < self.min_chars:
            return
        try:
            self._q.put_nowait((seg_id, text, hotwords or {}))
        except queue.Full:
            self.dropped += 1  # 高峰期丢弃，优先保证实时转写

    def _loop(self) -> None:
        while not self._stop.is_set():
            try:
                seg_id, text, hotwords = self._q.get(timeout=0.5)
            except queue.Empty:
                continue
            try:
                refined = self._refine(text, hotwords)
            except Exception:  # noqa: BLE001 —— 改写失败保留原文，不影响会议
                continue
            if refined and refined != text:
                self.refined_count += 1
                try:
                    self.on_refined(seg_id, refined)
                except Exception:  # noqa: BLE001
                    pass

    def _refine(self, text: str, hotwords: dict[str, int]) -> str:
        # 只挑权重高的少量术语作参考，控制 prompt 体积
        block = ""
        if hotwords:
            top = sorted(hotwords.items(), key=lambda kv: -kv[1])[:20]
            block = "参考术语表：" + "、".join(w for w, _ in top) + "\n\n"
        prompt = _PROMPT.format(hotwords_block=block, text=text)
        out = self.llm.chat(
            [{"role": "user", "content": prompt}], max_tokens=200, temperature=0.0
        )
        out = (out or "").strip().strip('"“”')
        # 防御：LLM 跑偏（输出过长/空）时放弃改写
        if not out or len(out) > len(text) * 2 + 20:
            return ""
        return out
