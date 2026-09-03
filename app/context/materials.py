"""会前资料加载：摘要 / PPT 文本 / 简介 / 术语表 → 纯文本（STATIC 上下文）。"""
from __future__ import annotations

import json
from pathlib import Path


def load_materials(materials_dir: Path, max_chars: int = 4000) -> str:
    """读取 materials 目录下所有可解析文件，拼接为纯文本。"""
    if not materials_dir.exists():
        return ""
    chunks: list[str] = []
    for f in sorted(materials_dir.iterdir()):
        if not f.is_file():
            continue
        try:
            text = _load_file(f)
        except Exception:
            continue
        if text:
            chunks.append(f"[{f.stem}]\n{text.strip()}")
        if sum(len(c) for c in chunks) >= max_chars:
            break
    joined = "\n\n".join(chunks)
    return joined[:max_chars]


def _load_file(f: Path) -> str:
    suffix = f.suffix.lower()
    if suffix in (".md", ".txt", ".text"):
        return f.read_text(encoding="utf-8", errors="ignore")
    if suffix == ".json":
        data = json.loads(f.read_text(encoding="utf-8", errors="ignore"))
        return json.dumps(data, ensure_ascii=False, indent=1)
    # PDF / PPTX 等：MVP 阶段给出提示（可选安装依赖后启用）
    return ""
