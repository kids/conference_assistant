// Package diarize 说话人区分客户端：把「一段语音的 PCM」交给本地 CAM++ sidecar，
// 换回该段属于哪一位说话人（同一 session 内编号稳定）。
//
// 为什么是「一段一段」而不是整场日志：
//   - 本项目的 ASR 是流式的，字幕逐句出；说话人标记要跟着句子走才有意义；
//   - 增量聚类（sidecar 内做）让编号在同一 session 内保持一致（S1/S2/...），
//     且不需要等整场音频结束再回头重算。
//
// 依赖 sidecar（tools/diarize/），未启用/未就绪时所有调用立即失败，主链路不受影响。
package diarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Result 一次说话人判定结果（sidecar /assign 的响应）。
type Result struct {
	Speaker      string  `json:"speaker"`       // S1/S2/...（空串表示未判定，见 Reason）
	Label        string  `json:"label"`         // 说话人 1 / 说话人 2 …
	Confidence   float64 `json:"confidence"`    // 与所属说话人中心的余弦相似度
	Margin       float64 `json:"margin"`        // 与次近说话人的差距，越小越可疑
	IsNew        bool    `json:"is_new"`        // 本段首次出现该说话人
	Seconds      float64 `json:"seconds"`       // 本段时长
	SpeakerCount int     `json:"speaker_count"` // 本场已出现的说话人数量
	Reason       string  `json:"reason"`        // too_short 等跳过原因
	Error        string  `json:"error"`
}

// Client sidecar HTTP 客户端。零值不可用；用 New 创建。
type Client struct {
	base      string
	http      *http.Client
	threshold float64 // >0 时随请求下发，覆盖 sidecar 默认阈值
	minMS     int     // 小于该时长的片段不送（省一次推理，也避免短音频 embedding 抖动）
}

// New 创建客户端。baseURL 形如 http://127.0.0.1:18901；timeoutSec<=0 取 20s。
func New(baseURL string, timeoutSec, threshold float64, minMS int) *Client {
	d := time.Duration(timeoutSec * float64(time.Second))
	if d <= 0 {
		d = 20 * time.Second
	}
	return &Client{
		base:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:      &http.Client{Timeout: d},
		threshold: threshold,
		minMS:     minMS,
	}
}

// Enabled 客户端是否可用（未配置 URL 时返回 false）。
func (c *Client) Enabled() bool { return c != nil && c.base != "" }

// MinMS 过短片段门槛。
func (c *Client) MinMS() int {
	if c == nil {
		return 0
	}
	return c.minMS
}

// Assign 把一段 16k/mono/int16 PCM 交给 sidecar 判定说话人。
// sessionDir 非空时，sidecar 会把该场的说话人表落盘到 <sessionDir>/speakers.json，
// 这样 sidecar 重启后同一 session 的编号仍然对齐。
func (c *Client) Assign(ctx context.Context, sessionID, sessionDir string, pcm []byte) (Result, error) {
	var out Result
	if !c.Enabled() {
		return out, fmt.Errorf("说话人区分未启用")
	}
	if len(pcm) < c.minMS*2*16000/1000 && c.minMS > 0 {
		return Result{Reason: "too_short", Seconds: float64(len(pcm)) / 2 / 16000}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/assign", sessionID, sessionDir), bytes.NewReader(pcm))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	body, err := c.do(req)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("sidecar 响应解析失败: %w", err)
	}
	if out.Error != "" {
		return out, fmt.Errorf("sidecar: %s", out.Error)
	}
	return out, nil
}

// Reset 清空某 session 的说话人表（新建 session 时调用）。
// sessionDir 一并带上：sidecar 据此把该场的说话人表落盘到 <sessionDir>/speakers.json。
func (c *Client) Reset(ctx context.Context, sessionID, sessionDir string) error {
	if !c.Enabled() {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/reset", sessionID, sessionDir), nil)
	if err != nil {
		return err
	}
	_, err = c.do(req)
	return err
}

// Health 探活，返回 sidecar 自述信息（模型、设备、阈值等）。
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("说话人区分未启用")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return nil, err
	}
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("sidecar 响应解析失败: %w", err)
	}
	return out, nil
}

func (c *Client) url(path, sessionID, sessionDir string) string {
	q := url.Values{}
	q.Set("session", sessionID)
	if sessionDir != "" {
		q.Set("dir", sessionDir)
	}
	if c.threshold > 0 {
		q.Set("threshold", fmt.Sprintf("%.3f", c.threshold))
	}
	return c.base + path + "?" + q.Encode()
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sidecar HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
