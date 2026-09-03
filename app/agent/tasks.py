"""5 个翻译任务的定义：系统提示词、任务指令、输出模板、字数窗口。"""
from __future__ import annotations

from dataclasses import dataclass

SYSTEM_PROMPT = """你是"AI 跨学科翻译席"，服务于高水平跨学科学术会议（Workshop）。
你的职责是：把某一学科的专业表达，实时转换为其他学科也能理解、能提问、能连接的科学问题。

铁律（违反即失败）：
- 只做翻译/解释/转译/桥接，绝不评价研究质量、不比较学术贡献、不给出未经确认的结论。
- 不编造数据、不夸大意义；不确定的地方明确说"这一点需要报告人确认"。
- 每次只做主持人指定的一件事，输出纯口播文本：无 Markdown、无列表、无编号、无括注。
- 输出必须自然、通俗、口语化，适合主持人现场朗读（约 20-30 秒）。
- 每次输出必须以请求报告人/提问者确认的句子结尾。"""


@dataclass(frozen=True)
class Task:
    key: str
    label: str
    instruction: str
    min_chars: int
    max_chars: int
    target_hint: str = ""  # 需要目标词/句时的提示


TASKS: dict[str, Task] = {
    "TERM": Task(
        key="TERM",
        label="术语翻译",
        instruction=(
            "请用约 20 秒解释主持人指定的这个术语，只做解释，不做评价。"
            "模板：这个术语可以理解为……；它在本报告中重要，是因为……。"
            "结尾：这个解释请报告人确认。"
        ),
        min_chars=60,
        max_chars=120,
        target_hint="需要解释的目标术语",
    ),
    "Q_TRANSLATE": Task(
        key="Q_TRANSLATE",
        label="问题转译",
        instruction=(
            "请把刚才听众提出的这个专业问题，转译成其他学科也能理解的科学问题。"
            "模板：刚才的问题更一般地说是在问……；它可能与……领域也有关。"
            "结尾：这个转译请提问者和报告人确认。"
        ),
        min_chars=60,
        max_chars=120,
        target_hint="需要转译的问题",
    ),
    "A_TRANSLATE": Task(
        key="A_TRANSLATE",
        label="回答转译",
        instruction=(
            "请把报告人刚才的回答，转述成非本领域听众容易理解的通俗版本。"
            "保留原意与关键术语，去除过于专业的表达。"
            "结尾：这个转述请报告人确认。"
        ),
        min_chars=60,
        max_chars=120,
        target_hint="需要转译的回答",
    ),
    "BRIDGE": Task(
        key="BRIDGE",
        label="桥接提问",
        instruction=(
            "请提出一个跨学科桥接问题，把当前讨论引向与其他领域的连接，不评价研究质量。"
            "模板：一个可以追问的问题是……；这个问题可能连接……和……。"
            "结尾：这个连接是否成立，请报告人判断。"
        ),
        min_chars=50,
        max_chars=110,
    ),
    "SUMMARY": Task(
        key="SUMMARY",
        label="报告总结",
        instruction=(
            "请用约 30 秒对刚才的报告做一个通俗易懂的总结，突出关键问题、核心概念与学术意义。"
            "结尾：以上总结请报告人确认。"
        ),
        min_chars=90,
        max_chars=150,
    ),
}


def estimate_secs(text: str) -> float:
    """中文口播时长估算：约 4.5 字/秒。"""
    return max(1.0, len(text) / 4.5)
