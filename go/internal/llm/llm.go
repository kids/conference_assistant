// Package llm OpenAI 兼容 chat.completions 客户端（默认对接 taiji，模型 hy3）。
// 对应 Python 版 app/providers/llm_openai.py。
//
// 超时语义与 Python 版对齐：httpx 的 timeout 是"单次操作"超时（连接/读），
// 不是整请求总时长。Go 的 http.Client.Timeout 是总时长，会把长时间流式生成掐断，
// 因此这里改为：Transport 层限制连接/响应头时间，流式读取用独立的总预算。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrAborted 由 onDelta 返回以主动中止流式生成（急停用）。
var ErrAborted = errors.New("生成已中止")

// Error LLM 调用错误。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Message 一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client LLM 客户端。
type Client struct {
	url      string
	apiKey   string
	model    string
	timeout  time.Duration
	http     *http.Client
	httpOnce *http.Client // 非流式复用同一 client
}

// New 创建客户端。chatPath 为空表示 baseURL 本身即完整端点（taiji）；
// 标准 OpenAI 兼容服务填 "/v1/chat/completions"。
func New(baseURL, apiKey, model string, timeout float64, chatPath string) *Client {
	base := strings.TrimRight(baseURL, "/")
	d := time.Duration(timeout * float64(time.Second))
	if d <= 0 {
		d = 30 * time.Second
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   d,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   d,
		ResponseHeaderTimeout: d,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{
		url:     base + chatPath,
		apiKey:  apiKey,
		model:   model,
		timeout: d,
		http:    &http.Client{Transport: tr}, // 不设 Timeout：流式由 ctx 控制
	}
}

// URL 端点（调试用）。
func (c *Client) URL() string { return c.url }

// Model 模型名。
func (c *Client) Model() string { return c.model }

func (c *Client) headers() map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	if c.apiKey != "" {
		h["Authorization"] = "Bearer " + c.apiKey
	}
	return h
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Code    *int   `json:"code"`
	Message string `json:"message"`
}

// Chat 非流式生成，返回完整正文（使用客户端默认超时）。
func (c *Client) Chat(ctx context.Context, messages []Message, maxTokens int, temperature float64) (string, error) {
	return c.ChatTimeout(ctx, messages, maxTokens, temperature, c.timeout)
}

// ChatTimeout 非流式生成，可覆盖单次调用超时。
// 思考模型（hy3）的思考时长随 prompt 波动很大：同一热词 prompt 实测 28~37s，
// 而 LLM_TIMEOUT 默认 30s（面向短交互任务设定）会随机超时。按调用给足预算，
// 比全局调大 LLM_TIMEOUT 更合适 —— 后者会让真正的失败路径也一起变慢。
func (c *Client) ChatTimeout(ctx context.Context, messages []Message, maxTokens int,
	temperature float64, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	payload := chatRequest{
		Model: c.model, Messages: messages,
		MaxTokens: maxTokens, Temperature: temperature, Stream: false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &Error{Msg: "请求序列化失败: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", &Error{Msg: "构造请求失败: " + err.Error()}
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", wrapNetErr(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", &Error{Msg: "读取响应失败: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Msg: fmt.Sprintf("LLM %d: %s", resp.StatusCode, truncate(string(raw), 200))}
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &Error{Msg: "响应解析失败: " + truncate(err.Error(), 200)}
	}
	// taiji 上游超时会返回 HTTP 200 + choices=null + code!=0，显式报错优于静默返回空串
	if len(out.Choices) == 0 && out.Code != nil && *out.Code != 0 {
		return "", &Error{Msg: fmt.Sprintf("LLM code=%d %s", *out.Code, out.Message)}
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	// 思考模型（hy3）的 reasoning 与正文共享 max_tokens：预算不足时会以
	// finish_reason=length 结束且正文为空。这种情况显式报错，避免调用方
	// 拿到空串后静默降级（如"生成了 0 个热词"这类难以排查的现象）。
	if content == "" && out.Choices[0].FinishReason == "length" {
		return "", &Error{Msg: fmt.Sprintf(
			"LLM 正文为空：max_tokens=%d 被思考内容占满，请调大该调用的 max_tokens", maxTokens)}
	}
	return content, nil
}

// streamBudget 流式生成的总时长预算。
// Python 版依赖 httpx 的"每次读操作"超时，每收到一个 delta 就重置，
// 因此思考模型跑 30s+ 也不会被掐断。Go 无逐次读超时，这里用一个
// 宽裕的总预算兜底：默认 timeout 的 6 倍（默认 30s → 180s），下限 60s。
func (c *Client) streamBudget() time.Duration {
	b := c.timeout * 6
	if b < 60*time.Second {
		b = 60 * time.Second
	}
	return b
}

// StreamChat 流式生成，逐段回调正文（跳过思考内容 reasoning_content）。
// onDelta 返回非 nil 错误会立即中止并关闭连接；返回 ErrAborted 表示主动急停。
func (c *Client) StreamChat(
	ctx context.Context,
	messages []Message,
	maxTokens int,
	temperature float64,
	onDelta func(string) error,
) error {
	payload := chatRequest{
		Model: c.model, Messages: messages,
		MaxTokens: maxTokens, Temperature: temperature, Stream: true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &Error{Msg: "请求序列化失败: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, c.streamBudget())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return &Error{Msg: "构造请求失败: " + err.Error()}
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return wrapNetErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &Error{Msg: fmt.Sprintf("LLM %d: %s", resp.StatusCode, truncate(string(raw), 200))}
	}

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if e := c.handleSSELine(line, onDelta); e != nil {
				return e
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// 主动中止（急停）不是网络错误
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				return ErrAborted
			}
			return wrapNetErr(err)
		}
	}
}

func (c *Client) handleSSELine(line string, onDelta func(string) error) error {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		return nil
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil // 单行解析失败跳过，不影响整体流
	}
	if len(obj.Choices) == 0 {
		return nil
	}
	if content := obj.Choices[0].Delta.Content; content != "" {
		if err := onDelta(content); err != nil {
			return err
		}
	}
	return nil
}

func wrapNetErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Msg: "LLM 超时"}
	}
	if errors.Is(err, context.Canceled) {
		return ErrAborted
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Msg: "LLM 超时"}
	}
	return &Error{Msg: "LLM 连接失败: " + err.Error()}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
