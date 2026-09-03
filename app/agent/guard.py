"""AI 输出后置校验：字数窗口 / 评价词 / 确认句 / Markdown / 时长。"""
from __future__ import annotations

import re

# 评价类禁用词（命中即拒绝）
EVAL_WORDS = [
    "很有价值", "水平很高", "突破性", "颠覆", "领先于", "不如", "更优秀",
    "权威", "顶尖", "一流", "里程碑", "重大贡献", "解决了难题", "远超",
    "最好", "最强", "第一", "优秀", "卓越",
]

# 确认句关键词
CONFIRM_WORDS = ["请报告人确认", "请提问者和报告人确认", "请报告人判断", "请专家确认", "请报告人确认。"]

_MARKDOWN = re.compile(r"[#*_>`\-]{2,}|\n|```")


def check_eval(text: str) -> tuple[bool, str]:
    for w in EVAL_WORDS:
        if w in text:
            return False, f"含评价性词汇：{w}"
    return True, "ok"


def check_confirm(text: str) -> tuple[bool, str]:
    for w in CONFIRM_WORDS:
        if w in text:
            return True, "ok"
    return False, "缺少确认句"


def check_markdown(text: str) -> tuple[bool, str]:
    if _MARKDOWN.search(text):
        return False, "含 Markdown/换行"
    return True, "ok"


def check_length(text: str, min_chars: int, max_chars: int) -> tuple[bool, str]:
    n = len(text)
    if n < min_chars:
        return False, f"过短({n}<{min_chars})"
    if n > max_chars:
        return False, f"过长({n}>{max_chars})"
    return True, "ok"


def check_duration(text: str, max_secs: float = 30.0, cps: float = 4.5) -> tuple[bool, str]:
    secs = len(text) / cps
    if secs > max_secs:
        return False, f"口播超时({secs:.0f}s>{max_secs:.0f}s)"
    return True, "ok"


def validate(text: str, min_chars: int, max_chars: int) -> dict:
    """返回校验结果 {ok, checks:{...}, text}；过长时截断到最后一个完整句。"""
    checks: dict[str, str] = {}
    ok, msg = check_length(text, min_chars, max_chars)
    checks["len"] = msg
    if not ok and "过长" in msg:
        text = _truncate_sentence(text, max_chars)
        ok, msg = check_length(text, min_chars, max_chars)
        checks["len"] = msg

    ok2, m2 = check_eval(text)
    checks["no_eval"] = m2
    ok3, m3 = check_confirm(text)
    checks["confirm"] = m3
    ok4, m4 = check_markdown(text)
    checks["markdown"] = m4
    ok5, m5 = check_duration(text)
    checks["duration"] = m5

    return {
        "ok": ok and ok2 and ok3 and ok4 and ok5,
        "checks": checks,
        "text": text.strip(),
    }


def _truncate_sentence(text: str, max_chars: int) -> str:
    """截断到最后一个完整句号，仍超则硬截。"""
    head = text[:max_chars]
    for punc in ("。", "？", "！", "；"):
        idx = head.rfind(punc)
        if idx > max_chars * 0.6:
            return head[: idx + 1]
    return head
