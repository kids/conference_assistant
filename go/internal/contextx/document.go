package contextx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"seat/internal/llm"
)

// ---- 文档解析服务（/parse_doc）----

// DefaultDocParseURL 文档解析服务地址。与 LLM（/tencent/llm/taiji）、
// hy ASR（/tencent/asr/recognize/stream）同属一个网关。
//
// 说明：网关上的 /tencent/read_url 只是本接口的一层壳 —— 它把 {url} 转成
// {file_url} 再调 /parse_doc，因此要求文件先有一个服务端可访问的 URL。
// 直接调 /parse_doc 可以收 multipart 上传，省掉「把上传目录暴露出去」这一步。
const DefaultDocParseURL = "https://ml-serv.ssv.qq.com/parse_doc"

// DefaultDocDigestChars 讲稿浓缩摘要的目标字数（进 AI 上下文的版本）。
const DefaultDocDigestChars = 800

// MaxDigestChars 摘要目标字数的上限。
//
// 上下文组装时 STATIC 块会被截到 staticContextMaxChars（见 Manager.Build），
// 再加写入文件时那行 "# 讲稿摘要：xxx" 标题，所以摘要本身不能超过 2400 ——
// 超了会在拼上下文时被静默截断，白丢信息。
//
// 实测（输出固定、只变输入）：上下文从 0 涨到 4309 prompt tokens，单次调用耗时
// 仍是 1.1~1.7s（噪声级）—— 耗时由生成量决定，prefill 很便宜。所以调大摘要是
// 安全的，没必要为了速度牺牲信息量；真正的约束是上面这个截断上限。
const MaxDigestChars = 2400

// docParseTimeout 解析服务自身超时为 60s，这里给足余量（大 PPT 解析较慢）。
const docParseTimeout = 120 * time.Second

// docMaxInputChars 送进 LLM 的讲稿上限。超出即截断 —— 一次调用塞太多会让
// hy3 的思考时间显著变长，而实时翻译对延迟敏感。
const docMaxInputChars = 20000

// DefaultMaxUploadBytes 上传体积上限，用于提前拦截。
//
// 真实上限来自 file 服务（ssv-ml-serv/services/file/fread_rs）的 axum
// DefaultBodyLimit，已从 axum 默认的 2 MiB 调到 256 MiB。这里取同一量级 ——
// 该服务会把整个文件读进内存（field.bytes()），完全放开有 OOM 风险。
//
// 踩坑记录（2026-09，以免重蹈）：当时 >2MB 的上传报
//   "multipart bytes error: Error parsing `multipart/form-data` request"
// 曾误判为网关 client_max_body_size，但实测网关侧 JSON 32MB 都能完整到达、
// 且 Kong 配置里 client_max_body_size=0（不限制）—— 真正原因是 axum 的
// Multipart 提取器读 DefaultBodyLimit（默认 2MB），而服务里只设了
// RequestBodyLimitLayer，对 Multipart 不生效。已在 file 服务修复。
const DefaultMaxUploadBytes = 256 << 20

// CheckUploadSize 体积预检。超出上限时返回带可执行建议的错误。
func CheckUploadSize(n, maxBytes int) error {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxUploadBytes
	}
	if n <= maxBytes {
		return nil
	}
	return fmt.Errorf(
		"文件 %.1f MB 超过 %.1f MB 上传上限（file 服务的 DefaultBodyLimit 限制，超出会被截断，"+
			"服务端只会报 multipart 解析失败）。建议：先用 pdftotext / soffice 等把文档转成"+
			"纯文本再上传（文字层通常只有几十 KB），或调大 file 服务的 DefaultBodyLimit "+
			"并把 DOC_MAX_BYTES 同步放开",
		float64(n)/1048576, float64(maxBytes)/1048576)
}

// ParsedDoc 解析结果。
type ParsedDoc struct {
	Filename string
	Content  string
	Truncated bool // 是否因超出 docMaxInputChars 被截断
}

// docRow /parse_doc 的单条返回。
type docRow struct {
	Content  string `json:"content"`
	Filename string `json:"filename"`
}

// ParseDocument 把文件交给文档解析服务，返回纯文本。
//
// 服务端契约（services/tencentapi：readURLHandler → parseDocByJSON → /parse_doc）：
//
//	请求  POST multipart/form-data，字段名 file
//	响应  [{"content":"正文","filename":"文件名"}]
func ParseDocument(ctx context.Context, parseURL, filename string, data []byte) (*ParsedDoc, error) {
	if strings.TrimSpace(parseURL) == "" {
		parseURL = DefaultDocParseURL
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("构造上传体失败: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("写入上传体失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("关闭上传体失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, docParseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parseURL, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("文档解析服务不可达: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("读取解析结果失败: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("文档解析服务返回 %d: %s", resp.StatusCode, clip(string(raw), 200))
	}

	rows := decodeDocRows(raw)
	if len(rows) == 0 {
		return nil, fmt.Errorf("文档解析服务未返回内容: %s", clip(string(raw), 200))
	}

	var sb strings.Builder
	name := filename
	for _, r := range rows {
		if strings.TrimSpace(r.Content) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(strings.TrimSpace(r.Content))
		if r.Filename != "" {
			name = r.Filename
		}
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return nil, fmt.Errorf("解析结果为空（该格式可能不支持，或文件无文字层）")
	}

	out := &ParsedDoc{Filename: name}
	if r := []rune(text); len(r) > docMaxInputChars {
		out.Content = string(r[:docMaxInputChars])
		out.Truncated = true
	} else {
		out.Content = text
	}
	return out, nil
}

// decodeDocRows 容错解析：正常是数组，也容忍单个对象。
func decodeDocRows(raw []byte) []docRow {
	var rows []docRow
	if err := json.Unmarshal(raw, &rows); err == nil {
		return rows
	}
	var one docRow
	if err := json.Unmarshal(raw, &one); err == nil && one.Content != "" {
		return []docRow{one}
	}
	return nil
}

// ---- 从讲稿抽取热词 + 浓缩摘要（一次 LLM 调用产出两者）----

// DocumentProfile 讲稿分析结果。
type DocumentProfile struct {
	Digest   string         // 浓缩摘要：进 AI 上下文的那份
	Fields   []string       // 学科方向
	Hotwords map[string]int // 热词 → 权重
}

const docPrompt = `你是学术会议支持助手。下面是一份报告讲稿/演示稿的解析文本（可能来自 PPT、PDF 或文档）。

请完成三件事，并严格输出一个 JSON 对象（不要输出任何其他文字、解释或 Markdown 代码块标记）：

{"digest":"...","fields":["方向1","方向2"],"hotwords":[{"word":"术语","weight":60}]}

要求：

1. digest —— {{DIGEST_CHARS}} 字以内的浓缩摘要，用于实时翻译时提供背景。
   - 必须保留：研究领域、核心术语、关键方法、主要结论、重要缩写、涉及的人名机构。
   - 必须去掉：致谢、目录、页眉页脚、参考文献列表、重复内容、版式残留字符。
   - 直接给内容，不要写「本文介绍了…」「该报告讨论了…」这类空话。
   - 幻灯片文本是碎片化的，请按语义重组为连贯段落。

2. hotwords —— 讲稿中出现的核心专业术语、方法名、模型名、算法名、缩写、专有名词，10~25 个。
   - weight 为 1~100 的整数：核心术语 60~100，一般术语 30~59。
   - 术语要具体、有辨识度，避免「研究」「方法」「问题」这类泛词。
   - 优先中文学术术语；英文缩写保留原文（如 CNN、CRISPR、Transformer）。

3. fields —— 讲稿涉及的学科方向，1~5 个。

讲稿文本：
---
{{DOC}}
---`

// BuildFromDocument 一次 LLM 调用同时产出浓缩摘要与热词。
//
// 为什么不直接把解析原文塞进上下文：幻灯片解析出来是碎片化的，信噪比低，
// 而且每次 AI 调用都要重新读一遍，长上下文会拉长 hy3 的思考时间。这里在
// 上传时一次性浓缩，之后每次调用只带摘要，既省 token 又更聚焦。
func BuildFromDocument(ctx context.Context, client *llm.Client, docText string, digestChars int) (*DocumentProfile, error) {
	if digestChars <= 0 {
		digestChars = DefaultDocDigestChars
	}
	if digestChars > MaxDigestChars {
		digestChars = MaxDigestChars // 超过上下文截断上限就白写了
	}
	prompt := strings.ReplaceAll(docPrompt, "{{DIGEST_CHARS}}", fmt.Sprint(digestChars))
	prompt = strings.ReplaceAll(prompt, "{{DOC}}", docText)

	// 预算与超时对齐 BuildSpeakerProfile：hy3 是思考模型，reasoning 与正文共享
	// max_tokens，给少了会正文为空；思考时长随 prompt 波动，按调用给 120s。
	// 实测长英文文献：reasoning 可占 4200+ token，故预算给到 8000 留足正文空间。
	raw, err := client.ChatTimeout(ctx, []llm.Message{{Role: "user", Content: prompt}},
		8000, 0.2, 120*time.Second)
	if err != nil {
		return nil, err
	}
	data := parseJSONObject(raw)

	out := &DocumentProfile{Hotwords: make(map[string]int)}
	if d, ok := data["digest"].(string); ok {
		out.Digest = strings.TrimSpace(d)
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
			weight := toInt(obj["weight"], 50)
			if weight < 1 {
				weight = 1
			}
			if weight > 100 {
				weight = 100
			}
			out.Hotwords[w] = weight
		}
	}
	// 空结果必须报错，不能当成「成功但 0 个热词」静默放过。
	// 实测过一次偶发：LLM 返回空/非 JSON 正文，调用方拿到空 profile 却显示成功，
	// 页面上表现为「抽了 0 个热词」，极难排查。
	if out.Digest == "" && len(out.Hotwords) == 0 {
		return nil, fmt.Errorf("LLM 未返回有效内容（原始输出 %d 字）: %s",
			len([]rune(raw)), clip(raw, 200))
	}
	return out, nil
}

// clip 按字符截断，避免把超长响应整段写进错误信息。
func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
