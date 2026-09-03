"""上下文组装：STATIC / RECENT / FOCUS / HISTORY 四块 + 预算控制。"""
from __future__ import annotations

from pathlib import Path

from app.context.store import Store


class ContextManager:
    def __init__(self, store: Store, materials_dir: Path | None = None) -> None:
        self.store = store
        self.materials_dir = materials_dir
        self._static_cache: str | None = None

    def static_text(self) -> str:
        if self._static_cache is None:
            from app.context.materials import load_materials

            self._static_cache = load_materials(self.materials_dir) if self.materials_dir else ""
        return self._static_cache

    def invalidate_static(self) -> None:
        """materials 目录有新增/变更后调用，使 STATIC 上下文下次重新加载。"""
        self._static_cache = None

    def build(self, sid: str, task: str, target: str | None, focus_segs: list[dict] | None = None) -> dict:
        """返回 {system, user} 消息结构所需的上下文文本块。"""
        static = self.static_text()[:2500]
        recent = self._recent(sid)[:2500]
        focus = self._focus(focus_segs)[:600]
        history = self._history(sid)[:400]
        return {
            "static": static,
            "recent": recent,
            "focus": focus,
            "history": history,
        }

    def _recent(self, sid: str) -> str:
        segs = self.store.recent_segments(sid, seconds=30 * 60)
        lines = []
        for s in segs:
            t = s.get("revised_text") or s.get("text") or ""
            if t:
                lines.append(t)
        return "\n".join(lines)

    def _focus(self, segs: list[dict] | None) -> str:
        if not segs:
            return ""
        lines = []
        for s in segs:
            t = s.get("revised_text") or s.get("text") or ""
            if t:
                lines.append(t)
        return "\n".join(lines)

    def _history(self, sid: str) -> str:
        invs = self.store.recent_invocations(sid)
        return "\n".join(f"[{i['task']}] {i['output_text']}" for i in invs)
