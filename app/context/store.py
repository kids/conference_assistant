"""SQLite 存储：转写段、调用记录、确认记录、指标（WAL 模式，线程安全）。"""
from __future__ import annotations

import json
import os
import sqlite3
import threading
import time
import uuid
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS session(
  id TEXT PRIMARY KEY, title TEXT, speaker_name TEXT, institution TEXT DEFAULT '',
  discipline TEXT, ai_enabled INTEGER DEFAULT 1, consent_at REAL, started_at REAL, ended_at REAL
);
CREATE TABLE IF NOT EXISTS segment(
  id TEXT PRIMARY KEY, session_id TEXT, seq INTEGER, t_start REAL, t_end REAL,
  track TEXT DEFAULT 'main', speaker_hint TEXT DEFAULT '', text TEXT,
  revised_text TEXT, is_final INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS invocation(
  id TEXT PRIMARY KEY, session_id TEXT, task TEXT, target TEXT,
  prompt_hash TEXT, output_text TEXT, checks_json TEXT,
  status TEXT DEFAULT 'generated', gen_ms REAL, shown_sec REAL, created_at REAL
);
CREATE TABLE IF NOT EXISTS confirmation(
  invocation_id TEXT PRIMARY KEY, confirmed_by TEXT, verdict TEXT, note TEXT
);
CREATE TABLE IF NOT EXISTS metric(
  session_id TEXT, k TEXT, v REAL, at REAL
);
CREATE INDEX IF NOT EXISTS idx_segment_session ON segment(session_id, seq);
"""


class Store:
    def __init__(self, db_path: Path) -> None:
        db_path.parent.mkdir(parents=True, exist_ok=True)
        self._conn = sqlite3.connect(str(db_path), check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._conn.executescript(SCHEMA)
        self._migrate()
        self._lock = threading.Lock()

    def _migrate(self) -> None:
        """对旧库做增量列迁移（幂等）。"""
        cols = {r["name"] for r in self._conn.execute("PRAGMA table_info(session)")}
        if "institution" not in cols:
            self._conn.execute("ALTER TABLE session ADD COLUMN institution TEXT DEFAULT ''")
            self._conn.commit()

    def _exec(self, sql: str, params: tuple = ()) -> None:
        with self._lock:
            self._conn.execute(sql, params)
            self._conn.commit()

    def _query(self, sql: str, params: tuple = ()) -> list[sqlite3.Row]:
        with self._lock:
            cur = self._conn.execute(sql, params)
            return cur.fetchall()

    @staticmethod
    def _row_to_dict(row: sqlite3.Row) -> dict:
        return {k: row[k] for k in row.keys()}

    # ---- session ----
    def create_session(self, title: str, speaker: str, discipline: str,
                       ai_enabled: bool = True, institution: str = "") -> str:
        sid = uuid.uuid4().hex[:12]
        now = time.time()
        self._exec(
            "INSERT INTO session(id,title,speaker_name,institution,discipline,ai_enabled,consent_at,started_at)"
            " VALUES(?,?,?,?,?,?,?,?)",
            (sid, title, speaker, institution, discipline, int(ai_enabled),
             now if ai_enabled else None, now),
        )
        return sid

    def set_ended(self, sid: str) -> None:
        self._exec("UPDATE session SET ended_at=? WHERE id=?", (time.time(), sid))

    def get_session(self, sid: str) -> sqlite3.Row | None:
        rows = self._query("SELECT * FROM session WHERE id=?", (sid,))
        return rows[0] if rows else None

    # ---- segment ----
    def add_segment(self, sid: str, seq: int, t_start: float, t_end: float, text: str,
                    track: str = "main") -> str:
        # id 带 session 前缀，避免服务重启后 seq 从头计数与历史数据撞主键
        seg_id = f"{sid}-s{seq:05d}"
        self._exec(
            "INSERT INTO segment(id,session_id,seq,t_start,t_end,track,text,is_final)"
            " VALUES(?,?,?,?,?,?,?,1)",
            (seg_id, sid, seq, t_start, t_end, track, text),
        )
        return seg_id

    def revise_segment(self, seg_id: str, text: str) -> None:
        self._exec("UPDATE segment SET revised_text=? WHERE id=?", (text, seg_id))

    def recent_segments(self, sid: str, seconds: float | None = None, limit: int = 500) -> list[dict]:
        if seconds is not None:
            since = time.time() - seconds
            rows = self._query(
                "SELECT * FROM segment WHERE session_id=? AND is_final=1 AND t_start>=? ORDER BY seq",
                (sid, since),
            )
        else:
            rows = self._query(
                "SELECT * FROM segment WHERE session_id=? AND is_final=1 ORDER BY seq DESC LIMIT ?",
                (sid, limit),
            )
            rows = list(reversed(rows))
        return [self._row_to_dict(r) for r in rows]

    def segments_between(self, sid: str, t_start: float, t_end: float) -> list[dict]:
        rows = self._query(
            "SELECT * FROM segment WHERE session_id=? AND is_final=1 AND t_start>=? AND t_start<=? ORDER BY seq",
            (sid, t_start, t_end),
        )
        return [self._row_to_dict(r) for r in rows]

    # ---- invocation ----
    def add_invocation(self, sid: str, task: str, target: str, prompt_hash: str,
                       output_text: str, checks: dict, gen_ms: float, iid: str | None = None) -> str:
        if iid is None:
            iid = uuid.uuid4().hex[:12]
        self._exec(
            "INSERT INTO invocation(id,session_id,task,target,prompt_hash,output_text,checks_json,"
            "status,gen_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
            (iid, sid, task, target, prompt_hash, output_text, json.dumps(checks, ensure_ascii=False),
             "generated", gen_ms, time.time()),
        )
        return iid

    def get_invocation(self, iid: str) -> dict | None:
        rows = self._query("SELECT * FROM invocation WHERE id=?", (iid,))
        return self._row_to_dict(rows[0]) if rows else None

    def revise_invocation(self, iid: str, text: str) -> None:
        """人工修订 AI 输出文本（投屏前/后编辑），覆盖 output_text。"""
        self._exec("UPDATE invocation SET output_text=? WHERE id=?", (text, iid))

    def set_status(self, iid: str, status: str, shown_sec: float = 0.0) -> None:
        self._exec("UPDATE invocation SET status=?, shown_sec=? WHERE id=?", (status, shown_sec, iid))

    def invocation_count(self, sid: str) -> int:
        rows = self._query("SELECT COUNT(*) c FROM invocation WHERE session_id=?", (sid,))
        return rows[0]["c"] if rows else 0

    def recent_invocations(self, sid: str, limit: int = 10) -> list[dict]:
        rows = self._query(
            "SELECT task,output_text FROM invocation WHERE session_id=? AND status IN ('shown','generated')"
            " ORDER BY created_at DESC LIMIT ?", (sid, limit),
        )
        return [self._row_to_dict(r) for r in reversed(rows)]

    # ---- confirmation ----
    def confirm(self, iid: str, confirmed_by: str, verdict: str, note: str = "") -> None:
        self._exec(
            "INSERT OR REPLACE INTO confirmation(invocation_id,confirmed_by,verdict,note) VALUES(?,?,?,?)",
            (iid, confirmed_by, verdict, note),
        )

    # ---- metric ----
    def add_metric(self, sid: str, k: str, v: float) -> None:
        self._exec("INSERT INTO metric(session_id,k,v,at) VALUES(?,?,?,?)", (sid, k, v, time.time()))
