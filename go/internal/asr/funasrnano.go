package asr

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// FunAsrNanoStreamClient Fun-ASR-Nano vLLM 实时流式客户端。
//
// 协议（START/STOP 文本命令 + PCM 二进制帧）：
//
//	客户端 → 服务端： "START" / "LANGUAGE:中文" / "HOTWORDS:词1,词2" / [binary PCM] / "STOP"
//	服务端 → 客户端： {"event":"started"} / {"sentences":[...],"partial":"...","is_final":false}
//
// 服务端自带动态 VAD 断句，客户端持续透传 PCM 即可；
// 本项目默认用本地 VAD 主动切句（句尾静音即发 STOP），出字延迟从 10~20s 降到约 0.8s。
type FunAsrNanoStreamClient struct {
	language string
	handlers Handlers

	mu        sync.Mutex
	hotwords  []string
	started   bool // 当前是否处于 START..STOP 会话内
	sentCount int
	sentTexts []string
	round     int
	status    string
	detail    string
	conn      *Conn
}

// quietEvents 切句产生的会话事件属噪声，不向前端广播（对齐 Python 版）。
var quietEvents = map[string]struct{}{
	"event:started": {}, "event:stopped": {}, "event:language_set": {}, "event:hotwords_set": {},
}

// NewFunAsrNanoStreamClient 创建客户端。
func NewFunAsrNanoStreamClient(url, language string, hotwords []string, h Handlers) *FunAsrNanoStreamClient {
	c := &FunAsrNanoStreamClient{
		language: language,
		hotwords: append([]string(nil), hotwords...),
		handlers: h,
		status:   "idle",
	}
	c.conn = newConn(url, nil)
	c.conn.handshake = func(ws *websocket.Conn) error {
		c.sendSessionInit(ws)
		return nil
	}
	c.conn.onText = c.onText
	c.conn.onStatus = c.onStatus
	return c
}

// sendSessionInit 发送 START 及语种/热词配置，开启一轮识别会话。
// 调用方需保证不与其他写操作并发（握手阶段在 Conn 的 dial goroutine 内独占，send_audio 由 Conn 写锁串行）。
func (c *FunAsrNanoStreamClient) sendSessionInit(ws *websocket.Conn) {
	c.mu.Lock()
	language, hotwords := c.language, append([]string(nil), c.hotwords...)
	c.started = true
	c.sentCount = 0
	c.sentTexts = nil
	c.round++
	c.mu.Unlock()

	send := func(s string) error { return ws.WriteMessage(websocket.TextMessage, []byte(s)) }
	if err := send("START"); err != nil {
		return
	}
	if language != "" {
		_ = send("LANGUAGE:" + language)
	}
	if len(hotwords) > 0 {
		_ = send("HOTWORDS:" + strings.Join(hotwords, ","))
	}
}

// Connect 预热连接。
func (c *FunAsrNanoStreamClient) Connect() { c.conn.Connect() }

// SendAudio 送一帧音频；上一句已 STOP 时在本连接上重开会话（无需重连）。
func (c *FunAsrNanoStreamClient) SendAudio(pcm []byte) {
	c.mu.Lock()
	needInit := !c.started
	c.mu.Unlock()

	if needInit {
		// 需要新开会话：交由 Conn 在写锁内完成，避免与音频帧交错
		ws := c.conn.get()
		if ws == nil {
			return
		}
		c.sendSessionInitLocked(ws)
	}
	c.conn.sendBinary(pcm)
}

// sendSessionInitLocked 在 Conn 写锁保护下重开会话。
func (c *FunAsrNanoStreamClient) sendSessionInitLocked(ws *websocket.Conn) {
	c.conn.wmu.Lock()
	defer c.conn.wmu.Unlock()
	c.sendSessionInit(ws)
}

// MarkEnd 本地 VAD 判定句尾：发 STOP 让服务端立即 flush 出终稿（约 0.2s）。
// 不阻塞等待结果（结果由接收协程异步回调）；下次 SendAudio 会自动重开会话。
func (c *FunAsrNanoStreamClient) MarkEnd() {
	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return
	}
	c.started = false
	c.mu.Unlock()
	c.conn.sendText("STOP")
}

// UpdateHotwords 更新热词并断开当前连接（下次 send 自动重连并携带新热词）。
func (c *FunAsrNanoStreamClient) UpdateHotwords(words map[string]int) {
	c.mu.Lock()
	c.hotwords = keysOf(words)
	c.started = false
	c.mu.Unlock()
	c.conn.disconnect()
}

func (c *FunAsrNanoStreamClient) onStatus(raw string) {
	state, detail := normalizeStatus(raw)
	c.mu.Lock()
	c.status, c.detail = state, detail
	c.mu.Unlock()
	if c.handlers.OnStatus == nil {
		return
	}
	// 切句造成的 started/stopped 等事件不广播，避免状态栏刷屏
	if _, quiet := quietEvents[raw]; quiet {
		return
	}
	c.handlers.OnStatus(raw)
}

func (c *FunAsrNanoStreamClient) onText(text string) {
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return
	}
	if ev, ok := data["event"].(string); ok {
		c.onStatus("event:" + ev)
		return
	}

	sentences := parseSentences(data["sentences"])
	partial, _ := data["partial"].(string)

	c.mu.Lock()
	round := c.round
	upper := c.sentCount
	if len(sentences) < upper {
		upper = len(sentences)
	}
	if len(c.sentTexts) < upper {
		upper = len(c.sentTexts)
	}
	var revisions []struct{ key, text string }
	if c.handlers.OnRevise != nil {
		for i := 0; i < upper; i++ {
			newText := trimSpace(sentences[i].Text)
			if newText != "" && newText != c.sentTexts[i] {
				c.sentTexts[i] = newText
				revisions = append(revisions, struct{ key, text string }{fmt.Sprintf("r%di%d", round, i), newText})
			}
		}
	}
	var finals []struct{ key, text string }
	if len(sentences) > c.sentCount {
		for i := c.sentCount; i < len(sentences); i++ {
			t := trimSpace(sentences[i].Text)
			c.sentTexts = append(c.sentTexts, t)
			if t != "" && c.handlers.OnOffline != nil {
				finals = append(finals, struct{ key, text string }{fmt.Sprintf("r%di%d", round, i), t})
			}
		}
		c.sentCount = len(sentences)
	}
	c.mu.Unlock()

	for _, r := range revisions {
		c.handlers.OnRevise(r.key, r.text)
	}
	for _, f := range finals {
		c.handlers.OnOffline(f.key, f.text)
	}
	// partial 临时文本 → 实时字幕草稿
	if c.handlers.OnOnline != nil {
		c.handlers.OnOnline(partial)
	}
}

// StatusInfo 当前状态。
func (c *FunAsrNanoStreamClient) StatusInfo() StatusInfo {
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
func (c *FunAsrNanoStreamClient) Close() { c.conn.Close() }
