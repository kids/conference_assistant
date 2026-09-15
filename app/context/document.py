"""讲稿文档解析与「热词 + 上下文摘要」抽取。

对应 Go 版 go/internal/contextx/document.go。

流程：上传的文件 → 文档解析服务（/parse_doc）→ 纯文本 → 一次 LLM 调用
同时产出「热词」（即时用于 ASR）与「浓缩摘要」（进 AI 上下文的 STATIC 块）。
"""
from __future__ import annotations

import json
import re

import httpx

from app.providers.llm_openai import OpenAICompatClient

# 文档解析服务地址。网关上的 /tencent/read_url 只是它的一层壳 —— 把 {url} 转成
# {file_url} 再转发过来，因此要求文件先有一个服务端可访问的 URL。直连本接口可以
# 收 multipart 上传，省掉「把上传目录暴露出去」这一步。
DEFAULT_DOC_PARSE_URL = "https://ml-serv.ssv.qq.com/parse_doc"

# 讲稿浓缩摘要的目标字数（进 AI 上下文的版本）。
DEFAULT_DOC_DIGEST_CHARS = 800

# 摘要目标字数的上限。上下文组装时 STATIC 块会被截到 2500 字（见 manager.build），
# 再加写入文件时那行 "# 讲稿摘要：xxx" 标题，所以摘要本身不能超过 2400 ——
# 超了会在拼上下文时被静默截断，白丢信息。
#
# 实测（输出固定、只变输入）：上下文从 0 涨到 4309 prompt tokens，单次调用耗时
# 仍是 1.1~1.7s（噪声级）—— 耗时由生成量决定，prefill 很便宜。所以调大摘要是
# 安全的，没必要为了速度牺牲信息量；真正的约束是上面这个截断上限。
MAX_DIGEST_CHARS = 2400

# 送进 LLM 的讲稿上限。超出即截断 —— 一次调用塞太多会拉长 hy3 的思考时间，
# 而实时翻译对延迟敏感。
_DOC_MAX_INPUT_CHARS = 20000

# 上传体积上限，用于提前拦截。
#
# 真实上限来自 file 服务（ssv-ml-serv/services/file/fread_rs）的 axum
# DefaultBodyLimit，已从 axum 默认的 2 MiB 调到 256 MiB。这里取同一量级 ——
# 该服务会把整个文件读进内存（field.bytes()），完全放开有 OOM 风险。
#
# 踩坑记录（2026-09，以免重蹈）：当时 >2MB 的上传报
#   "multipart bytes error: Error parsing `multipart/form-data` request"
# 曾误判为网关 client_max_body_size，但实测网关侧 JSON 32MB 都能完整到达、
# 且 Kong 配置里 client_max_body_size=0（不限制）—— 真正原因是 axum 的
# Multipart 提取器读 DefaultBodyLimit（默认 2MB），而服务里只设了
# RequestBodyLimitLayer，对 Multipart 不生效。已在 file 服务修复。
DEFAULT_MAX_UPLOAD_BYTES = 256 << 20

_PARSE_TIMEOUT = 120.0
_LLM_TIMEOUT = 120.0


class DocParseError(RuntimeError):
    """文档解析失败。"""


def check_upload_size(n: int, max_bytes: int = DEFAULT_MAX_UPLOAD_BYTES) -> None:
    """体积预检。超出上限时抛 DocParseError，附可执行建议。"""
    limit = max_bytes if max_bytes > 0 else DEFAULT_MAX_UPLOAD_BYTES
    if n <= limit:
        return
    raise DocParseError(
        f"文件 {n / 1048576:.1f} MB 超过 {limit / 1048576:.1f} MB 上传上限"
        "（file 服务的 DefaultBodyLimit 限制，超出会被截断，服务端只会报 multipart 解析失败）。"
        "建议：先用 pdftotext / soffice 等把文档转成纯文本再上传（文字层通常只有几十 KB），"
        "或调大 file 服务的 DefaultBodyLimit 并把 DOC_MAX_BYTES 同步放开"
    )


def parse_document(
    parse_url: str, filename: str, data: bytes, timeout: float = _PARSE_TIMEOUT
) -> dict:
    """把文件交给文档解析服务，返回 {filename, content, truncated}。

    服务端契约（services/tencentapi：readURLHandler → parseDocByJSON → /parse_doc）：

      请求  POST multipart/form-data，字段名 file
      响应  [{"content":"正文","filename":"文件名"}]
    """
    url = (parse_url or "").strip() or DEFAULT_DOC_PARSE_URL
    try:
        resp = httpx.post(url, files={"file": (filename, data)}, timeout=timeout)
    except httpx.HTTPError as e:
        raise DocParseError(f"文档解析服务不可达: {e}") from e
    if resp.status_code >= 300:
        raise DocParseError(f"文档解析服务返回 {resp.status_code}: {resp.text[:200]}")

    try:
        rows = resp.json()
    except ValueError as e:
        raise DocParseError(f"解析结果不是 JSON: {resp.text[:200]}") from e
    if isinstance(rows, dict):
        rows = [rows]
    if not isinstance(rows, list) or not rows:
        raise DocParseError(f"文档解析服务未返回内容: {resp.text[:200]}")

    parts: list[str] = []
    name = filename
    for r in rows:
        if not isinstance(r, dict):
            continue
        content = str(r.get("content") or "").strip()
        if content:
            parts.append(content)
        if r.get("filename"):
            name = str(r["filename"])

    text = "\n\n".join(parts).strip()
    if not text:
        raise DocParseError("解析结果为空（该格式可能不支持，或文件无文字层）")

    truncated = len(text) > _DOC_MAX_INPUT_CHARS
    return {
        "filename": name,
        "content": text[:_DOC_MAX_INPUT_CHARS],
        "truncated": truncated,
    }


_DOC_PROMPT = """你是学术会议支持助手。下面是一份报告讲稿/演示稿的解析文本（可能来自 PPT、PDF 或文档）。

请完成三件事，并严格输出一个 JSON 对象（不要输出任何其他文字、解释或 Markdown 代码块标记）：

{"digest":"...","fields":["方向1","方向2"],"hotwords":[{"word":"术语","weight":60}]}

要求：

1. digest —— {digest_chars} 字以内的浓缩摘要，用于实时翻译时提供背景。
   - 必须保留：研究领域、核心术语、关键方法、主要结论、重要缩写、涉及的人名机构。
   - 必须去掉：致谢、目录、页眉页脚、参考文献列表、重复内容、版式残留字符。
   - 直接给内容，不要写「本文介绍了…」「该报告讨论了…」这类空话。
   - 幻灯片文本是碎片化的，请按语义重组为连贯段落。

2. hotwords —— 讲稿中出现的核心专业术语、方法名、模型名、算法名、缩写、专有名词，10~25 个。
   - weight 为 1~100 的整数：核心术语 60~100，一般术语 30~59。
   - 术语要具体、有辨识度，避免「研究」「方法」「问题」这类泛词。
   - 优先中文学术术语；英文缩写保留原文（如 CNN、CRISPR、Transformer）。

3. fields —— 讲稿涉及的学科方向，1~5 个。

讲稿文本：
---
{doc}
---"""


def build_from_document(
    llm: OpenAICompatClient,
    doc_text: str,
    digest_chars: int = DEFAULT_DOC_DIGEST_CHARS,
) -> dict:
    """一次 LLM 调用同时产出浓缩摘要与热词。

    为什么不直接把解析原文塞进上下文：幻灯片解析出来是碎片化的，信噪比低，
    而且每次 AI 调用都要重新读一遍。这里在上传时一次性浓缩，之后每次调用只带
    摘要，既省 token 又更聚焦。
    """
    target = digest_chars if digest_chars > 0 else DEFAULT_DOC_DIGEST_CHARS
    target = min(target, MAX_DIGEST_CHARS)
    prompt = _DOC_PROMPT.replace("{digest_chars}", str(target)).replace("{doc}", doc_text)

    # 预算与超时对齐 build_speaker_profile：hy3 是思考模型，reasoning 与正文共享
    # max_tokens，给少了会正文为空；思考时长随 prompt 波动，按调用给 120s。
    # 实测长英文文献：reasoning 可占 4200+ token，故预算给到 8000 留足正文空间。
    raw = llm.chat(
        [{"role": "user", "content": prompt}],
        max_tokens=8000, temperature=0.2, timeout=_LLM_TIMEOUT,
    )
    data = _parse_json(raw)

    hotwords: dict[str, int] = {}
    for item in data.get("hotwords", []):
        if not isinstance(item, dict):
            continue
        word = str(item.get("word", "")).strip()
        if not word:
            continue
        try:
            weight = int(item.get("weight", 50))
        except (TypeError, ValueError):
            weight = 50
        hotwords[word] = max(1, min(100, weight))

    digest = str(data.get("digest", "")).strip()
    # 空结果必须报错，不能当成「成功但 0 个热词」静默放过。
    # 实测过一次偶发：LLM 返回空/非 JSON 正文，调用方拿到空 profile 却显示成功，
    # 页面上表现为「抽了 0 个热词」，极难排查。
    if not digest and not hotwords:
        raise DocParseError(
            f"LLM 未返回有效内容（原始输出 {len(raw)} 字）: {raw[:200]}"
        )
    return {
        "digest": digest,
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
