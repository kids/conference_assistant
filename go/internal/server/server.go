package server

import (
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"seat/internal/audio"
	"seat/internal/events"
)

// screenTypes 大屏只接收这些事件（对齐 Python 版 _SCREEN_TYPES）。
var screenTypes = map[string]struct{}{
	"TRANSCRIPT_PARTIAL": {}, "TRANSCRIPT_FINAL": {}, "TRANSCRIPT_REVISED": {},
	"AI_STATE": {}, "AI_DELTA": {}, "AI_READY": {}, "AI_SHOWING": {}, "AI_DONE": {},
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
	rt    *Runtime
	webFS fs.FS
	mux   *http.ServeMux
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// 内网工具，且页面与 WS 同源；不额外做来源校验（与 Python 版行为一致）
	CheckOrigin: func(r *http.Request) bool { return true },
}

// New 创建服务。webFS 为内嵌的前端静态资源（console.html / screen.html）。
func New(rt *Runtime, webFS fs.FS) *Server {
	s := &Server{rt: rt, webFS: webFS}
	s.mux = s.routes()
	return s
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// ---- 页面 ----
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console", http.StatusFound)
	})
	mux.HandleFunc("GET /console", s.servePage("console.html"))
	mux.HandleFunc("GET /screen", s.servePage("screen.html"))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.webFS))))

	// ---- REST ----
	mux.HandleFunc("POST /api/session", s.handleCreateSession)
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
	mux.HandleFunc("POST /api/invoke", s.handleInvoke)
	mux.HandleFunc("POST /api/invocation/{iid}/show", s.handleShow)
	mux.HandleFunc("PATCH /api/invocation/{iid}", s.handleReviseInvocation)
	mux.HandleFunc("POST /api/invocation/{iid}/discard", s.handleDiscard)
	mux.HandleFunc("POST /api/invocation/{iid}/confirm", s.handleConfirm)
	mux.HandleFunc("POST /api/kill", s.handleKill)
	mux.HandleFunc("PATCH /api/segment/{segID}", s.handleReviseSegment)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// ---- WebSocket ----
	mux.HandleFunc("GET /ws/console", func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, false) })
	mux.HandleFunc("GET /ws/screen", func(w http.ResponseWriter, r *http.Request) { s.serveWS(w, r, true) })
	mux.HandleFunc("GET /ws/audio", s.serveAudioWS)

	return mux
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
func (s *Server) snapshot() events.Event {
	hist := s.rt.Bus.Snapshot()
	if len(hist) > 30 {
		hist = hist[len(hist)-30:]
	}
	evs := make([]events.Event, len(hist))
	copy(evs, hist)
	return events.Event{
		"type":       "SNAPSHOT",
		"state":      string(s.rt.Display.State()),
		"session_id": s.rt.SessionID(),
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

	q := s.rt.Bus.Subscribe()
	defer s.rt.Bus.Unsubscribe(q)

	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := conn.WriteJSON(s.snapshot()); err != nil {
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

	defer func() {
		if bc, ok := s.rt.Capture().(*audio.BrowserCapture); ok {
			bc.MarkDisconnected()
			s.rt.Bus.Publish(events.Event{"type": "MIC_STATUS", "status": "disconnected"})
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
		// 动态读取当前 capture：切换/重启 session 后音频不会喂到旧采集对象
		bc, ok := s.rt.Capture().(*audio.BrowserCapture)
		if !ok {
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(4001, "流水线未开启浏览器收音"))
			return
		}
		if !bc.Connected() {
			bc.MarkConnected()
			s.rt.Bus.Publish(events.Event{"type": "MIC_STATUS", "status": "connected"})
		}
		bc.Feed(data)
	}
}

// LogStartup 打印启动信息。
func (s *Server) LogStartup(host string, port int) {
	log.Printf("[seat] 控制台：    http://%s:%d/console", host, port)
	log.Printf("[seat] 投屏大屏：  http://%s:%d/screen", host, port)
}
