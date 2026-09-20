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
//
// RECENT 按「大窗口模型 + 保最新」设定：不再限制时间窗（取本场全部转写），
// 超出预算时保留最新内容而不是最早的（见 clipRunesTail）。40k 汉字 ≈ 2.5 小时口播，
// 覆盖单场会议绰绰有余；prefill 成本很低，上下文放大对单次调用耗时几乎无感。
const (
	staticContextMaxChars  = 6000
	recentContextMaxChars  = 40000
	focusContextMaxChars   = 2000
	historyContextMaxChars = 2000
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

// Build 组装四块上下文（含字符预算截断）。
// 截断方向：STATIC 是文档（摘要/术语表，头部信息密度高）保头；
// RECENT / FOCUS / HISTORY 都是「越新越重要」，超预算一律保最新。
func (m *Manager) Build(sid string, focusSegs []store.Segment) Context {
	return Context{
		Static:  clipRunes(m.StaticText(), staticContextMaxChars),
		Recent:  clipRunesTail(m.recent(sid), recentContextMaxChars),
		Focus:   clipRunesTail(joinSegments(focusSegs), focusContextMaxChars),
		History: clipRunesTail(m.history(sid), historyContextMaxChars),
	}
}

// recentSegmentLimit 取本场转写时的条数上限（≈ 8 小时会议的句数，等同不限）。
const recentSegmentLimit = 10000

// recent 本场全部转写（升序拼接）。不再限制时间窗：长度由字符预算兜底，
// 超预算时 Build 保留最新内容（见 clipRunesTail）——「报告总结」这类需要
// 全场的任务才能看到完整过程，而不是只有最近 30 分钟的一小段。
func (m *Manager) recent(sid string) string {
	segs, err := m.store.RecentSegments(sid, 0, recentSegmentLimit)
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

// clipRunesTail 与 clipRunes 相反：超预算时保留**末尾**。
// 用于「越新越重要」的块：宁缺早期内容，不能丢最新的。
func clipRunesTail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[len(r)-max:])
}
