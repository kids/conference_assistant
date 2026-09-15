package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gen2brain/malgo"

	"seat/internal/asr"
	"seat/internal/audio"
	"seat/internal/contextx"
	"seat/internal/events"
	"seat/internal/state"
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
	// TargetDiscipline 目标学科（把 AI 输出翻译到该学科的表达层次），默认「白话」
	TargetDiscipline string `json:"target_discipline"`
}

type targetDisciplineIn struct {
	Discipline string `json:"discipline"`
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
	// 新 session 从表单取值；未传则回到默认「白话」
	s.rt.SetTargetDiscipline(body.TargetDiscipline)

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

// ---- 上传讲稿文档：解析 → 一次 LLM 调用产出热词 + 上下文摘要 ----

// maxUploadBytes 上传上限。演示稿通常几 MB，32MB 足够且能挡住误传的大文件。
const maxUploadBytes = 32 << 20

// handleUploadMaterial 接收演示稿/文档，交 /parse_doc 解析后由 LLM 一次调用同时
// 产出「热词」和「浓缩摘要」：
//   - 热词 → 合并进 session 热词，即时推给 ASR（下一次识别即生效）；
//   - 摘要 → 落成 materials/<名>.md，LoadMaterials 会自动读它作为每次 AI 调用的
//     STATIC 上下文（因此上传讲稿后，所有 AI 翻译都能引用讲稿内容）。
//
// 为什么存摘要而不是解析原文：幻灯片解析出来是碎片化的，信噪比低；每次调用都带
// 全文会拉长 hy3 的思考时间，而实时翻译对延迟敏感。原文另存 uploads/ 仅供追溯。
func (s *Server) handleUploadMaterial(w http.ResponseWriter, r *http.Request) {
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

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "未收到文件（表单字段名应为 file）: " + err.Error()})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "读取上传文件失败: " + err.Error()})
		return
	}
	if len(data) == 0 {
		writeJSON(w, 400, map[string]any{"error": "文件为空"})
		return
	}
	// 体积预检：网关对请求体有 2 MiB 上限，超出会被静默截断，
	// 上游只回一个看不懂的 multipart 解析错误。这里提前拦住。
	if err := contextx.CheckUploadSize(len(data), s.rt.Settings.DocMaxBytes); err != nil {
		writeJSON(w, 413, map[string]any{"error": err.Error()})
		return
	}
	// 只取 basename，防止用 ../ 之类构造路径穿越
	filename := filepath.Base(header.Filename)
	if filename == "" || filename == "." {
		filename = "document"
	}

	// 上限须大于 解析(120s) + LLM(120s) 之和
	ctx, cancel := contextWithTimeout(300 * time.Second)
	defer cancel()

	doc, err := contextx.ParseDocument(ctx, s.rt.Settings.DocParseURL, filename, data)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "文档解析失败：" + err.Error()})
		return
	}

	profile, err := contextx.BuildFromDocument(ctx, ag.LLM, doc.Content, s.rt.Settings.DocDigestChars)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "热词抽取失败：" + err.Error()})
		return
	}

	sessionDir := filepath.Join(s.rt.Settings.DataDirPath(), sid)
	materialsDir := filepath.Join(sessionDir, "materials")
	if err := mkdirAll(materialsDir); err != nil {
		writeJSON(w, 500, map[string]any{"error": "创建资料目录失败: " + err.Error()})
		return
	}
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	if stem == "" {
		stem = "document"
	}

	if profile.Digest != "" {
		writeFile(filepath.Join(materialsDir, stem+".md"),
			"# 讲稿摘要："+stem+"\n\n"+profile.Digest+"\n")
	}
	// 解析原文单独放 uploads/：不在 materials/ 下，因此不会进 AI 上下文
	writeFile(filepath.Join(sessionDir, "uploads", stem+".txt"), doc.Content)

	// 热词与已有 session 热词合并（可连续上传多份资料），同名词以新权重覆盖
	merged := s.rt.sessionHotwordsOnly()
	if merged == nil {
		merged = map[string]int{}
	}
	added := 0
	for word, weight := range profile.Hotwords {
		if _, exists := merged[word]; !exists {
			added++
		}
		merged[word] = weight
	}
	if len(profile.Hotwords) > 0 {
		_ = asr.SaveHotwords(filepath.Join(sessionDir, "hotwords.txt"), merged)
	}
	s.rt.setSessionHotwords(merged)

	// 摘要改变了 STATIC 上下文，让上下文缓存失效
	s.rt.mu.Lock()
	mgr := s.rt.context
	s.rt.mu.Unlock()
	if mgr != nil {
		mgr.InvalidateStatic()
	}

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
		"type": "HOTWORDS_STATUS", "status": "generated:" + strconv.Itoa(len(profile.Hotwords)),
	})

	writeJSON(w, 200, map[string]any{
		"ok":             true,
		"filename":       doc.Filename,
		"parsed_chars":   len([]rune(doc.Content)),
		"truncated":      doc.Truncated,
		"digest_chars":   len([]rune(profile.Digest)),
		"fields":         profile.Fields,
		"hotwords":       len(profile.Hotwords),
		"hotwords_new":   added,
		"hotwords_total": len(merged),
		"applied":        applied,
	})
}

// handleListMaterials 列出当前 session 已加载的资料（materials/ 目录）。
func (s *Server) handleListMaterials(w http.ResponseWriter, r *http.Request) {
	sid := s.rt.SessionID()
	if sid == "" {
		writeJSON(w, 200, map[string]any{"items": []any{}})
		return
	}
	materialsDir := filepath.Join(s.rt.Settings.DataDirPath(), sid, "materials")
	entries, err := os.ReadDir(materialsDir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"items": []any{}})
		return
	}
	type item struct {
		Name  string `json:"name"`
		Chars int    `json:"chars"`
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		chars := 0
		if info, err := e.Info(); err == nil {
			chars = int(info.Size())
		}
		items = append(items, item{Name: e.Name(), Chars: chars})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeJSON(w, 200, map[string]any{"items": items})
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
	// 可取消的生成 context：注册到状态机后，急停/丢弃能立刻中断本次生成
	// （含 hy3 只流思考内容、没有正文 delta 的阶段）。
	ctx, cancel := context.WithCancel(r.Context())
	s.rt.Display.SetCancel(cancel)
	defer func() {
		s.rt.Display.SetCancel(nil)
		cancel()
	}()

	iid, err := ag.Generate(ctx, sid, body.Task, body.Target, focus, s.rt.TargetDiscipline())
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"invocation_id": iid})
}

// handleGetTargetDiscipline 当前目标学科（页面加载时回填下拉框）。
func (s *Server) handleGetTargetDiscipline(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"discipline": s.rt.TargetDiscipline()})
}

// handleSetTargetDiscipline 设置目标学科（下一次 AI 调用即生效，无需重启 session）。
func (s *Server) handleSetTargetDiscipline(w http.ResponseWriter, r *http.Request) {
	var body targetDisciplineIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "discipline": s.rt.SetTargetDiscipline(body.Discipline)})
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
	// 丢弃语义 = 不要这次输出：若它还在生成，一并中止（否则十几秒后 AI_READY 又会把卡片放回来，
	// 等于丢弃被撤销）。这里只置中止标记，随后用 Clear() 清展示状态、保留标记给在飞的生成自行退出。
	if s.rt.Display.State() == state.GENERATING && s.rt.Display.Current() == iid {
		s.rt.Display.Kill()
	}
	s.rt.Display.Clear()
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
	// 先发事件（此刻 current 还有值，大屏据此下屏），再清展示状态。
	// 注意用 Clear() 而非 Reset()：Reset() 会清掉急停标记，那样正在飞的生成就不会中止了。
	s.rt.Bus.Publish(events.Event{"type": "AI_KILLED", "invocation_id": s.rt.Display.Current()})
	s.rt.Display.Clear()
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


