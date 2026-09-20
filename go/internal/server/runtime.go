// Package server HTTP/WebSocket 装配与运行时。
package server

import (
	"fmt"
	"log"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"seat/internal/agent"
	"seat/internal/asr"
	"seat/internal/audio"
	"seat/internal/config"
	"seat/internal/contextx"
	"seat/internal/diarize"
	"seat/internal/events"
	"seat/internal/state"
	"seat/internal/store"
)

// httpError 带状态码的业务错误。
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &httpError{code: 400, msg: fmt.Sprintf(format, a...)}
}

func serverError(format string, a ...any) error {
	return &httpError{code: 500, msg: fmt.Sprintf(format, a...)}
}

// Runtime 进程级单例运行时。
type Runtime struct {
	Settings *config.Settings
	Store    *store.Store
	Bus      *events.Bus
	Display  *state.Display

	mu              sync.Mutex
	sessionID       string
	context         *contextx.Manager
	agent           *agent.Runner
	pipeline        *audio.Pipeline
	capture         audio.Capture
	captureMode     string // mic（本机声卡）/ browser（远端浏览器收音）
	asrClient       asr.Client
	ring            *audio.RingBuffer
	baseGlossary    map[string]int
	glossary        map[string]int
	sessionHotwords map[string]int
	hotwordsEnabled bool
	segCounter      int
	roundSegs       map[string]string // 句键（r{轮次}i{序号}）→ seg_id
	roundKeys       []string          // 插入顺序，用于裁剪
	refiner         *agent.Refiner
	// 目标学科：把 AI 输出调整到该学科的表达层次。默认「白话」=非专业听众也能听懂。
	// 与 session.discipline（报告人学科，仅用于热词生成）是两个不同概念。
	targetDiscipline string

	// 说话人区分（CAM++ sidecar）：diarizer 为 nil 表示未启用。
	// spkCh/spkStop 由 startDiarize 懒创建；spk 见 diarize.go。
	diarizer *diarize.Client
	spkCh    chan spanJob
	spkStop  chan struct{}
	spkOnce  sync.Once
	spk      spkState
}

// DefaultTargetDiscipline 默认目标学科。
const DefaultTargetDiscipline = "白话"

// NewRuntime 创建运行时。
func NewRuntime(s *config.Settings) (*Runtime, error) {
	st, err := store.Open(filepath.Join(s.DataDirPath(), "transcript.sqlite"))
	if err != nil {
		return nil, err
	}
	base := asr.LoadHotwords(s.HotwordsFile())
	rt := &Runtime{
		Settings:         s,
		Store:            st,
		Bus:              events.New(800),
		Display:          state.New(),
		captureMode:      "mic",
		ring:             audio.NewRingBuffer(120, s.SampleRate),
		baseGlossary:     base,
		glossary:         copyMap(base),
		sessionHotwords:  map[string]int{},
		hotwordsEnabled:  true,
		roundSegs:        map[string]string{},
		targetDiscipline: DefaultTargetDiscipline,
	}
	if s.DiarizeEnabled {
		rt.diarizer = diarize.New(s.DiarizeURL, s.DiarizeTimeout, s.DiarizeThreshold, s.DiarizeMinMS)
		rt.spkCh = make(chan spanJob, spanQueue)
		rt.spkStop = make(chan struct{})
		log.Printf("[diarize] 说话人区分已启用：%s（阈值 %.2f，最短 %dms，自动拉起 %v）",
			s.DiarizeURL, s.DiarizeThreshold, s.DiarizeMinMS, s.DiarizeAutostart)
		rt.startDiarize()
	}
	// 录音保留期清理：启动时清一次（新建会话时还会再清一次，见 CreateSession）
	go sweepRecFiles(s.DataDirPath(), s.RecKeepDays)
	return rt, nil
}

// sweepRecFiles 异步清理超保留期的录音文件（REC_KEEP_DAYS，0=不清理）。
// 覆盖 sessions/<sid>/rec/*.wav（会话录音分片）与 sessions/<sid>/audio/*.wav（说话人段音频）。
func sweepRecFiles(dataDir string, keepDays int) {
	if n := audio.SweepRecFiles(dataDir, keepDays); n > 0 {
		log.Printf("[rec] 已清理 %d 个超期录音文件（保留 %d 天）", n, keepDays)
	}
}

// TargetDiscipline 当前目标学科。
func (rt *Runtime) TargetDiscipline() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.targetDiscipline == "" {
		return DefaultTargetDiscipline
	}
	return rt.targetDiscipline
}

// SetTargetDiscipline 设置目标学科，返回归一化后的值。下一次 AI 调用即生效。
func (rt *Runtime) SetTargetDiscipline(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		v = DefaultTargetDiscipline
	}
	rt.mu.Lock()
	rt.targetDiscipline = v
	rt.mu.Unlock()
	return v
}

// Close 释放运行时资源。
func (rt *Runtime) Close() {
	rt.StopPipeline()
	if rt.spkStop != nil {
		close(rt.spkStop)
	}
	rt.Store.Close()
	contextx.CloseTerms()
}

// SessionID 当前会话 id。
func (rt *Runtime) SessionID() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.sessionID
}

func (rt *Runtime) setSessionHotwords(words map[string]int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.sessionHotwords = copyMap(words)
	merged := make(map[string]int, len(rt.baseGlossary)+len(words))
	for k, v := range rt.baseGlossary {
		merged[k] = v
	}
	for k, v := range words {
		merged[k] = v
	}
	rt.glossary = merged
}

// sessionHotwordsOnly 当前 session 级热词（不含全局兜底）。
// 用于「上传讲稿」时往上累加：全局词典继续当兜底，不被复制进 session 文件。
func (rt *Runtime) sessionHotwordsOnly() map[string]int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return copyMap(rt.sessionHotwords)
}

// activeHotwords ASR 实际使用的热词：优先 session 级，否则全局；总开关关闭则返回空。
func (rt *Runtime) activeHotwords() map[string]int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.hotwordsEnabled {
		return map[string]int{}
	}
	if len(rt.sessionHotwords) > 0 {
		return copyMap(rt.sessionHotwords)
	}
	return copyMap(rt.baseGlossary)
}

func (rt *Runtime) currentGlossary() map[string]int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return copyMap(rt.glossary)
}

// ApplyRevision 更新某句转写文本：落库 + 推送前端覆盖显示。
func (rt *Runtime) ApplyRevision(segID, text, source string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if err := rt.Store.ReviseSegment(segID, text); err != nil {
		log.Printf("[store] revise_segment failed: %v", err)
	}
	rt.Bus.Publish(events.Event{
		"type": "TRANSCRIPT_REVISED", "seg_id": segID, "text": text, "source": source,
	})
}

// fillerRunes 纯语气词/填充音。ASR 在轻声、远场、迟疑、噪声处常吐出这类极短句
// （「嗯。」「呃。」），单条没有信息量，却会持续堆砌转写区、干扰阅读。
var fillerRunes = map[rune]bool{
	'嗯': true, '呃': true, '哦': true, '噢': true, '唔': true, '唉': true,
	'哎': true, '诶': true, '啊': true, '呀': true, '哈': true, '呵': true,
	'嘿': true, '哼': true, '咦': true, '唷': true, '呦': true, '嘛': true,
}

// isFillerOnly 判断整句是否为纯语气词短句（「嗯。」「呃呃。」）。
// 判定从严：只由语气词（≤4 个）与标点组成才算 —— 句中出现任何实义字
// （「嗯，但是这样不行」）都不算，照常保留。
func isFillerOnly(text string) bool {
	n := 0
	for _, r := range text {
		if fillerRunes[r] {
			n++
			continue
		}
		switch r {
		case '。', '，', '、', '！', '？', '…', '～', '~', ' ', '!', '?', '.', ',':
			// 标点与空白不计
		default:
			return false
		}
	}
	return n > 0 && n <= 4
}

// makeHandlers 构造 ASR 回调。
func (rt *Runtime) makeHandlers() asr.Handlers {
	return asr.Handlers{
		OnOnline: func(text string) {
			if text == "" {
				return
			}
			rt.Bus.Publish(events.Event{"type": "TRANSCRIPT_PARTIAL", "text": text})
		},
		OnOffline: func(key, text string) {
			sid := rt.SessionID()
			if sid == "" {
				return
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return
			}
			if isFillerOnly(text) {
				// 纯语气词短句（「嗯。」等）：不入库、不上屏 —— ASR 在轻音处会持续吐，
				// 堆砌转写区。判定极保守（见 isFillerOnly），带任何实义字都不丢。
				return
			}

			rt.mu.Lock()
			rt.segCounter++
			seq := rt.segCounter
			rt.mu.Unlock()

			tEnd := nowSeconds()
			tStart := tEnd - math.Min(60.0, float64(len([]rune(text)))*0.3)
			segID, err := rt.Store.AddSegment(sid, seq, tStart, tEnd, text)
			if err != nil {
				// 落库失败不炸 ASR 回调协程，转写照常投屏
				log.Printf("[store] add_segment failed: %v", err)
				segID = fmt.Sprintf("%s-s%05d", sid, seq)
			}

			// 记录句子键 → seg_id，供服务端回退修正定位（键含会话轮次，跨轮不串）
			if key != "" {
				rt.mu.Lock()
				rt.roundSegs[key] = segID
				rt.roundKeys = append(rt.roundKeys, key)
				// 只保留最近 200 句映射，避免长会议内存增长
				if len(rt.roundKeys) > 200 {
					for _, k := range rt.roundKeys[:100] {
						delete(rt.roundSegs, k)
					}
					rt.roundKeys = rt.roundKeys[100:]
				}
				rt.mu.Unlock()
			}

			rt.Bus.Publish(events.Event{
				"type": "TRANSCRIPT_FINAL", "seg_id": segID, "text": text,
				"t_start": tStart, "t_end": tEnd,
			})

			// 说话人区分：登记这一句，等 CAM++ 结果到达后按时间对齐回填
			// （先显示字幕、后补说话人标记，不拖慢转写）
			rt.noteSegment(segID, tStart, tEnd)

			glossary := rt.currentGlossary()
			if items := contextx.ScoreText(text, glossary); len(items) > 0 {
				rt.Bus.Publish(events.Event{"type": "TERM_CANDIDATES", "items": items, "seg_id": segID})
			}
			// 异步 LLM 顺句改写（失败/超时静默降级，不影响已显示的原文）
			rt.mu.Lock()
			ref := rt.refiner
			rt.mu.Unlock()
			if ref != nil {
				ref.Submit(segID, text, glossary)
			}
		},
		OnRevise: func(key, text string) {
			rt.mu.Lock()
			segID, ok := rt.roundSegs[key]
			rt.mu.Unlock()
			if ok {
				rt.ApplyRevision(segID, text, "asr")
			}
		},
		OnStatus: func(status string) {
			rt.Bus.Publish(events.Event{"type": "ASR_STATUS", "status": status})
		},
	}
}

// StartPipeline 启动采集→VAD→ASR 流水线。
// captureMode: mic（本机声卡/USB 麦）/ browser（远端浏览器收音）。
func (rt *Runtime) StartPipeline(replayPath, captureMode string) (map[string]any, error) {
	rt.mu.Lock()
	if rt.pipeline != nil && rt.pipeline.Alive() {
		if rt.captureMode == captureMode {
			rt.mu.Unlock()
			return map[string]any{"ok": true, "msg": "already running", "capture": captureMode}, nil
		}
		// 请求切换音源（本机麦 ↔ 浏览器收音）：停掉旧流水线，按新音源重启
		rt.mu.Unlock()
		rt.StopPipeline()
		rt.mu.Lock()
	}
	rt.roundSegs = map[string]string{}
	rt.roundKeys = nil
	rt.mu.Unlock()

	s := rt.Settings
	handlers := rt.makeHandlers()
	hotwords := rt.activeHotwords()
	hotwordList := asr.HotwordList(hotwords)

	var (
		asrClient asr.Client
		serverVAD bool
	)
	switch s.ASRProtocol {
	case "hy_stream":
		// HY-ASR-3-Stream：逐词增量 + 语义 VAD 自动断句（无需本地切句）
		asrClient = asr.NewHyAsrStreamClient(s.HyASRWSURL, s.HyASRToken, s.HyASRModel, hotwordList, handlers)
		serverVAD = true
	case "funasr_nano":
		asrClient = asr.NewFunAsrNanoStreamClient(s.WSURL(), s.ASRLanguage, hotwordList, handlers)
		// 服务端 VAD 断句过粗（实测 10~20s 才出一句），默认改用本地 VAD 主动切句：
		// 句尾静音即发 STOP 让服务端立刻 flush，出字延迟降到约 0.8s
		serverVAD = !s.LocalVADSegment
	case "qwen3_http":
		// 推理超时用 ASRInferTimeout 而非 LLMTimeout：两者合理值不同，
		// 且服务端排队抖动时 ASR 需要更宽的容忍度（实测 0.8s~60s）。
		client, err := asr.NewQwen3AsrHttpClient(s.Qwen3Backend, s.Qwen3Model, s.ASRLanguage,
			hotwordList, s.ASRPartialInterval, s.VADSilenceMS, s.VADAggressiveness,
			float64(s.MaxSegmentS), s.ASRInferTimeout, handlers)
		if err != nil {
			return nil, err
		}
		asrClient = client
		serverVAD = true // 持续透传，切句由客户端自管
	default:
		asrClient = asr.NewFunAsrStreamClient(s.WSURL(), s.ASRMode, s.ChunkSize(),
			asr.ToStreamJSON(hotwords), s.SampleRate, handlers)
		serverVAD = false
	}

	var vad *audio.VadSegmenter
	if !serverVAD {
		v, err := audio.NewVadSegmenter(s.SampleRate, s.FrameMS, s.VADSilenceMS,
			s.VADAggressiveness, s.MaxSegmentS)
		if err != nil {
			return nil, err
		}
		vad = v
	}

	var capture audio.Capture
	note := ""
	mode := captureMode
	switch {
	case replayPath != "":
		c, err := audio.NewFileReplayCapture(replayPath, s.SampleRate, s.FrameMS)
		if err != nil {
			asrClient.Close()
			return map[string]any{"ok": false, "msg": "音频采集失败: " + err.Error()}, nil
		}
		capture = c
	case captureMode == "browser":
		capture = audio.NewBrowserCapture(s.SampleRate, s.FrameMS)
	default:
		c, err := audio.NewMicrophoneCapture(s.SampleRate, s.FrameMS)
		if err != nil {
			// 远端/服务器部署常无声卡：自动降级为浏览器收音
			capture = audio.NewBrowserCapture(s.SampleRate, s.FrameMS)
			mode = "browser"
			note = fmt.Sprintf("本机音频采集失败（%v），已切换为浏览器收音", err)
		} else {
			capture = c
		}
	}

	// LLM 顺句改写器（可关闭；失败静默降级，不影响原始转写）
	var refiner *agent.Refiner
	rt.mu.Lock()
	ag := rt.agent
	rt.mu.Unlock()
	if s.RefineEnabled && ag != nil && ag.LLM != nil {
		refiner = agent.NewRefiner(ag.LLM,
			func(segID, text string) { rt.ApplyRevision(segID, text, "llm") },
			s.RefineMinChars)
		refiner.Start()
	}

	pipeline := audio.NewPipeline(capture, vad, asrClient, rt.ring, serverVAD)

	// 会话录音留存（排查用，默认关闭）：连续写 sessions/<sid>/rec/*.wav。
	// 挂点在流水线帧循环，浏览器收音与本机声卡两种音源都会被录到。
	if s.RecEnabled {
		if sid := rt.SessionID(); sid != "" {
			recDir := filepath.Join(s.DataDirPath(), sid, "rec")
			if rec, err := audio.NewRecorder(recDir, s.RecSegmentSec); err == nil {
				pipeline.SetRecorder(rec)
				log.Printf("[rec] 会话录音留存已开启：%s（每 %d 秒分片，保留 %d 天）",
					recDir, s.RecSegmentSec, s.RecKeepDays)
			} else {
				log.Printf("[rec] 开启录音留存失败：%v", err)
			}
		}
	}

	// 说话人区分：需要「一次连续说话」的音频与边界。
	// serverVAD 模式下本地 VAD 不参与断句，这里另建一个只做监测的 VAD
	// （webrtcvad 开销约 0.08% 实时，不影响音频链路）。
	if rt.diarizer != nil {
		monitor := vad
		if monitor == nil {
			// 段长用 DiarizeMaxSegS 而非 ASR 的 MaxSegmentS：15s 的强切段在多人接话时
			// 会把几个人的声音混进同一段，声纹聚类随之失效（不同人被并成一个编号）。
			if v, err := audio.NewVadSegmenter(s.SampleRate, s.FrameMS, s.VADSilenceMS,
				s.VADAggressiveness, s.DiarizeMaxSegS); err == nil {
				monitor = v
			} else {
				log.Printf("[diarize] 监测 VAD 初始化失败，本场不产出说话人标记: %v", err)
			}
		}
		if monitor != nil {
			pipeline.SetSpanSink(monitor, rt.onSpan)
		}
	}

	rt.mu.Lock()
	rt.capture = capture
	rt.captureMode = mode
	rt.asrClient = asrClient
	rt.pipeline = pipeline
	rt.refiner = refiner
	rt.mu.Unlock()

	pipeline.Start()
	// 预热连接：避免开头几百毫秒的音频因"首次 send 才建连"被丢弃
	asrClient.Connect()

	res := map[string]any{"ok": true, "msg": "started", "capture": mode}
	if note != "" {
		res["note"] = note
	}
	return res, nil
}

// StopPipeline 停止流水线并释放相关资源。
func (rt *Runtime) StopPipeline() {
	rt.mu.Lock()
	pipeline := rt.pipeline
	refiner := rt.refiner
	rt.pipeline = nil
	rt.capture = nil
	rt.captureMode = "mic"
	rt.asrClient = nil
	rt.refiner = nil
	rt.mu.Unlock()

	if pipeline != nil {
		pipeline.Stop()
		select {
		case <-pipeline.Done():
		case <-time.After(5 * time.Second):
			log.Printf("[pipeline] 停止超时，继续后续清理")
		}
	}
	if refiner != nil {
		refiner.Stop()
	}
}

// Capture 当前音源（供 /ws/audio 与健康检查）。
func (rt *Runtime) Capture() audio.Capture {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.capture
}

// ASRStatus 当前 ASR 连接状态。
func (rt *Runtime) ASRStatus() asr.StatusInfo {
	rt.mu.Lock()
	client := rt.asrClient
	rt.mu.Unlock()
	if client == nil {
		return asr.StatusInfo{State: "idle", Detail: "流水线未启动（点「开始 Session」）", IdleSec: -1}
	}
	return client.StatusInfo()
}

// PipelineAlive 流水线是否在运行。
func (rt *Runtime) PipelineAlive() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.pipeline != nil && rt.pipeline.Alive()
}

// CaptureMode 当前收音模式。
func (rt *Runtime) CaptureMode() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.pipeline == nil {
		return ""
	}
	return rt.captureMode
}

// Agent 当前 agent（可能为 nil）。
func (rt *Runtime) Agent() *agent.Runner {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.agent
}

// ---- 业务动作 ----

// CreateSession 新建会话并立即启动流水线（不阻塞等热词生成）。
func (rt *Runtime) CreateSession(title, speaker, institution, discipline string,
	aiEnabled bool, replayPath, captureMode string) (map[string]any, error) {

	sid, err := rt.Store.CreateSession(title, speaker, discipline, aiEnabled, institution)
	if err != nil {
		return nil, serverError("创建会话失败: %v", err)
	}

	sessionDir := filepath.Join(rt.Settings.DataDirPath(), sid)
	materialsDir := filepath.Join(sessionDir, "materials")
	if err := mkdirAll(materialsDir); err != nil {
		return nil, serverError("创建会话目录失败: %v", err)
	}
	// 新建会话时顺手清一遍超期录音（保留期见 REC_KEEP_DAYS）
	go sweepRecFiles(rt.Settings.DataDirPath(), rt.Settings.RecKeepDays)

	// 说话人区分：新 session 从零开始编号（sidecar 里若存着同 id 的旧状态也一并清掉）
	rt.resetDiarize(sid)

	rt.mu.Lock()
	rt.sessionID = sid
	rt.context = contextx.NewManager(rt.Store, materialsDir)
	rt.agent = agent.NewRunner(rt.Settings, rt.context, rt.Store, rt.Display, rt.Bus)
	// 每次点「开始 Session」都重新加载全局热词文件，改动即时生效（无需重启）
	rt.baseGlossary = asr.LoadHotwords(rt.Settings.HotwordsFile())
	rt.sessionHotwords = map[string]int{}
	merged := make(map[string]int, len(rt.baseGlossary))
	for k, v := range rt.baseGlossary {
		merged[k] = v
	}
	rt.glossary = merged
	ag := rt.agent
	rt.mu.Unlock()

	// 立即启动流水线（先用全局热词），不阻塞等热词生成 —— 点开始马上有转写
	res, err := rt.StartPipeline(replayPath, captureMode)
	if err != nil {
		return nil, serverError("启动流水线失败: %v", err)
	}
	rt.Bus.Publish(events.Event{
		"type": "SESSION", "session_id": sid, "title": title,
		"ai_enabled": aiEnabled, "speaker": speaker, "hotwords": 0,
	})

	// 热词后台异步生成：完成后无缝切换 ASR 热词（仅重连 ASR 连接，不中断流水线）
	if speaker != "" && ag != nil && ag.LLM != nil {
		go rt.generateHotwordsBG(sid, sessionDir, materialsDir, speaker, institution, discipline)
	} else {
		status := "skipped:llm_unavailable"
		if speaker == "" {
			status = "skipped:no_speaker"
		}
		rt.Bus.Publish(events.Event{"type": "HOTWORDS_STATUS", "status": status})
	}

	return map[string]any{
		"session_id": sid, "pipeline": res, "hotwords": nil, "profile": "",
	}, nil
}

// generateHotwordsBG 后台生成科学家专属热词；完成后应用并触发 ASR 重连使热词即时生效。
func (rt *Runtime) generateHotwordsBG(sid, sessionDir, materialsDir, speaker, institution, discipline string) {
	// 兜住后台协程 panic，热词生成失败不应影响正在进行的会议
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[hotwords] 后台生成 panic 已兜住: %v", r)
			rt.Bus.Publish(events.Event{
				"type": "HOTWORDS_STATUS", "status": fmt.Sprintf("failed:%v", r), "speaker": speaker,
			})
		}
	}()

	rt.Bus.Publish(events.Event{"type": "HOTWORDS_STATUS", "status": "generating", "speaker": speaker})

	ag := rt.Agent()
	if ag == nil || ag.LLM == nil {
		return
	}
	// 上限须大于单次 LLM 调用的超时（热词生成按调用给 90s），否则会被父 context 提前掐断
	ctx, cancel := contextWithTimeout(150 * time.Second)
	defer cancel()
	result, err := contextx.BuildSpeakerProfile(ctx, ag.LLM, speaker, institution, discipline)
	if err != nil {
		// 生成失败降级，不影响开会
		rt.Bus.Publish(events.Event{"type": "HOTWORDS_STATUS", "status": "failed:" + err.Error(), "speaker": speaker})
		return
	}

	// 用户已切换到新 session：丢弃结果，避免热词串场
	if rt.SessionID() != sid {
		return
	}

	if len(result.Hotwords) > 0 {
		if err := asr.SaveHotwords(filepath.Join(sessionDir, "hotwords.txt"), result.Hotwords); err != nil {
			log.Printf("[hotwords] 保存失败: %v", err)
		}
	}
	if result.Profile != "" {
		writeFile(filepath.Join(materialsDir, "speaker_profile.md"),
			"# 报告人背景\n\n"+result.Profile+"\n")
		// 让 STATIC 上下文重新加载，报告人背景才能进入 LLM 提示词
		rt.mu.Lock()
		mgr := rt.context
		rt.mu.Unlock()
		if mgr != nil {
			mgr.InvalidateStatic()
		}
	}
	rt.setSessionHotwords(result.Hotwords)
	rt.Bus.Publish(events.Event{
		"type": "HOTWORDS_STATUS", "status": fmt.Sprintf("generated:%d", len(result.Hotwords)),
		"speaker": speaker,
	})
	rt.pushHotwords()
}

// pushHotwords 把当前生效热词推送到运行中的 ASR 客户端（即时生效，不中断流水线）。
func (rt *Runtime) pushHotwords() {
	rt.mu.Lock()
	client := rt.asrClient
	rt.mu.Unlock()
	if client != nil {
		client.UpdateHotwords(rt.activeHotwords())
	}
}

func copyMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
