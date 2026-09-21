package server

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"seat/internal/audio"
	"seat/internal/events"
)

// screenTypes 大屏只接收这些事件（对齐 Python 版 _SCREEN_TYPES）。
// 注意：新增事件若要上大屏，必须加进这张表 —— 漏加的表现是"后端发了、页面没反应"，
// 从页面侧完全看不出原因（translation_seat 的 Go/Python 两版都以本表为准）。
var screenTypes = map[string]struct{}{
	"TRANSCRIPT_PARTIAL": {}, "TRANSCRIPT_FINAL": {}, "TRANSCRIPT_REVISED": {},
	"SPEAKER_ASSIGNED": {},
	"AI_STATE":         {}, "AI_DELTA": {}, "AI_READY": {}, "AI_SHOWING": {}, "AI_DONE": {},
	"AI_KILLED": {}, "HEALTH": {}, "SNAPSHOT": {}, "SESSION": {},
}

const (
	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 25 * time.Second
	wsPongTimeout  = 60 * time.Second
	wsReadLimit    = 4 << 20 // /ws/audio 单帧上限 4MB，防异常客户端打爆内存
)

// Server HTTP/WebSocket 服务。
type Server struct {
	reg   *Registry
	webFS fs.FS
	mux   *http.ServeMux

	// cur 当前会场：无 sid 的入口（/console、/api/*、/ws/*）作用于它。
	// 会场的 URL 路由（/<sid>/...）与前端绑定落地后，它退化为「最近活跃会场」的回落。
	mu  sync.Mutex
	cur *Runtime
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// 内网工具，且页面与 WS 同源；不额外做来源校验（与 Python 版行为一致）
	CheckOrigin: func(r *http.Request) bool { return true },
}

// New 创建服务。webFS 为内嵌的前端静态资源（console.html / screen.html）。
func New(reg *Registry, webFS fs.FS) *Server {
	s := &Server{reg: reg, webFS: webFS, cur: reg.Recent()}
	s.mux = s.routes()
	return s
}

// current 取本请求所属的会场：
//   - 带 sid 的路由（/<sid>/...）由 withSession 解析后注入请求上下文；
//   - 无 sid 的旧路由（兼容保留）回落到「当前会场」。
//
// 同时刷新会场的活跃时间（供 Registry 的 LRU 回收判断）。
func (s *Server) current(r *http.Request) *Runtime {
	if rt, ok := r.Context().Value(rtCtxKey{}).(*Runtime); ok && rt != nil {
		rt.Touch()
		return rt
	}
	s.mu.Lock()
	rt := s.cur
	s.mu.Unlock()
	if rt != nil {
		rt.Touch()
	}
	return rt
}

// rtCtxKey 请求上下文里存放「本请求所属会场」的键。
type rtCtxKey struct{}

// withSession 把路径参数 {sid} 解析为会场运行时（必要时按 DB 记录恢复）并注入上下文；
// 会场不存在时返回 400。/<sid>/... 下的全部路由都经它进入。
func (s *Server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		rt, err := s.reg.Ensure(sid)
		if err != nil {
			writeErr(w, err)
			return
		}
		if rt == nil {
			writeErr(w, badRequest("会场不存在：%s", sid))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), rtCtxKey{}, rt)))
	}
}

// setCurrent 切换当前会场（新建/恢复会话时）。
func (s *Server) setCurrent(rt *Runtime) {
	s.mu.Lock()
	s.cur = rt
	s.mu.Unlock()
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- 入口（无 sid）----
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// 挂在路径前缀下时（Kong base path + strip_path），后端看到的是去掉前缀的路径，
		// 此时直接跳 /console 会让浏览器丢掉前缀、落到网关的其它路由上。
		// 前缀由网关通过 X-Forwarded-Prefix 告知（腾讯云 Kong 可配；未配则为空，行为不变）。
		prefix := strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
		http.Redirect(w, r, prefix+"/console", http.StatusFound)
	})
	// /console：新建会场并跳转 /<sid>/console（每个控制台页面绑定一个独立会场；
	//          打开同一个 sid 的地址才是共享同一份状态）。
	mux.HandleFunc("GET /console", s.handleConsoleEntry)
	// /screen：跳转到「最近的会场」的投屏页（大屏不新建会场）。
	mux.HandleFunc("GET /screen", s.handleScreenEntry)

	// ---- 会场页面 ----
	// 注：原先的 GET /static/ 前缀路由已移除 —— 它没有任何实际资源（web 目录只有
	// 两个 HTML，页面也未引用），而前缀模式与 /{sid}/console 互相覆盖，
	// Go 1.22 ServeMux 会直接 panic（"/static/console" 两边都匹配）。
	mux.HandleFunc("GET /{sid}/console", s.servePage("console.html"))
	mux.HandleFunc("GET /{sid}/screen", s.servePage("screen.html"))

	// ---- 会场 REST（/{sid}/api/...，由 withSession 注入本请求的会场）----
	scoped := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{"POST", "/api/session", s.handleStartSessionCompat}, // 前端「开始 Session」：启动本会场流水线
		{"POST", "/api/start", s.handleStartSession},
		{"POST", "/api/stop", s.handleStopSession},
		{"GET", "/api/export", s.handleExport},
		{"GET", "/api/hotwords", s.handleGetHotwords},
		{"PUT", "/api/hotwords", s.handlePutHotwords},
		{"POST", "/api/hotwords/toggle", s.handleToggleHotwords},
		{"POST", "/api/hotwords/generate", s.handleGenerateHotwords},
		{"POST", "/api/materials/upload", s.handleUploadMaterial},
		{"GET", "/api/materials", s.handleListMaterials},
		{"GET", "/api/target-discipline", s.handleGetTargetDiscipline},
		{"POST", "/api/target-discipline", s.handleSetTargetDiscipline},
		{"GET", "/api/devices", s.handleDevices},
		{"GET", "/api/speech/options", s.handleSpeechOptions},
		{"GET", "/api/speakers", s.handleSpeakers},
		{"POST", "/api/invoke", s.handleInvoke},
		{"POST", "/api/invocation/{iid}/show", s.handleShow},
		{"PATCH", "/api/invocation/{iid}", s.handleReviseInvocation},
		{"POST", "/api/invocation/{iid}/discard", s.handleDiscard},
		{"POST", "/api/invocation/{iid}/confirm", s.handleConfirm},
		{"POST", "/api/kill", s.handleKill},
		{"PATCH", "/api/segment/{segID}", s.handleReviseSegment},
		{"GET", "/api/health", s.handleHealth},
	}
	for _, e := range scoped {
		mux.HandleFunc(e.method+" /{sid}"+e.path, s.withSession(e.h))
	}

	// ---- 全局（不属于任何会场）----
	mux.HandleFunc("POST /api/session", s.handleCreateSession) // 新建会场，返回新 sid
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)  // 活跃会场列表

	// ---- 会场 WebSocket ----
	mux.HandleFunc("GET /{sid}/ws/console", s.withSession(func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, false) }))
	mux.HandleFunc("GET /{sid}/ws/screen", s.withSession(func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, true) }))
	mux.HandleFunc("GET /{sid}/ws/audio", s.withSession(s.serveAudioWS))

	// ---- 兼容：无 sid 的旧路由作用于「当前会场」（现有页面/脚本不受影响）----
	mux.HandleFunc("POST /api/session/{sid}/start", s.handleStartSession)
	mux.HandleFunc("POST /api/session/{sid}/stop", s.handleStopSession)
	mux.HandleFunc("GET /api/session/{sid}/export", s.handleExport)
	mux.HandleFunc("GET /api/hotwords", s.handleGetHotwords)
	mux.HandleFunc("PUT /api/hotwords", s.handlePutHotwords)
	mux.HandleFunc("POST /api/hotwords/toggle", s.handleToggleHotwords)
	mux.HandleFunc("POST /api/hotwords/generate", s.handleGenerateHotwords)
	mux.HandleFunc("POST /api/materials/upload", s.handleUploadMaterial)
	mux.HandleFunc("GET /api/materials", s.handleListMaterials)
	mux.HandleFunc("GET /api/target-discipline", s.handleGetTargetDiscipline)
	mux.HandleFunc("POST /api/target-discipline", s.handleSetTargetDiscipline)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("GET /api/speech/options", s.handleSpeechOptions)
	mux.HandleFunc("GET /api/speakers", s.handleSpeakers)
	mux.HandleFunc("POST /api/invoke", s.handleInvoke)
	mux.HandleFunc("POST /api/invocation/{iid}/show", s.handleShow)
	mux.HandleFunc("PATCH /api/invocation/{iid}", s.handleReviseInvocation)
	mux.HandleFunc("POST /api/invocation/{iid}/discard", s.handleDiscard)
	mux.HandleFunc("POST /api/invocation/{iid}/confirm", s.handleConfirm)
	mux.HandleFunc("POST /api/kill", s.handleKill)
	mux.HandleFunc("PATCH /api/segment/{segID}", s.handleReviseSegment)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /ws/console", func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, false) })
	mux.HandleFunc("GET /ws/screen", func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, true) })
	mux.HandleFunc("GET /ws/audio", s.serveAudioWS)

	return mux
}

// handleConsoleEntry 无 sid 的控制台入口：新建一个会场并跳转到 /<sid>/console。
// 这是刻意的设计——每个控制台页面绑定一个独立会场（sid 写进 URL）；
// 打开同一个 sid 的地址则共享同一份状态（多人看同一个控制台）。
func (s *Server) handleConsoleEntry(w http.ResponseWriter, r *http.Request) {
	rt, err := s.reg.Create("Workshop", "", "", "", true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setCurrent(rt)
	prefix := strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
	http.Redirect(w, r, prefix+"/"+rt.SessionID()+"/console", http.StatusSeeOther)
}

// handleScreenEntry 无 sid 的大屏入口：跳转到「最近的会场」的投屏页。
// 大屏是只读终端，不新建会场；要投某个会场的屏，直接用 /<sid>/screen。
func (s *Server) handleScreenEntry(w http.ResponseWriter, r *http.Request) {
	rt := s.reg.Recent()
	if rt == nil {
		rt = s.reg.EnsureDefault()
	}
	if rt == nil {
		http.Error(w, "暂无会场", http.StatusNotFound)
		return
	}
	prefix := strings.TrimRight(r.Header.Get("X-Forwarded-Prefix"), "/")
	http.Redirect(w, r, prefix+"/"+rt.SessionID()+"/screen", http.StatusSeeOther)
}

// handleListSessions 当前活跃会场列表（含各自的控制台/投屏地址）。
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	items := make([]map[string]any, 0, DefaultMaxSessions)
	for _, rt := range s.reg.List() {
		sid := rt.SessionID()
		items = append(items, map[string]any{
			"session_id": sid,
			"state":      string(rt.Display.State()),
			"pipeline":   rt.PipelineAlive(),
			"console":    "/" + sid + "/console",
			"screen":     "/" + sid + "/screen",
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.webFS, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

// snapshot 连接建立时补发的快照（只带最近 30 条历史）。
func (s *Server) snapshot(rt *Runtime) events.Event {
	hist := rt.Bus.Snapshot()
	if len(hist) > 30 {
		hist = hist[len(hist)-30:]
	}
	evs := make([]events.Event, len(hist))
	copy(evs, hist)
	return events.Event{
		"type":       "SNAPSHOT",
		"state":      string(rt.Display.State()),
		"session_id": rt.SessionID(),
		"events":     evs,
	}
}

// serveWS 控制台 / 大屏的事件推送。
// 单写协程（select 事件 + ping 心跳），单读协程（探测断开 + 响应 pong），符合 gorilla 的单读单写约束。
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request, screenOnly bool) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// 订阅本请求所属会场的事件总线：不同会场的事件天然隔离
	rt := s.current(r)
	q := rt.Bus.Subscribe()
	defer rt.Bus.Unsubscribe(q)

	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := conn.WriteJSON(s.snapshot(rt)); err != nil {
		return
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn.SetReadLimit(wsReadLimit)
		_ = conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-closed:
			return
		case ev, ok := <-q:
			if !ok {
				return
			}
			if screenOnly {
				if t, _ := ev["type"].(string); !isScreenType(t) {
					continue
				}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func isScreenType(t string) bool {
	_, ok := screenTypes[t]
	return ok
}

// serveAudioWS 远端浏览器收音：前端推送 16k/mono/int16 PCM 二进制帧，喂入采集队列。
func (s *Server) serveAudioWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(wsReadLimit)

	// 音频只进本请求所属会场：/<sid>/ws/audio 与旧 /ws/audio（当前会场）都走这里
	rt := s.current(r)

	defer func() {
		if bc, ok := rt.Capture().(*audio.BrowserCapture); ok {
			bc.MarkDisconnected()
			rt.Bus.Publish(events.Event{"type": "MIC_STATUS", "status": "disconnected"})
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(data) == 0 {
			continue
		}
		// 动态读取当前 capture：切换/重启流水线后音频不会喂到旧采集对象
		bc, ok := rt.Capture().(*audio.BrowserCapture)
		if !ok {
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(4001, "流水线未开启浏览器收音"))
			return
		}
		if !bc.Connected() {
			bc.MarkConnected()
			rt.Bus.Publish(events.Event{"type": "MIC_STATUS", "status": "connected"})
		}
		bc.Feed(data)
	}
}

// LogStartup 打印启动信息。
func (s *Server) LogStartup(host string, port int) {
	log.Printf("[seat] 控制台：    http://%s:%d/console", host, port)
	log.Printf("[seat] 投屏大屏：  http://%s:%d/screen", host, port)
}
