"""根据科学家姓名+机构，用 LLM 生成研究背景与相关热词。

输入：科学家姓名、机构、学科（可选）
输出：{profile: 研究背景简介, fields: 研究领域列表, hotwords: {词: 权重}}
"""
from __future__ import annotations

import json
import re

from app.providers.llm_openai import OpenAICompatClient

_BUILD_PROMPT = """你是学术会议支持助手。请根据以下科学家信息，基于你的知识库，整理其研究背景并提取相关热词。

科学家姓名：{name}
机构：{institution}
学科领域：{discipline}

请严格输出一个 JSON 对象（不要输出任何其他文字、解释或 Markdown 代码块标记），格式如下：
{{"profile":"150字以内的研究背景简介，概括其研究领域、核心方向与代表工作","fields":["研究领域1","研究领域2"],"hotwords":[{{"word":"专业术语或方法名","weight":60}}]}}

要求：
1. hotwords 为这位科学家研究领域的核心专业术语、方法名、模型名、算法名、缩写、专有名词等，10~25 个。
2. weight 为 1~100 的整数权重：核心术语 60~100，一般术语 30~59。
3. 术语要具体、有辨识度，避免"研究""方法""问题"这类泛词。
4. 优先中文学术术语；英文缩写保留原文（如 CNN、CRISPR、Transformer）。
5. 术语应覆盖：研究对象/材料/基因/化合物、技术方法、关键概念、重要缩写。"""


def build_speaker_profile(
    llm: OpenAICompatClient,
    name: str,
    institution: str = "",
    discipline: str = "",
) -> dict:
    """调用 LLM 生成研究背景与热词。失败抛异常，由调用方降级处理。"""
    prompt = _BUILD_PROMPT.format(
        name=name, institution=institution or "未知", discipline=discipline or "未知"
    )
    # max_tokens 与 timeout 都必须给足：hy3 的 reasoning 与正文共享 max_tokens
    # （实测 2000 会被思考吃光、正文 0 字），且思考时长随 prompt 波动很大
    # （同一 prompt 实测 28~37s）。这里按调用给 90s，而不是依赖默认的 30s。
    raw = llm.chat(
        [{"role": "user", "content": prompt}],
        max_tokens=6000, temperature=0.2, timeout=90.0,
    )
    data = _parse_json(raw)

    hotwords: dict[str, int] = {}
    for item in data.get("hotwords", []):
        if not isinstance(item, dict):
            continue
        w = str(item.get("word", "")).strip()
        if not w:
            continue
        try:
            weight = int(item.get("weight", 50))
        except (TypeError, ValueError):
            weight = 50
        hotwords[w] = max(1, min(100, weight))

    profile = str(data.get("profile", "")).strip()
    # 空结果必须报错：LLM 偶发返回空/非 JSON 正文时，静默返回空 profile 会让
    # 调用方显示「成功但 0 个热词」，极难排查。
    if not profile and not hotwords:
        raise RuntimeError(f"LLM 未返回有效内容（原始输出 {len(raw)} 字）: {raw[:200]}")
    return {
        "profile": profile,
        "fields": [str(f).strip() for f in data.get("fields", []) if str(f).strip()],
        "hotwords": hotwords,
    }


def _parse_json(raw: str) -> dict:
    """容错解析 LLM 输出：剥掉 ```json 包裹，或提取首个 {...}。"""
    raw = raw.strip()
    m = re.search(r"```(?:json)?\s*(.*?)```", raw, re.DOTALL)
    if m:
        raw = m.group(1).strip()
    try:
        obj = json.loads(raw)
        return obj if isinstance(obj, dict) else {}
    except json.JSONDecodeError:
        pass
    m = re.search(r"\{.*\}", raw, re.DOTALL)
    if m:
        try:
            obj = json.loads(m.group(0))
            return obj if isinstance(obj, dict) else {}
        except json.JSONDecodeError:
            pass
    return {}
