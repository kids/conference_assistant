"""热词解析：复用 tan-asr/hotwords_dict.txt（词<TAB>权重）→ 流式 JSON 字符串。

HTTP 整段接口用空格分隔词，FunASR 流式接口要求 JSON 字符串：
    {"节水抗旱稻":69,"耦合簇方法":50}
"""
from __future__ import annotations

import json
from pathlib import Path


def load_hotwords(path: Path) -> dict[str, int]:
    """解析热词词典，返回 {词: 权重}。"""
    words: dict[str, int] = {}
    if not path.exists():
        return words
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split("\t")
            w = parts[0].strip()
            if not w:
                continue
            weight = 50
            if len(parts) >= 2:
                try:
                    weight = int(float(parts[1]))
                except ValueError:
                    weight = 50
            words[w] = weight
    return words


def to_stream_json(words: dict[str, int]) -> str:
    """转换为 FunASR 流式接口的 hotwords 字符串。"""
    if not words:
        return ""
    return json.dumps(words, ensure_ascii=False)


def load_hotwords_json(path: Path) -> str:
    return to_stream_json(load_hotwords(path))


def save_hotwords(path: Path, words: dict[str, int]) -> None:
    """写热词词典文件（词<TAB>权重 格式，与 hotwords_dict.txt 一致）。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write("# AI 自动生成的热词词典\n")
        f.write("# 格式: 热词<TAB>权重(1-100)\n")
        for w, weight in sorted(words.items(), key=lambda kv: -kv[1]):
            f.write(f"{w}\t{weight}\n")
