// Package store SQLite 持久化：转写段、调用记录、确认记录、指标。
// 对应 Python 版 app/context/store.py，表结构完全一致（可与 Python 版共用同一份 transcript.sqlite）。
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3" // sqlite3 驱动
)

const schema = `
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
`

// Store 数据库句柄。内部用 database/sql 连接池，限制为单连接：
// SQLite 是单写者，串行化可彻底避免 "database is locked"。
type Store struct {
	db *sql.DB
}

// Segment 一条已确认转写。
type Segment struct {
	ID          string
	SessionID   string
	Seq         int
	TStart      float64
	TEnd        float64
	Track       string
	SpeakerHint string
	Text        string
	RevisedText string
	IsFinal     bool
}

// Effective 返回生效文本（人工/AI 修订优先）。
func (s Segment) Effective() string {
	if s.RevisedText != "" {
		return s.RevisedText
	}
	return s.Text
}

// ToMap 输出与 Python 版 SELECT * 一致的字面键名，保证导出接口兼容。
func (s Segment) ToMap() map[string]any {
	return map[string]any{
		"id": s.ID, "session_id": s.SessionID, "seq": s.Seq,
		"t_start": s.TStart, "t_end": s.TEnd, "track": s.Track,
		"speaker_hint": s.SpeakerHint, "text": s.Text,
		"revised_text": nullIfEmpty(s.RevisedText), "is_final": boolToInt(s.IsFinal),
	}
}

// Invocation 一次 AI 调用记录。
type Invocation struct {
	ID         string
	SessionID  string
	Task       string
	Target     string
	PromptHash string
	OutputText string
	ChecksJSON string
	Status     string
	GenMS      float64
	ShownSec   float64
	CreatedAt  float64
}

// ToMap 与 Python 版 SELECT * 键名一致。
func (i Invocation) ToMap() map[string]any {
	return map[string]any{
		"id": i.ID, "session_id": i.SessionID, "task": i.Task, "target": i.Target,
		"prompt_hash": i.PromptHash, "output_text": i.OutputText,
		"checks_json": i.ChecksJSON, "status": i.Status,
		"gen_ms": i.GenMS, "shown_sec": i.ShownSec, "created_at": i.CreatedAt,
	}
}

// Open 打开（必要时创建）数据库并建表 / 增量迁移。
func Open(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	dsn := "file:" + dbPath + "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrate 对旧库做增量列迁移（幂等），对应 Python 版 _migrate。
func (s *Store) migrate() error {
	rows, err := s.db.Query("PRAGMA table_info(session)")
	if err != nil {
		return err
	}
	defer rows.Close()
	hasInstitution := false
	for rows.Next() {
		var (
			cid        int
			name, ctyp string
			notnull    int
			dflt       sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctyp, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "institution" {
			hasInstitution = true
		}
	}
	if !hasInstitution {
		if _, err := s.db.Exec("ALTER TABLE session ADD COLUMN institution TEXT DEFAULT ''"); err != nil {
			return err
		}
	}
	return nil
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ---- session ----

// CreateSession 新建会话，返回 session_id。
func (s *Store) CreateSession(title, speaker, discipline string, aiEnabled bool, institution string) (string, error) {
	sid := shortID()
	t := now()
	var consent any
	if aiEnabled {
		consent = t
	}
	_, err := s.db.Exec(
		`INSERT INTO session(id,title,speaker_name,institution,discipline,ai_enabled,consent_at,started_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		sid, title, speaker, institution, discipline, boolToInt(aiEnabled), consent, t,
	)
	if err != nil {
		return "", err
	}
	return sid, nil
}

// HasSession 会话是否存在（按 sid 恢复会场运行时时用）。
func (s *Store) HasSession(sid string) (bool, error) {
	var one int
	err := s.db.QueryRow("SELECT 1 FROM session WHERE id=?", sid).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SetEnded 标记会话结束。
func (s *Store) SetEnded(sid string) error {
	_, err := s.db.Exec("UPDATE session SET ended_at=? WHERE id=?", now(), sid)
	return err
}

// ---- segment ----

// AddSegment 落库一条终稿。seg_id 带 session 前缀，避免重启后 seq 从头计数撞主键。
func (s *Store) AddSegment(sid string, seq int, tStart, tEnd float64, text string) (string, error) {
	segID := fmt.Sprintf("%s-s%05d", sid, seq)
	_, err := s.db.Exec(
		`INSERT INTO segment(id,session_id,seq,t_start,t_end,track,text,is_final)
		 VALUES(?,?,?,?,?,?,?,1)`,
		segID, sid, seq, tStart, tEnd, "main", text,
	)
	if err != nil {
		return "", err
	}
	return segID, nil
}

// ReviseSegment 覆盖修订文本（人工修正或 AI 顺句改写）。
func (s *Store) ReviseSegment(segID, text string) error {
	_, err := s.db.Exec("UPDATE segment SET revised_text=? WHERE id=?", text, segID)
	return err
}

// SetSegmentSpeaker 写入说话人标记（说话人区分结果，异步补写）。
// 只写 speaker_hint，不动文本，因此可与人工修正 / LLM 顺句改写并发进行。
func (s *Store) SetSegmentSpeaker(segID, speaker string) error {
	_, err := s.db.Exec("UPDATE segment SET speaker_hint=? WHERE id=?", speaker, segID)
	return err
}

// SpeakerStat 一位说话人在本场的出现统计。
type SpeakerStat struct {
	Speaker  string
	Segments int
	Seconds  float64
}

// SessionSpeakers 本场已识别出的说话人（按首次出现顺序）。
// 数据源是 segment.speaker_hint —— 与「对齐到句子」是同一份事实，
// 不额外维护状态，因此页面刷新/导出看到的说话人始终一致。
func (s *Store) SessionSpeakers(sid string) ([]SpeakerStat, error) {
	rows, err := s.db.Query(
		`SELECT speaker_hint, COUNT(*), COALESCE(SUM(t_end - t_start), 0)
		 FROM segment
		 WHERE session_id=? AND speaker_hint <> ''
		 GROUP BY speaker_hint
		 ORDER BY MIN(seq)`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpeakerStat
	for rows.Next() {
		var st SpeakerStat
		if err := rows.Scan(&st.Speaker, &st.Segments, &st.Seconds); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// RecentSegments 最近转写：seconds>0 时按时间窗，否则取最近 limit 条（升序返回）。
func (s *Store) RecentSegments(sid string, seconds float64, limit int) ([]Segment, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if seconds > 0 {
		rows, err = s.db.Query(
			`SELECT id,session_id,seq,t_start,t_end,track,speaker_hint,text,revised_text,is_final
			 FROM segment WHERE session_id=? AND is_final=1 AND t_start>=? ORDER BY seq`,
			sid, now()-seconds,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id,session_id,seq,t_start,t_end,track,speaker_hint,text,revised_text,is_final
			 FROM segment WHERE session_id=? AND is_final=1 ORDER BY seq DESC LIMIT ?`,
			sid, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanSegments(rows)
	if err != nil {
		return nil, err
	}
	if seconds <= 0 {
		// 倒序取回后反转为升序，与 Python 版一致
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func scanSegments(rows *sql.Rows) ([]Segment, error) {
	var out []Segment
	for rows.Next() {
		var (
			s       Segment
			revised sql.NullString
			isFinal int
		)
		if err := rows.Scan(&s.ID, &s.SessionID, &s.Seq, &s.TStart, &s.TEnd, &s.Track,
			&s.SpeakerHint, &s.Text, &revised, &isFinal); err != nil {
			return nil, err
		}
		s.RevisedText = revised.String
		s.IsFinal = isFinal != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- invocation ----

// AddInvocation 落库一次 AI 调用。
func (s *Store) AddInvocation(sid, task, target, promptHash, outputText, checksJSON string, genMS float64, iid string) (string, error) {
	if iid == "" {
		iid = shortID()
	}
	_, err := s.db.Exec(
		`INSERT INTO invocation(id,session_id,task,target,prompt_hash,output_text,checks_json,
		 status,gen_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		iid, sid, task, target, promptHash, outputText, checksJSON, "generated", genMS, now(),
	)
	if err != nil {
		return "", err
	}
	return iid, nil
}

// GetInvocation 按 id 查询。
func (s *Store) GetInvocation(iid string) (*Invocation, error) {
	row := s.db.QueryRow(
		`SELECT id,session_id,task,target,prompt_hash,output_text,checks_json,status,gen_ms,shown_sec,created_at
		 FROM invocation WHERE id=?`, iid)
	var i Invocation
	err := row.Scan(&i.ID, &i.SessionID, &i.Task, &i.Target, &i.PromptHash, &i.OutputText,
		&i.ChecksJSON, &i.Status, &i.GenMS, &i.ShownSec, &i.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// ReviseInvocation 人工修订 AI 输出文本（投屏前/后编辑）。
func (s *Store) ReviseInvocation(iid, text string) error {
	_, err := s.db.Exec("UPDATE invocation SET output_text=? WHERE id=?", text, iid)
	return err
}

// SetStatus 更新状态（shown / discarded）。
func (s *Store) SetStatus(iid, status string, shownSec float64) error {
	_, err := s.db.Exec("UPDATE invocation SET status=?, shown_sec=? WHERE id=?", status, shownSec, iid)
	return err
}

// InvocationCount 会话内调用次数。
func (s *Store) InvocationCount(sid string) (int, error) {
	var c int
	err := s.db.QueryRow("SELECT COUNT(*) FROM invocation WHERE session_id=?", sid).Scan(&c)
	return c, err
}

// RecentInvocations 最近已展示/已生成的输出（升序返回），用于避免重复。
func (s *Store) RecentInvocations(sid string, limit int) ([]Invocation, error) {
	rows, err := s.db.Query(
		`SELECT id,session_id,task,target,prompt_hash,output_text,checks_json,status,gen_ms,shown_sec,created_at
		 FROM invocation WHERE session_id=? AND status IN ('shown','generated')
		 ORDER BY created_at DESC LIMIT ?`, sid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invocation
	for rows.Next() {
		var i Invocation
		if err := rows.Scan(&i.ID, &i.SessionID, &i.Task, &i.Target, &i.PromptHash, &i.OutputText,
			&i.ChecksJSON, &i.Status, &i.GenMS, &i.ShownSec, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ---- confirmation / metric ----

// Confirm 记录报告人确认结果。
func (s *Store) Confirm(iid, confirmedBy, verdict, note string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO confirmation(invocation_id,confirmed_by,verdict,note) VALUES(?,?,?,?)",
		iid, confirmedBy, verdict, note)
	return err
}

// AddMetric 记录一条指标。
func (s *Store) AddMetric(sid, k string, v float64) error {
	_, err := s.db.Exec("INSERT INTO metric(session_id,k,v,at) VALUES(?,?,?,?)", sid, k, v, now())
	return err
}

// ---- helpers ----

func shortID() string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 12)
	n := time.Now().UnixNano()
	for i := range b {
		b[i] = hexDigits[(n>>(uint(i)*4))&0xf]
	}
	return string(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ChecksJSON 序列化校验结果（供落库）。
func ChecksJSON(checks map[string]string) string {
	b, err := json.Marshal(checks)
	if err != nil {
		return "{}"
	}
	return string(b)
}
