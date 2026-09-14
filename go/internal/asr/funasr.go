package asr

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// FunAsrStreamClient 标准 FunASR 流式推理客户端。
//
// 协议（对齐 FunASR 官方 websocket_protocol）：
//  1. 建立 WebSocket 连接
//  2. 发送配置 JSON（mode/2pass、wav_name、is_speaking、wav_format=pcm、chunk_size、audio_fs、hotwords、itn）
//  3. 持续发送 PCM 二进制帧（16k/mono/int16，无 WAV 头）
//  4. 一句结束发送 {"is_speaking": false}
//  5. 服务端返回 {"mode":"2pass-online","text":...} 实时结果 / {"mode":"2pass-offline","text":...,"is_final":true} 终稿
//
// 注意：该协议没有服务端回退修正与句键（对齐 Python 版行为，句键传空串）。
type FunAsrStreamClient struct {
	mode      string
	chunkSize []int
	audioFS   int
	handlers  Handlers

	mu       sync.Mutex
	hotwords string
	speaking bool
	seq      int
	status   string
	detail   string
	conn     *Conn
}

// NewFunAsrStreamClient 创建客户端。
func NewFunAsrStreamClient(url, mode string, chunkSize []int, hotwordsJSON string, audioFS int, h Handlers) *FunAsrStreamClient {
	if len(chunkSize) == 0 {
		chunkSize = []int{5, 10, 5}
	}
	c := &FunAsrStreamClient{
		mode:      mode,
		chunkSize: chunkSize,
		audioFS:   audioFS,
		hotwords:  hotwordsJSON,
		handlers:  h,
		status:    "idle",
	}
	c.conn = newConn(url, nil)
	c.conn.handshake = c.doHandshake
	c.conn.onText = c.onText
	c.conn.onStatus = c.onStatus
	return c
}

func (c *FunAsrStreamClient) nextName() string {
	c.seq++
	return fmt.Sprintf("seg_%04d", c.seq)
}

// doHandshake 发送首条配置 JSON。
func (c *FunAsrStreamClient) doHandshake(ws *websocket.Conn) error {
	c.mu.Lock()
	cfg := map[string]any{
		"mode":        c.mode,
		"wav_name":    c.nextName(),
		"is_speaking": true,
		"wav_format":  "pcm",
		"chunk_size":  c.chunkSize,
		"audio_fs":    c.audioFS,
		"itn":         true,
	}
	if c.hotwords != "" {
		cfg["hotwords"] = c.hotwords
	}
	c.speaking = true
	c.mu.Unlock()

	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.TextMessage, raw)
}

// Connect 预热连接。
func (c *FunAsrStreamClient) Connect() { c.conn.Connect() }

// SendAudio 送一帧音频；本句尚未开启时先发 is_speaking=true 开新句。
func (c *FunAsrStreamClient) SendAudio(pcm []byte) {
	c.mu.Lock()
	needStart := !c.speaking
	var startMsg []byte
	if needStart {
		startMsg, _ = json.Marshal(map[string]any{"is_speaking": true, "wav_name": c.nextName()})
		c.speaking = true
	}
	c.mu.Unlock()

	if needStart {
		c.conn.send(startMsg, false)
	}
	c.conn.sendBinary(pcm)
}

// MarkEnd 断句：发送 is_speaking=false。
func (c *FunAsrStreamClient) MarkEnd() {
	c.mu.Lock()
	if !c.speaking {
		c.mu.Unlock()
		return
	}
	c.speaking = false
	c.mu.Unlock()
	raw, _ := json.Marshal(map[string]any{"is_speaking": false})
	c.conn.send(raw, false)
}

// UpdateHotwords 更新热词并断开当前连接（下次 send 自动重连并携带新热词）。
func (c *FunAsrStreamClient) UpdateHotwords(words map[string]int) {
	c.mu.Lock()
	c.hotwords = ToStreamJSON(words)
	c.speaking = false
	c.mu.Unlock()
	c.conn.disconnect()
}

func (c *FunAsrStreamClient) onStatus(raw string) {
	state, detail := normalizeStatus(raw)
	// 对齐 Python 版：该协议的接收线程失败时先报 error，随后 finally 里 on_close 报 closed，
	// 最终对外可见状态为 closed。
	dropped := strings.HasPrefix(raw, "error:")
	if dropped {
		state, detail = "closed", ""
	}
	c.mu.Lock()
	c.status, c.detail = state, detail
	c.mu.Unlock()

	if c.handlers.OnStatus != nil {
		c.handlers.OnStatus(raw)
		if dropped {
			c.handlers.OnStatus("closed")
		}
	}
}

func (c *FunAsrStreamClient) onText(text string) {
	var data struct {
		Mode    string `json:"mode"`
		Text    string `json:"text"`
		IsFinal bool   `json:"is_final"`
	}
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return
	}
	switch {
	case strings.HasSuffix(data.Mode, "online"):
		if c.handlers.OnOnline != nil && data.Text != "" {
			c.handlers.OnOnline(data.Text)
		}
	case strings.HasSuffix(data.Mode, "offline") || data.IsFinal:
		if c.handlers.OnOffline != nil && data.Text != "" {
			c.handlers.OnOffline("", data.Text)
		}
	}
}

// StatusInfo 当前状态。
func (c *FunAsrStreamClient) StatusInfo() StatusInfo {
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
func (c *FunAsrStreamClient) Close() { c.conn.Close() }
