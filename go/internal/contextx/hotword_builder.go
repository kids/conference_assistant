package contextx

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"seat/internal/llm"
)

// SpeakerProfile 科学家研究背景与专属热词。
type SpeakerProfile struct {
	Profile  string
	Fields   []string
	Hotwords map[string]int
}

const buildPrompt = `你是学术会议支持助手。请根据以下科学家信息，基于你的知识库，整理其研究背景并提取相关热词。

科学家姓名：{{NAME}}
机构：{{INSTITUTION}}
学科领域：{{DISCIPLINE}}

请严格输出一个 JSON 对象（不要输出任何其他文字、解释或 Markdown 代码块标记），格式如下：
{"profile":"150字以内的研究背景简介，概括其研究领域、核心方向与代表工作","fields":["研究领域1","研究领域2"],"hotwords":[{"word":"专业术语或方法名","weight":60}]}

要求：
1. hotwords 为这位科学家研究领域的核心专业术语、方法名、模型名、算法名、缩写、专有名词等，10~25 个。
2. weight 为 1~100 的整数权重：核心术语 60~100，一般术语 30~59。
3. 术语要具体、有辨识度，避免"研究""方法""问题"这类泛词。
4. 优先中文学术术语；英文缩写保留原文（如 CNN、CRISPR、Transformer）。
5. 术语应覆盖：研究对象/材料/基因/化合物、技术方法、关键概念、重要缩写。`

// BuildSpeakerProfile 调用 LLM 生成研究背景与热词。失败返回 error，由调用方降级处理。
func BuildSpeakerProfile(ctx context.Context, client *llm.Client, name, institution, discipline string) (*SpeakerProfile, error) {
	if institution == "" {
		institution = "未知"
	}
	if discipline == "" {
		discipline = "未知"
	}
	prompt := buildPrompt
	prompt = strings.ReplaceAll(prompt, "{{NAME}}", name)
	prompt = strings.ReplaceAll(prompt, "{{INSTITUTION}}", institution)
	prompt = strings.ReplaceAll(prompt, "{{DISCIPLINE}}", discipline)

	// 预算与超时都必须给足：hy3 是思考模型，reasoning_content 与正文共享 max_tokens，
	// 且思考时长随 prompt 波动很大。
	//   max_tokens 实测：2000 → reasoning 吃光额度、正文 0 字、0 个热词；
	//                    4000 → reasoning 3578、正文 757 字、20 个热词；
	//                    6000 → 正文 1001 字、23 个热词。
	//   耗时实测：同一 prompt 28~37s，而 LLM_TIMEOUT 默认 30s → 会随机超时，故按调用给 90s。
	raw, err := client.ChatTimeout(ctx, []llm.Message{{Role: "user", Content: prompt}},
		6000, 0.2, 90*time.Second)
	if err != nil {
		return nil, err
	}
	data := parseJSONObject(raw)

	out := &SpeakerProfile{Hotwords: make(map[string]int)}
	if p, ok := data["profile"].(string); ok {
		out.Profile = strings.TrimSpace(p)
	}
	if fields, ok := data["fields"].([]any); ok {
		for _, f := range fields {
			if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
				out.Fields = append(out.Fields, strings.TrimSpace(s))
			}
		}
	}
	if hw, ok := data["hotwords"].([]any); ok {
		for _, item := range hw {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			w, _ := obj["word"].(string)
			w = strings.TrimSpace(w)
			if w == "" {
				continue
			}
			weight := 50
			if v, ok := obj["weight"]; ok {
				weight = toInt(v, 50)
			}
			if weight < 1 {
				weight = 1
			}
			if weight > 100 {
				weight = 100
			}
			out.Hotwords[w] = weight
		}
	}
	// 空结果必须报错：LLM 偶发返回空/非 JSON 正文时，静默返回空 profile 会让
	// 调用方显示「成功但 0 个热词」，极难排查。
	if out.Profile == "" && len(out.Hotwords) == 0 {
		return nil, fmt.Errorf("LLM 未返回有效内容（原始输出 %d 字）: %s",
			len([]rune(raw)), clip(raw, 200))
	}
	return out, nil
}

var (
	reFencedJSON = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")
	reBraceJSON  = regexp.MustCompile(`(?s)\{.*\}`)
)

// parseJSONObject 容错解析 LLM 输出：剥掉 ```json 包裹，或提取首个 {...}。
func parseJSONObject(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if m := reFencedJSON.FindStringSubmatch(raw); m != nil {
		raw = strings.TrimSpace(m[1])
	}
	if obj := tryUnmarshalObject(raw); obj != nil {
		return obj
	}
	if m := reBraceJSON.FindString(raw); m != "" {
		if obj := tryUnmarshalObject(m); obj != nil {
			return obj
		}
	}
	return map[string]any{}
}

func tryUnmarshalObject(s string) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil
	}
	return obj
}

func toInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int(f)
		}
	}
	return def
}
