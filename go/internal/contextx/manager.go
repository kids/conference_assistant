package contextx

import (
	"strings"
	"sync"

	"seat/internal/store"
)

// Context 四块上下文文本。
type Context struct {
	Static  string
	Recent  string
	Focus   string
	History string
}

// 四块上下文各自的字符预算。STATIC 的值与 document.go 的 MaxDigestChars 有约束关系：
// 摘要目标字数必须小于它，否则会在 Build 时被静默截断。
const (
	staticContextMaxChars = 2500
	recentContextMaxChars = 2500
	focusContextMaxChars  = 600
	historyContextMaxChars = 400
)

// Manager 上下文组装：STATIC / RECENT / FOCUS / HISTORY 四块 + 预算控制。
type Manager struct {
	store        *store.Store
	materialsDir string

	mu          sync.Mutex
	staticCache *string // nil 表示未加载；指向空串表示已加载但为空
}

// NewManager 创建上下文管理器。
func NewManager(st *store.Store, materialsDir string) *Manager {
	return &Manager{store: st, materialsDir: materialsDir}
}

// StaticText STATIC 块（会前资料），首次访问时加载并缓存。
func (m *Manager) StaticText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.staticCache == nil {
		var text string
		if m.materialsDir != "" {
			text = LoadMaterials(m.materialsDir, defaultMaterialsMaxChars)
		}
		m.staticCache = &text
	}
	return *m.staticCache
}

// InvalidateStatic materials 目录有新增/变更后调用，使 STATIC 下次重新加载。
func (m *Manager) InvalidateStatic() {
	m.mu.Lock()
	m.staticCache = nil
	m.mu.Unlock()
}

// Build 组装四块上下文（含字符预算截断，与 Python 版一致）。
func (m *Manager) Build(sid string, focusSegs []store.Segment) Context {
	return Context{
		Static:  clipRunes(m.StaticText(), staticContextMaxChars),
		Recent:  clipRunes(m.recent(sid), recentContextMaxChars),
		Focus:   clipRunes(joinSegments(focusSegs), focusContextMaxChars),
		History: clipRunes(m.history(sid), historyContextMaxChars),
	}
}

// recent 最近 30 分钟转写。
func (m *Manager) recent(sid string) string {
	segs, err := m.store.RecentSegments(sid, 30*60, 0)
	if err != nil {
		return ""
	}
	return joinSegments(segs)
}

// history 本场已展示过的 AI 输出（避免重复）。
func (m *Manager) history(sid string) string {
	invs, err := m.store.RecentInvocations(sid, 10)
	if err != nil {
		return ""
	}
	lines := make([]string, 0, len(invs))
	for _, i := range invs {
		lines = append(lines, "["+i.Task+"] "+i.OutputText)
	}
	return strings.Join(lines, "\n")
}

func joinSegments(segs []store.Segment) string {
	lines := make([]string, 0, len(segs))
	for _, s := range segs {
		if t := s.Effective(); t != "" {
			lines = append(lines, t)
		}
	}
	return strings.Join(lines, "\n")
}

func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
