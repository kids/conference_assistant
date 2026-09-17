package agent

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"seat/internal/config"
	"seat/internal/contextx"
	"seat/internal/events"
	"seat/internal/llm"
	"seat/internal/state"
	"seat/internal/store"
)

// Runner 智能体编排：组装 prompt → 流式生成 → 后置校验 → 发布事件。
type Runner struct {
	Settings *config.Settings
	Context  *contextx.Manager
	LLM      *llm.Client
	Display  *state.Display
	Bus      *events.Bus
	Store    *store.Store
}

// NewRunner 创建编排器。LLM 未配置时 LLM 为 nil（走占位输出）。
func NewRunner(s *config.Settings, ctx *contextx.Manager, st *store.Store,
	d *state.Display, b *events.Bus) *Runner {
	r := &Runner{Settings: s, Context: ctx, Display: d, Bus: b, Store: st}
	if s.LLMBase != "" && s.LLMModel != "" {
		r.LLM = llm.New(s.LLMBase, s.LLMKey, s.LLMModel, s.LLMTimeout, s.LLMChatPath)
	}
	return r
}

// Generate 同步生成，返回 invocation_id；内部发布 AI_STATE/AI_DELTA/AI_READY 事件。
// targetDiscipline：把输出调整到该学科的表达层次（空值按「白话」处理）。
func (r *Runner) Generate(ctx context.Context, sid, taskKey, target string,
	focusSegs []store.Segment, targetDiscipline string) (string, error) {
	task, ok := Tasks[taskKey]
	if !ok {
		return "", fmt.Errorf("未知任务: %s", taskKey)
	}
	ctxData := r.Context.Build(sid, focusSegs)
	messages := buildMessages(task, target, ctxData, targetDiscipline)
	return r.run(ctx, runSpec{
		sid: sid, taskKey: taskKey, target: target, messages: messages,
		minChars: task.MinChars, maxChars: task.MaxChars,
		maxSecs: 30.0, cps: 4.5,
	})
}

// GenerateSpeech 「发言」：按立场 + 会议上下文起草发言稿。
// 与 Generate 共用同一条流式/事件/落库链路，差别只在提示词、校验与 token 预算
// （发言稿更长，见 config.SpeechMaxTokens）。
func (r *Runner) GenerateSpeech(ctx context.Context, sid string, opts SpeechOptions,
	focusSegs []store.Segment, targetDiscipline string) (string, error) {
	opts = opts.Normalize()
	if opts.Stance == "" {
		return "", fmt.Errorf("请先填写立场")
	}
	minChars, maxChars, seconds := SpeechWindow(opts.Length, opts.Language)
	messages := buildSpeechMessages(opts, r.Context.Build(sid, focusSegs), targetDiscipline,
		minChars, maxChars, seconds)
	return r.run(ctx, runSpec{
		sid: sid, taskKey: TaskSpeech, target: opts.Stance, messages: messages,
		minChars: minChars, maxChars: maxChars,
		// 时长上限留 35% 余量：模型偶尔写长一点，只要不显著超时就让操作员自己删
		maxSecs: float64(seconds) * 1.35, cps: SpeechCPS(opts.Language), speech: true,
	})
}

// runSpec 一次生成的全部输入。
type runSpec struct {
	sid      string
	taskKey  string
	target   string
	messages []llm.Message
	minChars int
	maxChars int
	maxSecs  float64 // 口播时长上限（秒）
	cps      float64 // 口播速度（rune/秒），用于时长换算
	speech   bool    // true：走发言稿校验（不做评价词/确认句/禁换行约束）
}

// run 通用生成流程：状态机 → 流式生成 → 后置校验 → 事件 → 落库。
func (r *Runner) run(ctx context.Context, sp runSpec) (string, error) {
	promptHash := hashPrompt(sp.messages)

	iid := fmt.Sprintf("iv%06d", time.Now().UnixMilli()%1000000)
	r.Display.BeginGenerate(iid, sp.taskKey)
	r.Bus.Publish(events.Event{
		"type": "AI_STATE", "state": "GENERATING", "task": sp.taskKey, "invocation_id": iid,
	})

	maxTokens := r.Settings.LLMMaxTokens
	if sp.speech && r.Settings.SpeechMaxTokens > 0 {
		maxTokens = r.Settings.SpeechMaxTokens
	}

	t0 := time.Now()
	var parts []string

	if r.LLM != nil {
		err := r.LLM.StreamChat(ctx, sp.messages, maxTokens, r.Settings.LLMTemperature,
			func(delta string) error {
				if r.Display.Killed() {
					return llm.ErrAborted
				}
				parts = append(parts, delta)
				r.Bus.Publish(events.Event{"type": "AI_DELTA", "invocation_id": iid, "delta": delta})
				return nil
			})
		if err != nil && !errors.Is(err, llm.ErrAborted) {
			r.Bus.Publish(events.Event{
				"type": "AI_STATE", "state": "IDLE", "invocation_id": iid, "error": err.Error(),
			})
			r.Display.Reset()
			return "", err
		}
	} else {
		// 未配置 LLM：模拟流式输出占位文本
		full := MockOutput(sp.taskKey, sp.target)
		for _, ch := range full {
			if r.Display.Killed() {
				break
			}
			parts = append(parts, string(ch))
			time.Sleep(15 * time.Millisecond)
			r.Bus.Publish(events.Event{"type": "AI_DELTA", "invocation_id": iid, "delta": string(ch)})
		}
	}

	if r.Display.Killed() {
		r.Bus.Publish(events.Event{"type": "AI_KILLED", "invocation_id": iid})
		r.Display.Reset()
		return iid, nil
	}

	raw := strings.TrimSpace(strings.Join(parts, ""))
	var result Result
	if sp.speech {
		result = ValidateSpeech(raw, sp.minChars, sp.maxChars, sp.maxSecs, sp.cps)
	} else {
		result = Validate(raw, sp.minChars, sp.maxChars)
	}
	genMS := float64(time.Since(t0).Microseconds()) / 1000.0

	if !result.OK {
		var failed []string
		for k, v := range result.Checks {
			if v != "ok" {
				failed = append(failed, k+":"+v)
			}
		}
		hint := "校验未通过: " + strings.Join(failed, ", ")
		ev := events.Event{
			"type": "AI_STATE", "state": "IDLE", "invocation_id": iid, "error": hint,
			"task": sp.taskKey,
		}
		if raw == "" {
			// 正文为空：思考模型的 reasoning 可能吃光了 token 预算
			ev["error"] = hint + "（模型未返回正文，可提高 LLM_MAX_TOKENS 后重试）"
			ev["raw_text"] = ""
		} else {
			// 保留原文供操作员人工判断，避免白屏无从下手
			ev["raw_text"] = raw
		}
		r.Bus.Publish(ev)
		r.Display.Reset()
		return iid, nil
	}

	r.Display.Ready()
	r.Bus.Publish(events.Event{
		"type":          "AI_READY",
		"invocation_id": iid,
		"text":          result.Text,
		"chars":         len([]rune(result.Text)),
		"est_sec":       round1(EstimateSecs(result.Text)),
		"checks":        result.Checks,
		// task 供前端把「发言稿」路由到右下展示区（而非 AI 输出卡）
		"task": sp.taskKey,
	})
	// 落库
	if _, err := r.Store.AddInvocation(sp.sid, sp.taskKey, sp.target, promptHash, result.Text,
		store.ChecksJSON(result.Checks), genMS, iid); err != nil {
		return iid, fmt.Errorf("落库失败: %w", err)
	}
	return iid, nil
}

// PlainLanguage 默认目标学科：白话（非专业听众也能听懂）。
const PlainLanguage = "白话"

// targetDisciplineBlock 生成「翻译目标学科」约束块。
// 与 session.discipline（报告人学科）不同 —— 这里指的是「把内容翻译成给谁看」。
func targetDisciplineBlock(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		d = PlainLanguage
	}
	if d == PlainLanguage {
		return "【翻译目标学科】白话 —— 用非专业听众也能听懂的日常语言表达，避免堆砌专业术语。"
	}
	return "【翻译目标学科】" + d + " —— 请把输出调整到该学科的研究者能直接理解、能据此提问的表达层次。"
}

func buildMessages(task Task, target string, c contextx.Context, targetDiscipline string) []llm.Message {
	orNone := func(s string) string {
		if s == "" {
			return "（无）"
		}
		return s
	}
	userParts := []string{
		"【会议材料（摘要/PPT/术语表）】\n" + orNone(c.Static),
		"【最近 30 分钟转写】\n" + orNone(c.Recent),
	}
	if c.Focus != "" {
		userParts = append(userParts, "【当前焦点（主持人框选/最近 60 秒）】\n"+c.Focus)
	}
	if c.History != "" {
		userParts = append(userParts, "【本场已展示过的 AI 输出（避免重复）】\n"+c.History)
	}
	if target != "" {
		hint := task.TargetHint
		if hint == "" {
			hint = "目标"
		}
		userParts = append(userParts, "【"+hint+"】\n"+target)
	}
	userParts = append(userParts, targetDisciplineBlock(targetDiscipline))
	userParts = append(userParts, "【任务】\n"+task.Instruction)

	return []llm.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: strings.Join(userParts, "\n\n")},
	}
}

// buildSpeechMessages 组装「发言」任务的提示词。
// 上下文沿用同一份四块（STATIC/RECENT/FOCUS/HISTORY）+ 目标学科约束。
func buildSpeechMessages(opts SpeechOptions, c contextx.Context, targetDiscipline string,
	minChars, maxChars, seconds int) []llm.Message {
	orNone := func(s string) string {
		if s == "" {
			return "（无）"
		}
		return s
	}
	userParts := []string{
		"【会议材料（摘要/PPT/术语表）】\n" + orNone(c.Static),
		"【最近 30 分钟转写】\n" + orNone(c.Recent),
	}
	if c.Focus != "" {
		userParts = append(userParts, "【当前焦点（最近 60 秒）】\n"+c.Focus)
	}
	if c.History != "" {
		// 避免与已展示内容重复：发言稿同样不应复述刚讲过的 AI 输出
		userParts = append(userParts, "【本场已展示过的 AI 输出（避免重复）】\n"+c.History)
	}
	userParts = append(userParts, "【发言者立场】\n"+opts.Stance)
	if opts.Extra != "" {
		userParts = append(userParts, "【补充要求】\n"+opts.Extra)
	}
	userParts = append(userParts, targetDisciplineBlock(targetDiscipline))
	userParts = append(userParts, "【任务】\n"+SpeechInstruction(opts.Language, seconds, minChars, maxChars))

	return []llm.Message{
		{Role: "system", Content: SpeechSystemPrompt},
		{Role: "user", Content: strings.Join(userParts, "\n\n")},
	}
}

func hashPrompt(messages []llm.Message) string {
	b, err := json.Marshal(messages)
	if err != nil {
		return "00000000"
	}
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])[:8]
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
