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
	promptHash := hashPrompt(messages)

	iid := fmt.Sprintf("iv%06d", time.Now().UnixMilli()%1000000)
	r.Display.BeginGenerate(iid, taskKey)
	r.Bus.Publish(events.Event{
		"type": "AI_STATE", "state": "GENERATING", "task": taskKey, "invocation_id": iid,
	})

	t0 := time.Now()
	var parts []string

	if r.LLM != nil {
		err := r.LLM.StreamChat(ctx, messages, r.Settings.LLMMaxTokens, r.Settings.LLMTemperature,
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
		full := MockOutput(taskKey, target)
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
	result := Validate(raw, task.MinChars, task.MaxChars)
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
		"type": "AI_READY",
		"invocation_id": iid,
		"text":          result.Text,
		"chars":         len([]rune(result.Text)),
		"est_sec":       round1(EstimateSecs(result.Text)),
		"checks":        result.Checks,
	})
	// 落库
	if _, err := r.Store.AddInvocation(sid, taskKey, target, promptHash, result.Text,
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

func hashPrompt(messages []llm.Message) string {
	b, err := json.Marshal(messages)
	if err != nil {
		return "00000000"
	}
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])[:8]
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
