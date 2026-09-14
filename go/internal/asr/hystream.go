package asr

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// HyAsrStreamClient HY-ASR-3-Stream 流式识别客户端
// （asr-gateway /tencent/asr/recognize/stream）。
//
// 协议：
//
//	客户端 → 服务端：{"event":"asr.start", voice_id, session_id, user_id, model,
//	                  audio:{format,sample_rate,channel}, options:{enable_semantic_vad,hotwords}}
//	                [binary PCM 16k/mono/int16]
//	服务端 → 客户端：{"event":"asr.started"}
//	                {"event":"asr.result","data":{text, sentences:[{index,text,is_final}]}}
//	                {"event":"asr.done","code":0,"data":{text}}
//
// 特性：逐词增量 → 实时草稿；语义 VAD 自动断句；已确认句变化 → on_revise 回退修正。
type HyAsrStreamClient struct {
	baseURL  string
	token    string
	model    string
	handlers Handlers

	mu         sync.Mutex
	hotwords   []string
	sentCount  int      // 已确认(final)句子数
	sentTexts  []string // 已确认句子文本（回退修正检测）
	round      int
	status     string
	detail     string
	conn       *Conn
	closedOnce sync.Once
}

// NewHyAsrStreamClient 创建客户端。
func NewHyAsrStreamClient(url, token, model string, hotwords []string, h Handlers) *HyAsrStreamClient {
	c := &HyAsrStreamClient{
		baseURL:  url,
		token:    token,
		model:    model,
		hotwords: append([]string(nil), hotwords...),
		handlers: h,
		status:   "idle",
	}
	c.conn = newConn(c.endpoint(), nil)
	c.conn.handshake = c.doHandshake
	c.conn.onConnected = c.onConnected
	c.conn.onText = c.onText
	c.conn.onStatus = c.onStatus
	return c
}

func (c *HyAsrStreamClient) endpoint() string {
	return fmt.Sprintf("%s?model=%s&token=%s", c.baseURL, c.model, c.token)
}

// doHandshake 发送 asr.start 并等待 asr.started（对齐 Python 版首次 recv 校验）。
func (c *HyAsrStreamClient) doHandshake(ws *websocket.Conn) error {
	c.mu.Lock()
	opt := map[string]any{"enable_semantic_vad": true}
	if len(c.hotwords) > 0 {
		opt["hotwords"] = append([]string(nil), c.hotwords...)
	}
	c.mu.Unlock()

	msg := map[string]any{
		"event":      "asr.start",
		"voice_id":   newUUID(),
		"session_id": newUUID(),
		"user_id":    "translation-seat",
		"model":      c.model,
		"audio":      map[string]any{"format": "pcm", "sample_rate": 16000, "channel": 1},
		"options":    opt,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := ws.WriteMessage(websocket.TextMessage, raw); err != nil {
		return err
	}

	// 等待 asr.started 确认（超时保护）
	_ = ws.SetReadDeadline(time.Now().Add(dialTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	var first struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(data, &first); err != nil {
		return fmt.Errorf("握手响应解析失败: %w", err)
	}
	if first.Event != "asr.started" {
		return fmt.Errorf("未收到 asr.started: %.120s", string(data))
	}
	return nil
}

// onConnected 每次成功连接后复位句内计数并递增轮次。
func (c *HyAsrStreamClient) onConnected() {
	c.mu.Lock()
	c.sentCount = 0
	c.sentTexts = nil
	c.round++
	c.status, c.detail = "connected", ""
	c.mu.Unlock()
}

func (c *HyAsrStreamClient) onStatus(raw string) {
	state, detail := normalizeStatus(raw)
	c.mu.Lock()
	c.status, c.detail = state, detail
	c.mu.Unlock()
	if c.handlers.OnStatus != nil {
		c.handlers.OnStatus(raw)
	}
}

func (c *HyAsrStreamClient) onText(text string) {
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return
	}
	event, _ := data["event"].(string)
	if event == "asr.result" {
		d, _ := data["data"].(map[string]any)
		if d == nil {
			return
		}
		sentences := parseSentences(d["sentences"])
		c.handleSentences(sentences)
		// 草稿：当前句（最后一个未确认句）的增量文本
		if c.handlers.OnOnline != nil && len(sentences) > 0 {
			draft := trimSpace(sentences[len(sentences)-1].Text)
			c.handlers.OnOnline(draft)
		}
		return
	}
	if event == "asr.done" || event == "asr.error" || event == "asr.failed" {
		code, hasCode := data["code"]
		if hasCode && code != nil {
			if f, ok := toFloat(code); ok && f != 0 {
				msg, _ := data["message"].(string)
				c.onStatus(fmt.Sprintf("error:%s:%s", event, msg))
			}
		}
	}
}

type sentence struct {
	Index   int
	Text    string
	IsFinal bool
}

func parseSentences(v any) []sentence {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]sentence, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		s := sentence{}
		if idx, ok := toFloat(obj["index"]); ok {
			s.Index = int(idx)
		}
		s.Text, _ = obj["text"].(string)
		s.IsFinal, _ = obj["is_final"].(bool)
		out = append(out, s)
	}
	return out
}

// handleSentences 按 index 处理：已确认句文本变化 → 修正；新确认句 → 终稿。
func (c *HyAsrStreamClient) handleSentences(sentences []sentence) {
	if len(sentences) == 0 {
		return
	}
	c.mu.Lock()
	round, sentCount := c.round, c.sentCount
	upper := sentCount
	if len(sentences) < upper {
		upper = len(sentences)
	}
	if len(c.sentTexts) < upper {
		upper = len(c.sentTexts)
	}
	// 1) 已确认句文本变化 → 回退修正
	var revisions []struct {
		key  string
		text string
	}
	if c.handlers.OnRevise != nil {
		for i := 0; i < upper; i++ {
			newText := trimSpace(sentences[i].Text)
			if newText != "" && newText != c.sentTexts[i] {
				c.sentTexts[i] = newText
				revisions = append(revisions, struct {
					key  string
					text string
				}{fmt.Sprintf("r%di%d", round, i), newText})
			}
		}
	}
	// 2) 新确认句（is_final=true 且序号 >= sentCount）→ 终稿
	var finals []struct {
		key  string
		text string
	}
	for _, s := range sentences[c.sentCount:] {
		if !s.IsFinal {
			break
		}
		text := trimSpace(s.Text)
		c.sentTexts = append(c.sentTexts, text)
		if text != "" && c.handlers.OnOffline != nil {
			idx := s.Index
			if idx == 0 {
				idx = c.sentCount
			}
			finals = append(finals, struct {
				key  string
				text string
			}{fmt.Sprintf("r%di%d", round, idx), text})
		}
		c.sentCount++
	}
	c.mu.Unlock()

	// 回调在锁外执行：回调会反向调用 Runtime（落库/广播），避免持锁穿越
	for _, r := range revisions {
		c.handlers.OnRevise(r.key, r.text)
	}
	for _, f := range finals {
		c.handlers.OnOffline(f.key, f.text)
	}
}

// Connect 预热连接。
func (c *HyAsrStreamClient) Connect() { c.conn.Connect() }

// SendAudio 送一帧音频。连接不可用时该帧被丢弃（实时优先）。
func (c *HyAsrStreamClient) SendAudio(pcm []byte) { c.conn.sendBinary(pcm) }

// MarkEnd 语义 VAD 自动断句，会话进行中无需 asr.end（保持长连接持续转写）。
func (c *HyAsrStreamClient) MarkEnd() {}

// UpdateHotwords 更新热词并断开当前连接（下次 send 自动重连并携带新热词）。
func (c *HyAsrStreamClient) UpdateHotwords(words map[string]int) {
	c.mu.Lock()
	c.hotwords = keysOf(words)
	c.mu.Unlock()
	c.conn.disconnect()
}

// StatusInfo 当前状态。
func (c *HyAsrStreamClient) StatusInfo() StatusInfo {
	state, detail := c.conn.State()
	c.mu.Lock()
	if c.status != "" && c.status != "idle" {
		state = c.status
		detail = c.detail
	}
	c.mu.Unlock()
	return StatusInfo{State: state, Detail: detail, IdleSec: c.conn.IdleSec()}
}

// Close 释放资源。
func (c *HyAsrStreamClient) Close() { c.closedOnce.Do(func() { c.conn.Close() }) }
