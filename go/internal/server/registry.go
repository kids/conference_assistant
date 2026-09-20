package server

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"seat/internal/config"
	"seat/internal/contextx"
	"seat/internal/diarize"
	"seat/internal/store"
)

// DefaultMaxSessions 同时可用的会场上限。每个活跃会场占一条 ASR 连接 + 一路音频采集
// + 一个流水线协程 + 一个说话人工作协程，上限用来防误开与资源失控。
const DefaultMaxSessions = 4

// Registry 多会场注册表：每个会场（sid）一个独立的 Runtime 实例。
//
// 共享（进程级）：Settings、Store（SQLite，WAL 支持并发）、diarizer
// （sidecar 内部按 session 分组，天然支持多会场）。
// 隔离（会场级）：事件 Bus、AI 展示状态、流水线/采集/ASR 连接、上下文、热词、说话人状态。
// 隔离带来的语义：不同会场的转写与 AI 输出互不可见、互不干扰；
// 同一会场（同一 sid）的多个页面则共享同一份状态（多人看同一个控制台）。
type Registry struct {
	Settings *config.Settings
	Store    *store.Store
	diarizer *diarize.Client

	max int

	mu       sync.Mutex
	sessions map[string]*Runtime
	order    []string // 创建顺序（用于列表与「最近会场」回落）
}

// NewRegistry 创建注册表：打开共享 Store、初始化 diarizer、清理超期录音。
func NewRegistry(s *config.Settings) (*Registry, error) {
	st, err := store.Open(filepath.Join(s.DataDirPath(), "transcript.sqlite"))
	if err != nil {
		return nil, err
	}
	reg := &Registry{
		Settings: s,
		Store:    st,
		max:      DefaultMaxSessions,
		sessions: map[string]*Runtime{},
	}
	if s.DiarizeEnabled {
		reg.diarizer = diarize.New(s.DiarizeURL, s.DiarizeTimeout, s.DiarizeThreshold, s.DiarizeMinMS)
		log.Printf("[diarize] 说话人区分已启用：%s（阈值 %.2f，最短 %dms，自动拉起 %v）",
			s.DiarizeURL, s.DiarizeThreshold, s.DiarizeMinMS, s.DiarizeAutostart)
	}
	// 录音保留期清理：启动时清一次（新建会话时还会再清一次，见 Create）
	go sweepRecFiles(s.DataDirPath(), s.RecKeepDays)
	return reg, nil
}

// Get 取已注册的会场运行时；不存在返回 nil。
func (reg *Registry) Get(sid string) *Runtime {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.sessions[sid]
}

// Ensure 取会场运行时；未注册时按「恢复已有会场」构造（浏览器历史 / 分享链接 /
// 服务重启后都要能回到原会场）。返回 (nil, nil) 表示该 sid 在数据库里也不存在。
func (reg *Registry) Ensure(sid string) (*Runtime, error) {
	if rt := reg.Get(sid); rt != nil {
		return rt, nil
	}
	ok, err := reg.Store.HasSession(sid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	rt := reg.newRuntime(sid)
	reg.mu.Lock()
	if cur := reg.sessions[sid]; cur != nil { // 并发 Ensure 同一 sid：保留先到者
		reg.mu.Unlock()
		rt.close()
		return cur, nil
	}
	reg.sessions[sid] = rt
	reg.order = append(reg.order, sid)
	reg.mu.Unlock()
	return rt, nil
}

// Create 新建会场：DB 记录 + 目录 + 独立 Runtime（尚未启动音频流水线，
// 启动见 Runtime.initSession）。超上限返回 400。
func (reg *Registry) Create(title, speaker, institution, discipline string,
	aiEnabled bool) (*Runtime, error) {
	reg.mu.Lock()
	if len(reg.sessions) >= reg.max {
		reg.mu.Unlock()
		return nil, badRequest("同时最多 %d 个会场；请先结束不用的会场", reg.max)
	}
	reg.mu.Unlock()

	sid, err := reg.Store.CreateSession(title, speaker, discipline, aiEnabled, institution)
	if err != nil {
		return nil, serverError("创建会话失败: %v", err)
	}
	sessionDir := filepath.Join(reg.Settings.DataDirPath(), sid)
	if err := mkdirAll(filepath.Join(sessionDir, "materials")); err != nil {
		return nil, serverError("创建会话目录失败: %v", err)
	}
	if reg.diarizer != nil {
		reg.resetDiarize(sid)
	}
	// 新建会话时顺手清一遍超期录音（保留期见 REC_KEEP_DAYS）
	go sweepRecFiles(reg.Settings.DataDirPath(), reg.Settings.RecKeepDays)

	rt := reg.newRuntime(sid)
	reg.mu.Lock()
	reg.sessions[sid] = rt
	reg.order = append(reg.order, sid)
	reg.mu.Unlock()
	return rt, nil
}

// EnsureDefault 保证至少存在一个会场（服务启动时调用）：
// 保持「打开就能用」的单会场体验与旧版一致。
func (reg *Registry) EnsureDefault() *Runtime {
	if rt := reg.Recent(); rt != nil {
		return rt
	}
	rt, err := reg.Create("Workshop", "", "", "", true)
	if err != nil {
		log.Printf("[session] 创建默认会场失败: %v", err)
		return nil
	}
	return rt
}

// Recent 最近创建的会场（无则 nil）：供无 sid 的入口回落。
func (reg *Registry) Recent() *Runtime {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for i := len(reg.order) - 1; i >= 0; i-- {
		if rt := reg.sessions[reg.order[i]]; rt != nil {
			return rt
		}
	}
	return nil
}

// List 当前注册的会场（按创建顺序）。
func (reg *Registry) List() []*Runtime {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := make([]*Runtime, 0, len(reg.order))
	for _, sid := range reg.order {
		if rt := reg.sessions[sid]; rt != nil {
			out = append(out, rt)
		}
	}
	return out
}

// Close 关闭全部会场与共享资源。
func (reg *Registry) Close() {
	reg.mu.Lock()
	rts := make([]*Runtime, 0, len(reg.sessions))
	for _, rt := range reg.sessions {
		rts = append(rts, rt)
	}
	reg.sessions = map[string]*Runtime{}
	reg.order = nil
	reg.mu.Unlock()
	for _, rt := range rts {
		rt.close()
	}
	reg.Store.Close()
	contextx.CloseTerms()
}

// resetDiarize 重置 sidecar 里该会场的说话人表（新会场从零编号）。
// Runtime 未就绪时也能调用（只依赖共享的 diarizer client）。
func (reg *Registry) resetDiarize(sid string) {
	if reg.diarizer == nil {
		return
	}
	go func() {
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		if err := reg.diarizer.Reset(ctx, sid, filepath.Join(reg.Settings.DataDirPath(), sid)); err != nil {
			log.Printf("[diarize] 重置 session %s 说话人表失败（不影响转写）：%v", sid, err)
		}
	}()
}

// newRuntime 构造一个会场运行时（共享 Store 与 diarizer）。
func (reg *Registry) newRuntime(sid string) *Runtime {
	return newSessionRuntime(reg.Settings, reg.Store, reg.diarizer, sid)
}
