package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gen2brain/malgo"

	"seat/internal/agent"
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

	// 「发言」任务专用（task=SPEECH 时生效）：
	Stance   string `json:"stance"`   // 立场（必填）
	Language string `json:"language"` // 语言：中文 / English / 中英双语
	Length   string `json:"length"`   // 长度档位：short / medium / long
	Extra    string `json:"extra"`    // 补充要求（可选）
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
	rt, err := s.reg.Create(body.Title, body.Speaker, body.Institution, body.Discipline, aiEnabled)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 目标学科作用于新会场；未传则回到默认「白话」。
	// 非法值不阻断开会：回退为默认（校验失败原因由 /api/target-discipline 单独提示）。
	_, _ = rt.SetTargetDiscipline(body.TargetDiscipline)

	res, err := rt.initSession(body.Title, body.Speaker, body.Institution, body.Discipline,
		aiEnabled, body.ReplayPath, body.Capture)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.setCurrent(rt)
	writeJSON(w, 200, res)
}

// handleStartSessionCompat 兼容前端「开始 Session」的调用形状：POST /<sid>/api/session。
// 会场已由 URL 确定，这里只启动它的音频流水线；响应保持与会话创建接口相同的形状
// （{session_id, pipeline, ...}），前端无需区分「新建会场」与「启动本会场」。
func (s *Server) handleStartSessionCompat(w http.ResponseWriter, r *http.Request) {
	rt := s.current(r)
	var body sessionIn
	_ = decodeBody(r, &body)
	body.withDefaults()

	// 目标学科作用于本会场（未传则不动）；非法值不阻断开会
	if body.TargetDiscipline != "" {
		_, _ = rt.SetTargetDiscipline(body.TargetDiscipline)
	}
	res, err := rt.StartPipeline("", body.Capture)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"session_id": rt.SessionID(),
		"pipeline":   res,
		"hotwords":   nil,
		"profile":    "",
	})
}

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	var body sessionIn
	_ = decodeBody(r, &body)
	body.withDefaults()

	// 按 sid 取会场（未注册时按 DB 记录恢复，支持刷新/分享链接/重启后回到原会场）
	rt, err := s.reg.Ensure(sid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rt == nil {
		writeErr(w, badRequest("会话不存在：%s", sid))
		return
	}
	s.setCurrent(rt)

	res, err := rt.StartPipeline("", body.Capture)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	rt, err := s.reg.Ensure(sid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rt == nil {
		writeErr(w, badRequest("会话不存在：%s", sid))
		return
	}
	rt.StopPipeline()
	_ = rt.Store.SetEnded(sid)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	segs, err := s.current(r).Store.RecentSegments(sid, 0, 500)
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
	s.current(r).mu.Lock()
	words := s.current(r).sessionHotwords
	if len(words) == 0 {
		words = s.current(r).baseGlossary
	}
	enabled := s.current(r).hotwordsEnabled
	out := copyMap(words)
	s.current(r).mu.Unlock()

	writeJSON(w, 200, map[string]any{"words": out, "enabled": enabled})
}

func (s *Server) handlePutHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordsTextIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	s.current(r).mu.Lock()
	old := copyMap(s.current(r).sessionHotwords)
	s.current(r).mu.Unlock()

	newWords := asr.ParseHotwordsText(body.Text, old)
	s.current(r).setSessionHotwords(newWords)
	s.current(r).pushHotwords()
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(newWords)})
}

func (s *Server) handleToggleHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordsToggleIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	s.current(r).mu.Lock()
	s.current(r).hotwordsEnabled = body.Enabled
	s.current(r).mu.Unlock()
	s.current(r).pushHotwords()
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": body.Enabled})
}

func (s *Server) handleGenerateHotwords(w http.ResponseWriter, r *http.Request) {
	var body hotwordIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	ag := s.current(r).Agent()
	if ag == nil || ag.LLM == nil {
		writeJSON(w, 400, map[string]any{"error": "LLM 未配置"})
		return
	}
	sid := s.current(r).SessionID()
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

	sessionDir := filepath.Join(s.current(r).Settings.DataDirPath(), sid)
	materialsDir := filepath.Join(sessionDir, "materials")
	_ = mkdirAll(materialsDir)

	if len(result.Hotwords) > 0 {
		_ = asr.SaveHotwords(filepath.Join(sessionDir, "hotwords.txt"), result.Hotwords)
	}
	if result.Profile != "" {
		writeFile(filepath.Join(materialsDir, "speaker_profile.md"),
			"# 报告人背景\n\n"+result.Profile+"\n")
		s.current(r).mu.Lock()
		mgr := s.current(r).context
		s.current(r).mu.Unlock()
		if mgr != nil {
			mgr.InvalidateStatic()
		}
	}
	s.current(r).setSessionHotwords(result.Hotwords)

	applied := "asr_reconnect"
	s.current(r).mu.Lock()
	hasClient := s.current(r).asrClient != nil
	s.current(r).mu.Unlock()
	if hasClient {
		s.current(r).pushHotwords()
	} else {
		if _, err := s.current(r).StartPipeline("", "mic"); err == nil {
			applied = "pipeline_start"
		}
	}
	s.current(r).Bus.Publish(events.Event{
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
	ag := s.current(r).Agent()
	if ag == nil || ag.LLM == nil {
		writeJSON(w, 400, map[string]any{"error": "LLM 未配置"})
		return
	}
	sid := s.current(r).SessionID()
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
	if err := contextx.CheckUploadSize(len(data), s.current(r).Settings.DocMaxBytes); err != nil {
		writeJSON(w, 413, map[string]any{"error": err.Error()})
		return
	}
	// 只取 basename，防止用 ../ 之类构造路径穿越
	filename := filepath.Base(header.Filename)
	if filename == "" || filename == "." {
		filename = "document"
	}

	// 解析超时可配（DOC_PARSE_TIMEOUT，默认 180s）；外层 ctx 必须大于 解析 + LLM 之和
	parseTimeout := time.Duration(s.current(r).Settings.DocParseTimeout * float64(time.Second))
	if parseTimeout <= 0 {
		parseTimeout = 180 * time.Second
	}
	ctx, cancel := contextWithTimeout(parseTimeout + 180*time.Second)
	defer cancel()

	// 体积与耗时都打日志：解析失败时这两个数字是判断「服务慢」还是「网络不通」的唯一依据
	log.Printf("[upload] 解析讲稿：%s（%.1f MB，超时 %.0fs）", filename, float64(len(data))/(1<<20), parseTimeout.Seconds())
	parseStart := time.Now()
	doc, err := contextx.ParseDocument(ctx, s.current(r).Settings.DocParseURL, filename, data, parseTimeout)
	if err != nil {
		log.Printf("[upload] 解析失败（已等 %.1fs）：%v", time.Since(parseStart).Seconds(), err)
		writeJSON(w, 502, map[string]any{"error": "文档解析失败：" + err.Error()})
		return
	}
	log.Printf("[upload] 解析完成：正文 %d 字，耗时 %.1fs", len([]rune(doc.Content)), time.Since(parseStart).Seconds())

	profile, err := contextx.BuildFromDocument(ctx, ag.LLM, doc.Content, s.current(r).Settings.DocDigestChars)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "热词抽取失败：" + err.Error()})
		return
	}

	sessionDir := filepath.Join(s.current(r).Settings.DataDirPath(), sid)
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
	merged := s.current(r).sessionHotwordsOnly()
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
	s.current(r).setSessionHotwords(merged)

	// 摘要改变了 STATIC 上下文，让上下文缓存失效
	s.current(r).mu.Lock()
	mgr := s.current(r).context
	s.current(r).mu.Unlock()
	if mgr != nil {
		mgr.InvalidateStatic()
	}

	applied := "asr_reconnect"
	s.current(r).mu.Lock()
	hasClient := s.current(r).asrClient != nil
	s.current(r).mu.Unlock()
	if hasClient {
		s.current(r).pushHotwords()
	} else {
		if _, err := s.current(r).StartPipeline("", "mic"); err == nil {
			applied = "pipeline_start"
		}
	}
	s.current(r).Bus.Publish(events.Event{
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
	sid := s.current(r).SessionID()
	if sid == "" {
		writeJSON(w, 200, map[string]any{"items": []any{}})
		return
	}
	materialsDir := filepath.Join(s.current(r).Settings.DataDirPath(), sid, "materials")
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
	ag := s.current(r).Agent()
	sid := s.current(r).SessionID()
	if ag == nil || sid == "" {
		writeJSON(w, 400, map[string]any{"error": "请先创建 session"})
		return
	}
	focus, err := s.current(r).Store.RecentSegments(sid, 60, 0)
	if err != nil {
		focus = nil
	}
	// 可取消的生成 context：注册到状态机后，急停/丢弃能立刻中断本次生成
	// （含 hy3 只流思考内容、没有正文 delta 的阶段）。
	ctx, cancel := context.WithCancel(r.Context())
	s.current(r).Display.SetCancel(cancel)
	defer func() {
		s.current(r).Display.SetCancel(nil)
		cancel()
	}()

	// 「发言」：按立场 + 会议上下文起草发言稿（与 5 个翻译任务共用生成链路，
	// 前端据事件里的 task=SPEECH 把结果渲染到右下角的发言稿区）
	var iid string
	if body.Task == agent.TaskSpeech {
		iid, err = ag.GenerateSpeech(ctx, sid, agent.SpeechOptions{
			Stance: body.Stance, Language: body.Language, Length: body.Length, Extra: body.Extra,
		}, focus, s.current(r).TargetDiscipline())
	} else {
		iid, err = ag.Generate(ctx, sid, body.Task, body.Target, focus, s.current(r).TargetDiscipline())
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"invocation_id": iid})
}

// handleSpeechOptions 发言可选的语言/长度档位。
// 由后端下发而不是前端写死：字数窗口同时被校验函数使用，两处必须一致，
// 否则会出现「选了 2 分钟但模型按 30 秒校验」这类难查的偏差。
func (s *Server) handleSpeechOptions(w http.ResponseWriter, r *http.Request) {
	lengths := make([]map[string]any, 0, len(agent.SpeechLengths))
	for _, l := range agent.SpeechLengths {
		lengths = append(lengths, map[string]any{
			"key": l.Key, "label": l.Label, "seconds": l.Seconds,
			"min_chars": l.MinChars, "max_chars": l.MaxChars,
		})
	}
	writeJSON(w, 200, map[string]any{
		"languages":      agent.SpeechLanguages,
		"lengths":        lengths,
		"default_length": "medium",
	})
}

// handleSpeakers 本场已识别出的说话人（说话人区分结果，按句子聚合）。
func (s *Server) handleSpeakers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"speakers": s.current(r).Speakers()})
}

// handleGetTargetDiscipline 当前目标学科（页面加载时回填下拉框）。
func (s *Server) handleGetTargetDiscipline(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"discipline": s.current(r).TargetDiscipline()})
}

// handleSetTargetDiscipline 设置目标学科（下一次 AI 调用即生效，无需重启 session）。
func (s *Server) handleSetTargetDiscipline(w http.ResponseWriter, r *http.Request) {
	var body targetDisciplineIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	v, err := s.current(r).SetTargetDiscipline(body.Discipline)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "discipline": v})
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	var body showIn
	_ = decodeBody(r, &body)

	s.current(r).Display.Show()
	text := ""
	if strings.TrimSpace(body.Text) != "" {
		text = strings.TrimSpace(body.Text)
		_ = s.current(r).Store.ReviseInvocation(iid, text) // 编辑版落库（复盘导出用编辑后文本）
	}
	if text == "" {
		if inv, _ := s.current(r).Store.GetInvocation(iid); inv != nil {
			text = inv.OutputText
		}
	}
	task := ""
	if inv, _ := s.current(r).Store.GetInvocation(iid); inv != nil {
		task = inv.Task
	}
	_ = s.current(r).Store.SetStatus(iid, "shown", 0)
	s.current(r).Bus.Publish(events.Event{
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
	_ = s.current(r).Store.ReviseInvocation(iid, body.Text)
	// 编辑 AI 输出卡；若该卡正在投屏，大屏实时同步更新
	if s.current(r).Display.State() == "SHOWING" && s.current(r).Display.Current() == iid {
		task := ""
		if inv, _ := s.current(r).Store.GetInvocation(iid); inv != nil {
			task = inv.Task
		}
		s.current(r).Bus.Publish(events.Event{
			"type": "AI_SHOWING", "invocation_id": iid, "text": body.Text, "task": task,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	iid := r.PathValue("iid")
	// 丢弃语义 = 不要这次输出：若它还在生成，一并中止（否则十几秒后 AI_READY 又会把卡片放回来，
	// 等于丢弃被撤销）。这里只置中止标记，随后用 Clear() 清展示状态、保留标记给在飞的生成自行退出。
	if s.current(r).Display.State() == state.GENERATING && s.current(r).Display.Current() == iid {
		s.current(r).Display.Kill()
	}
	s.current(r).Display.Clear()
	_ = s.current(r).Store.SetStatus(iid, "discarded", 0)
	s.current(r).Bus.Publish(events.Event{"type": "AI_STATE", "state": "IDLE", "invocation_id": iid})
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
	_ = s.current(r).Store.Confirm(iid, body.By, body.Verdict, body.Note)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	s.current(r).Display.Kill()
	// 先发事件（此刻 current 还有值，大屏据此下屏），再清展示状态。
	// 注意用 Clear() 而非 Reset()：Reset() 会清掉急停标记，那样正在飞的生成就不会中止了。
	s.current(r).Bus.Publish(events.Event{"type": "AI_KILLED", "invocation_id": s.current(r).Display.Current()})
	s.current(r).Display.Clear()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleReviseSegment(w http.ResponseWriter, r *http.Request) {
	segID := r.PathValue("segID")
	var body reviseIn
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if err := s.current(r).Store.ReviseSegment(segID, body.Text); err != nil {
		writeErr(w, serverError("修订失败: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.current(r).Settings
	info := s.current(r).ASRStatus()

	llmStatus := "not_configured"
	if st.LLMBase != "" && st.LLMModel != "" {
		llmStatus = "ok"
	}

	// 音源与浏览器收音连接状态
	micState := ""
	if bc, ok := s.current(r).Capture().(*audio.BrowserCapture); ok {
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

	// 说话人区分状态（off/ok/offline + 计数），供状态条显示
	diarState, diarDetail := s.current(r).DiarizeStatus()

	writeJSON(w, 200, map[string]any{
		"asr":          info.State,
		"asr_detail":   info.Detail,
		"asr_idle_sec": info.IdleSec,
		// 连续推理失败次数：用来区分「确实没语音」与「有语音但推理一直失败」——
		// 没有这个字段时两者都只表现为 asr_idle_sec 不断增长，从外部无法分辨。
		"asr_infer_fail": info.InferFail,
		"llm":            llmStatus,
		"llm_model":      st.LLMModel,
		"protocol":       st.ASRProtocol,
		"asr_ws_url":     asrURL,
		"capture":        s.current(r).CaptureMode(),
		"mic":            micState,
		"state":          string(s.current(r).Display.State()),
		"diarize":        diarState,
		"diarize_detail": diarDetail,
	})
}
