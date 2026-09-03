"""术语候选提取（分词 + 规则打分，不调用 LLM，零延迟）。

目标：从口语化的 ASR 转写里挑出**值得解释的学术名词**，而不是随意的短句片段。

策略（按可信度排序）：
1. 命中会前热词表 / 科学家专属术语 —— 最可信（这些正是该报告人领域的核心术语）；
2. 英文缩写与专有名词（CRISPR、Transformer、DNA）；
3. 中英混排（T细胞、Q因子）；
4. 中文术语：jieba 分词后，**必须具备学术前后缀特征**（…效应/…机制/…张力、微…/纳米…），
   或由相邻名词组合成复合术语（表面 + 张力 → 表面张力）。

关键点：用分词而非正则贪婪匹配，避免切出 "相当于我""那T""个机制" 这类
无意义片段（这是旧实现的主要缺陷）。
"""
from __future__ import annotations

import re

# 口语/功能词：本身不是术语，也不能作为术语的组成部分
_STOP = {
    "我们", "这个", "那个", "就是", "然后", "一个", "可以", "因为", "所以",
    "如果", "但是", "现在", "大家", "问题", "方法", "研究", "比较", "可能",
    "已经", "还是", "这样", "什么", "怎么", "非常", "关于", "通过", "相当",
    "其实", "当然", "刚才", "下面", "上面", "这些", "那些", "自己", "东西",
    "时候", "地方", "样子", "情况", "内容", "部分", "方面", "结果", "意思",
    "老师", "同学", "报告", "谢谢", "大概", "有点", "只是", "而且", "另外",
    "首先", "其次", "最后", "比如", "例如", "所谓", "一下", "一些", "考虑",
    "觉得", "认为", "看到", "发现", "得到", "出来", "进来", "起来", "下去",
    "工作", "情形", "过程", "阶段", "水平", "程度", "关系", "作用", "影响",
}

# 学术术语后缀（中文术语的强信号）
_ACADEMIC_SUFFIX = (
    "效应", "机制", "机理", "模型", "理论", "定律", "定理", "方程", "公式",
    "张力", "势能", "动能", "能量", "熵", "焓", "梯度", "通量", "速率", "常数",
    "系数", "参数", "变量", "函数", "算法", "网络", "架构", "范式",
    "结构", "构型", "构象", "晶格", "晶体", "分子", "原子", "离子", "电子",
    "细胞", "基因", "蛋白", "酶", "受体", "通道", "信号", "通路", "代谢",
    "谱", "成像", "显微", "衍射", "散射", "共振", "激发", "跃迁",
    "催化", "合成", "反应", "极化", "磁化", "掺杂", "缺陷", "界面",
    "薄膜", "衬底", "器件", "芯片", "传感", "微结构", "纳米结构",
    "接触角", "粘度", "黏度", "浸润", "润湿", "铺展", "毛细", "流量", "湍流",
    "层流", "边界层", "雷诺数", "拓扑", "对称性", "手性", "量子",
    "纠缠", "相干", "隧穿", "自旋", "轨道", "带隙", "载流子", "速度", "密度",
)

# 学术术语前缀
_ACADEMIC_PREFIX = (
    "微", "纳米", "量子", "超导", "半导体", "生物", "分子", "原子",
    "化学", "物理", "神经", "免疫", "基因", "柱状", "毛细", "表面",
    "非线性", "高维", "多尺度", "自组装", "各向异性", "介观", "宏观", "微观",
)

_ABBR = re.compile(r"^[A-Za-z][A-Za-z0-9\-]{1,}$")
_HAS_CN = re.compile(r"[\u4e00-\u9fff]")
_HAS_EN = re.compile(r"[A-Za-z]")
_PURE_CN = re.compile(r"^[\u4e00-\u9fff]+$")

# 英文停用词（避免 the/this/and 被当缩写）
_EN_STOP = {
    "the", "this", "that", "and", "for", "with", "you", "are", "was", "our",
    "can", "not", "but", "all", "one", "two", "its", "has", "have", "will",
    "ok", "okay", "yes", "no", "so", "we", "it", "is", "of", "to", "in",
    "ppt", "ai",
}

_jieba = None
_posseg = None


def _get_posseg():
    """延迟导入 jieba.posseg（首次约 0.5s 建词典），失败返回 None 走降级。"""
    global _posseg
    if _posseg is None:
        try:
            import jieba
            import jieba.posseg as pseg

            jieba.setLogLevel(60)  # 静音
            _posseg = pseg
        except Exception:  # noqa: BLE001
            _posseg = False
    return _posseg or None


# 可作为术语（组成部分）的词性：名词类 / 动名词 / 英文 / 简称 / 未知专名
_NOUN_FLAGS = {"n", "nz", "nt", "ns", "nr", "ng", "nrt", "vn", "an", "eng", "j", "l", "x"}


def _is_academic_cn(w: str) -> bool:
    """判断一个中文词是否具备学术术语特征。"""
    if not (2 <= len(w) <= 10) or not _PURE_CN.match(w):
        return False
    if w in _STOP:
        return False
    # 含口语词作为子串（如"这个结构"）→ 不是干净术语
    if any(s in w for s in _STOP):
        return False
    return w.endswith(_ACADEMIC_SUFFIX) or w.startswith(_ACADEMIC_PREFIX)


def _tagged_tokens(text: str) -> list[tuple[str, str]]:
    """分词 + 词性标注；jieba 不可用时退回按标点切分（词性标为 n）。"""
    pseg = _get_posseg()
    if pseg is not None:
        return [(w.word.strip(), w.flag) for w in pseg.cut(text) if w.word.strip()]
    return [(t, "n") for t in re.split(r"[，。！？；、\s,.!?;]+", text) if t]


def score_text(text: str, glossary: dict[str, int]) -> list[dict]:
    """对一句转写文本提取候选术语并打分，返回 [{term, score}]（降序）。"""
    text = text.strip()
    if not text:
        return []
    cand: dict[str, float] = {}

    # 1) 命中会前/科学家术语表（最高分；表内权重高的更靠前）
    for term, weight in glossary.items():
        if len(term) >= 2 and term in text:
            cand[term] = max(cand.get(term, 0.0), 3.0 + weight / 1000.0)

    tagged = _tagged_tokens(text)

    # 2) 英文缩写 / 3) 中英混排 / 4) 中文学术名词（仅名词性词参与）
    for w, flag in tagged:
        if w in _STOP or len(w) < 2:
            continue
        has_cn, has_en = bool(_HAS_CN.search(w)), bool(_HAS_EN.search(w))
        if has_en and not has_cn:
            if _ABBR.match(w) and w.lower() not in _EN_STOP:
                bonus = 0.4 if w.isupper() else 0.0  # 全大写更像专业缩写
                cand[w] = max(cand.get(w, 0.0), 1.6 + bonus)
        elif has_en and has_cn:
            cand[w] = max(cand.get(w, 0.0), 1.4)
        elif flag in _NOUN_FLAGS and _is_academic_cn(w):
            cand[w] = max(cand.get(w, 0.0), 1.0)

    # 5) 相邻**名词**组合成复合术语（表面+张力 → 表面张力）
    #    要求两部分均为名词性且长度≥2，避免 "的"+"模型"、"观察"+"蛋白" 这类噪声
    for i in range(len(tagged) - 1):
        (a, fa), (b, fb) = tagged[i], tagged[i + 1]
        if fa not in _NOUN_FLAGS or fb not in _NOUN_FLAGS:
            continue
        if len(a) < 2 or len(b) < 2 or a in _STOP or b in _STOP:
            continue
        if not (_PURE_CN.match(a) and _PURE_CN.match(b)):
            continue
        merged = a + b
        if 4 <= len(merged) <= 10 and _is_academic_cn(merged):
            cand[merged] = max(cand.get(merged, 0.0), 1.2)  # 复合术语信息量更大

    # 去掉被更长候选完全包含且分数不更高的短候选
    terms = sorted(cand, key=len, reverse=True)
    for i, long_t in enumerate(terms):
        for short_t in terms[i + 1:]:
            if short_t in long_t and cand.get(short_t, 0.0) <= cand.get(long_t, 0.0):
                cand.pop(short_t, None)

    ranked = sorted(cand.items(), key=lambda kv: (-kv[1], -len(kv[0])))[:10]
    top = ranked[0][1] if ranked else 1.0
    return [{"term": k, "score": round(min(v / max(top, 1.0), 1.0), 2)} for k, v in ranked]
