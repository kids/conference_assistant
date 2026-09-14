package server

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gen2brain/malgo"

	"seat/internal/asr"
	"seat/internal/audio"
	"seat/internal/contextx"
	"seat/internal/events"
)

// ---- 请求体定义（与 Python 版 pydantic 模型字段对齐）----

type sessionIn struct {
	Title       string `json:"title"`
	Speaker     string `json:"speaker"`
	Institution string `json:"institution"`
	Discipline  string `json:"discipline"`
	AIEnabled   *bool  `json:"ai_enabled"`
	ReplayPath  string `json:"replay_path"`
	Capture     string `json:"capture"`
}

func (b *sessionIn) withDefaults() {
	if b.Title == "" {
		b.Title = "Workshop"
	}
	if b.Capture == "" {
		b.Capture = "mic"
	}
}

type hotwordIn struct {
	Name        string `json:"name"`
	Institution string `json:"institution"`
	Discipline  string `json:"discipline"`
}

type hotwordsTextIn struct {
	Text string `json:"text"`
}

type hotwordsToggleIn struct {
	Enabled bool `json:"enabled"`
}

type invokeIn struct {
	Task   string `json:"task"`
	Target string `json:"target"`
}

type reviseIn struct {
	Text string `json:"text"`
}

type showIn struct {
	Text string `json:"text"`
}

// ---- 端点实现 ----

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body sessionIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	body.withDefaults()
	aiEnabled := true
	if body.AIEnabled != nil {
		aiEnabled = *body.AIEnabled
	}

	res, err := s.rt.CreateSession(body.Title, body.Speaker, body.Institution, body.Discipline,
		aiEnabled, body.ReplayPath, body.Capture)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	var body sessionIn
	_ = decodeBody(r, &body)
	body.withDefaults()

	s.rt.mu.Lock()
	s.rt.sessionID = sid
	s.rt.mu.Unlock()

	res, err := s.rt.StartPipeline("", body.Capture)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	s.rt.StopPipeline()
	_ = s.rt.Store.SetEnded(sid)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	segs, err := s.rt.Store.RecentSegments(sid, 0, 500)
	if err != nil {
		writeErr(w, serverError("查询转写失败: %v", err))
		return
	}
	md := []string{"# 转写记录\n"}
	items := make([]map[string]any, 0, len(segs))
	for _, seg := range segs {
		md = append(md, "- "+seg.Effective())
		items = append(items, seg.ToMap())
	}
	writeJSON(w, 200, map[string]any{
		"transcript_md": strings.Join(md, "\n"),
		"segments":      items,
	})
}

func (s *Server) handleGetHotwords(w http.ResponseWriter, r *http.Request) {
	s.rt.mu.Lock()
	words := s.rt.sessionHotwords
	if len(words) == 0 {
		words = s.rt.baseGlossary
	}
	enabled := s.rt.hotwordsEnabled
	out := copyMap(words)
	s.rt.mu.Unlock()

	writeJSON(w, 200, map[string]any{"words": out, "enabled": enabled})
}

func (s *Server) handlePutHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordsTextIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	s.rt.mu.Lock()
	old := copyMap(s.rt.sessionHotwords)
	s.rt.mu.Unlock()

	newWords := asr.ParseHotwordsText(body.Text, old)
	s.rt.setSessionHotwords(newWords)
	s.rt.pushHotwords()
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(newWords)})
}

func (s *Server) handleToggleHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordsToggleIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	s.rt.mu.Lock()
	s.rt.hotwordsEnabled = body.Enabled
	s.rt.mu.Unlock()
	s.rt.pushHotwords()
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": body.Enabled})
}

func (s *Server) handleGenerateHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	ag := s.rt.Agent()
	if ag == nil || ag.LLM == nil {
		writeJSON(w, 400, map[string]any{"error": "LLM 未配置"})
		return
	}
	sid := s.rt.SessionID()
	if sid == "" {
		writeJSON(w, 400, map[string]any{"error": "请先创建 session"})
		return
	}

	// 上限须大于单次 LLM 调用的超时（热词生成按调用给 90s）
	ctx, cancel := contextWithTimeout(150 * time.Second)
	defer cancel()
	result, err := contextx.BuildSpeakerProfile(ctx, ag.LLM, body.Name, body.Institution, body.Discipline)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}

	sessionDir := filepath.Join(s.rt.Settings.DataDirPath(), sid)
	materialsDir := filepath.Join(sessionDir, "materials")
	_ = mkdirAll(materialsDir)

	if len(result.Hotwords) > 0 {
		_ = asr.SaveHotwords(filepath.Join(sessionDir, "hotwords.txt"), result.Hotwords)
	}
	if result.Profile != "" {
		writeFile(filepath.Join(materialsDir, "speaker_profile.md"),
			"# 报告人背景\n\n"+result.Profile+"\n")
		s.rt.mu.Lock()
		mgr := s.rt.context
		s.rt.mu.Unlock()
		if mgr != nil {
			mgr.InvalidateStatic()
		}
	}
	s.rt.setSessionHotwords(result.Hotwords)

	applied := "asr_reconnect"
	s.rt.mu.Lock()
	hasClient := s.rt.asrClient != nil
	s.rt.mu.Unlock()
	if hasClient {
		s.rt.pushHotwords()
	} else {
		if _, err := s.rt.StartPipeline("", "mic"); err == nil {
			applied = "pipeline_start"
		}
	}
	s.rt.Bus.Publish(events.Event{
		"type": "HOTWORDS_STATUS", "status": "generated:" + strconv.Itoa(len(result.Hotwords)),
	})
	writeJSON(w, 200, map[string]any{
		"hotwords": result.Hotwords,
		"fields":   result.Fields,
		"profile":  result.Profile,
		"applied":  applied,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	defer func() {
		ctx.Uninit()
		ctx.Free()
	}()

	describe := func(kind malgo.DeviceType, label string) []map[string]any {
		devs, err := ctx.Devices(kind)
		if err != nil {
			return []map[string]any{}
		}
		out := make([]map[string]any, 0, len(devs))
		for i := range devs {
			d := &devs[i]
			out = append(out, map[string]any{
				"name":         d.Name(),
				"is_default":   d.IsDefault != 0,
				"device_type":  label,
				"format_count": d.FormatCount,
			})
		}
		return out
	}
	inputs := describe(malgo.Capture, "capture")
	all := append(describe(malgo.Capture, "capture"), describe(malgo.Playback, "playback")...)
	writeJSON(w, 200, map[string]any{"inputs": inputs, "devices": all})
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	var body invokeIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	ag := s.rt.Agent()
	sid := s.rt.SessionID()
	if ag == nil || sid == "" {
		writeJSON(w, 400, map[string]any{"error": "请先创建 session"})
		return
	}
	focus, err := s.rt.Store.RecentSegments(sid, 60, 0)
	if err != nil {
		focus = nil
	}
	iid, err := ag.Generate(r.Context(), sid, body.Task, body.Target, focus)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"invocation_id": iid})
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	var body showIn
	_ = decodeBody(r, &body)

	s.rt.Display.Show()
	text := ""
	if strings.TrimSpace(body.Text) != "" {
		text = strings.TrimSpace(body.Text)
		_ = s.rt.Store.ReviseInvocation(iid, text) // 编辑版落库（复盘导出用编辑后文本）
	}
	if text == "" {
		if inv, _ := s.rt.Store.GetInvocation(iid); inv != nil {
			text = inv.OutputText
		}
	}
	task := ""
	if inv, _ := s.rt.Store.GetInvocation(iid); inv != nil {
		task = inv.Task
	}
	_ = s.rt.Store.SetStatus(iid, "shown", 0)
	s.rt.Bus.Publish(events.Event{
		"type": "AI_SHOWING", "invocation_id": iid, "text": text, "task": task,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleReviseInvocation(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	var body reviseIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	_ = s.rt.Store.ReviseInvocation(iid, body.Text)
	// 编辑 AI 输出卡；若该卡正在投屏，大屏实时同步更新
	if s.rt.Display.State() == "SHOWING" && s.rt.Display.Current() == iid {
		task := ""
		if inv, _ := s.rt.Store.GetInvocation(iid); inv != nil {
			task = inv.Task
		}
		s.rt.Bus.Publish(events.Event{
			"type": "AI_SHOWING", "invocation_id": iid, "text": body.Text, "task": task,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	s.rt.Display.Reset()
	_ = s.rt.Store.SetStatus(iid, "discarded", 0)
	s.rt.Bus.Publish(events.Event{"type": "AI_STATE", "state": "IDLE", "invocation_id": iid})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	var body struct {
		By      string `json:"by"`
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	_ = decodeBody(r, &body)
	if body.By == "" {
		body.By = "speaker"
	}
	if body.Verdict == "" {
		body.Verdict = "ok"
	}
	_ = s.rt.Store.Confirm(iid, body.By, body.Verdict, body.Note)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	s.rt.Display.Kill()
	s.rt.Bus.Publish(events.Event{"type": "AI_KILLED", "invocation_id": s.rt.Display.Current()})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleReviseSegment(w http.ResponseWriter, r *http.Request) {
	segID := r.PathValue("segID")
	var body reviseIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if err := s.rt.Store.ReviseSegment(segID, body.Text); err != nil {
		writeErr(w, serverError("修订失败: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.rt.Settings
	info := s.rt.ASRStatus()

	llmStatus := "not_configured"
	if st.LLMBase != "" && st.LLMModel != "" {
		llmStatus = "ok"
	}

	// 音源与浏览器收音连接状态
	micState := ""
	if bc, ok := s.rt.Capture().(*audio.BrowserCapture); ok {
		if bc.Connected() {
			micState = "connected"
		} else {
			micState = "waiting"
		}
	}

	asrURL := st.WSURL()
	switch st.ASRProtocol {
	case "hy_stream":
		asrURL = st.HyASRWSURL
	case "qwen3_http":
		asrURL = st.Qwen3Backend
	}

	writeJSON(w, 200, map[string]any{
		"asr":          info.State,
		"asr_detail":   info.Detail,
		"asr_idle_sec": info.IdleSec,
		"llm":          llmStatus,
		"llm_model":    st.LLMModel,
		"protocol":     st.ASRProtocol,
		"asr_ws_url":   asrURL,
		"capture":      s.rt.CaptureMode(),
		"mic":          micState,
		"state":        string(s.rt.Display.State()),
	})
}


