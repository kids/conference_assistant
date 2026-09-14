package asr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/maxhawkins/go-webrtcvad"
)

// Qwen3-ASR HTTP 实时客户端：WS 流式体验由客户端拼装（无需服务端封装层）。
//
// 底层 vLLM OpenAI 兼容转写 POST /v1/audio/transcriptions（整段 WAV → 文本，单段 ≤30s）。
// 策略（与 Python 版线上实测结论一致）：
//   - partial：每 partial_interval 秒音频，把自上一确认句起的累积缓冲整段重推一次，
//     草稿单调增长（实测前缀稳定、词中截断自动修正，RTF 0.03~0.07）；
//   - final：webrtcvad 检测句尾静音后推理确认句，清空缓冲；
//   - 保护：单段达 max_segment_s 强制切句；纯静音段不推理（避免整段静音识别出「嗯。」）。
const (
	qwenFrameMS    = 30
	qwenSampleRate = 16000
	qwenFrameBytes = qwenSampleRate * 2 * qwenFrameMS / 1000
)

// langMap 语言别名 → ISO 639-1（vLLM transcriptions 仅接受 ISO 码）。
var langMap = map[string]string{
	"中文": "zh", "汉语": "zh", "英语": "en", "英文": "en", "日语": "ja", "韩语": "ko",
}

// Qwen3AsrHttpClient Qwen3-ASR HTTP 转写客户端（模拟流式）。
type Qwen3AsrHttpClient struct {
	baseURL        string
	model          string
	language       string
	partialInterval float64
	vadSilenceMS   int
	maxSegmentS    float64
	inferTimeoutS  float64
	handlers       Handlers

	http *http.Client

	mu           sync.Mutex
	hotwords     []string
	buf          []byte // 自上一确认句起的音频
	frameBuf     []byte // 任意 chunk → 30ms 帧重组
	inSpeech     bool
	speechFrames int
	silenceFrames int
	speechSeen   bool
	lastPartial  float64
	round        int
	forceFinal   bool
	status       string
	detail       string
	lastActive   time.Time
	inferFail    int

	vad  *webrtcvad.VAD
	stop chan struct{}
	done chan struct{}
	wake chan struct{}
}

// NewQwen3AsrHttpClient 创建客户端并启动调度协程。
func NewQwen3AsrHttpClient(baseURL, model, language string, hotwords []string,
	partialIntervalS float64, vadSilenceMS, vadAggressiveness int, maxSegmentS, inferTimeoutS float64,
	h Handlers) (*Qwen3AsrHttpClient, error) {

	v, err := webrtcvad.New()
	if err != nil {
		return nil, fmt.Errorf("初始化 webrtcvad 失败: %w", err)
	}
	if vadAggressiveness < 0 || vadAggressiveness > 3 {
		vadAggressiveness = 2
	}
	if err := v.SetMode(vadAggressiveness); err != nil {
		return nil, err
	}

	lang := strings.TrimSpace(strings.ToLower(language))
	iso, ok := langMap[lang]
	if !ok {
		iso = "zh"
		if len([]rune(language)) == 2 {
			iso = language
		}
	}

	c := &Qwen3AsrHttpClient{
		baseURL:         strings.TrimRight(baseURL, "/"),
		model:           model,
		language:        iso,
		partialInterval: partialIntervalS,
		vadSilenceMS:    vadSilenceMS,
		maxSegmentS:     maxSegmentS,
		inferTimeoutS:   inferTimeoutS,
		handlers:        h,
		hotwords:        append([]string(nil), hotwords...),
		http:            &http.Client{Timeout: time.Duration(inferTimeoutS * float64(time.Second))},
		vad:             v,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
		wake:            make(chan struct{}, 1),
		status:          "idle",
	}
	go c.scheduler()
	return c, nil
}

// Connect 该协议为按需 HTTP 请求，无需预热连接。
func (c *Qwen3AsrHttpClient) Connect() {}

// SendAudio 送入音频（内部重切为 30ms 帧并做 VAD）。
func (c *Qwen3AsrHttpClient) SendAudio(pcm []byte) {
	select {
	case <-c.stop:
		return
	default:
	}

	c.mu.Lock()
	c.frameBuf = append(c.frameBuf, pcm...)
	var frames [][]byte
	for len(c.frameBuf) >= qwenFrameBytes {
		f := make([]byte, qwenFrameBytes)
		copy(f, c.frameBuf[:qwenFrameBytes])
		frames = append(frames, f)
		c.frameBuf = c.frameBuf[qwenFrameBytes:]
	}
	c.mu.Unlock()

	for _, f := range frames {
		c.feedFrame(f)
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// feedFrame 累积音频并更新 VAD 状态机。
func (c *Qwen3AsrHttpClient) feedFrame(frame []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, frame...)
	isSpeech := false
	if ok, err := c.vad.Process(qwenSampleRate, frame); err == nil {
		isSpeech = ok
	}
	if isSpeech {
		c.speechFrames++
		c.silenceFrames = 0
		if !c.inSpeech && c.speechFrames >= 3 {
			c.inSpeech = true
		}
	} else {
		c.silenceFrames++
		c.speechFrames = 0
		c.inSpeech = false
	}
	if c.inSpeech || c.speechFrames > 0 {
		c.speechSeen = true
	}
}

// MarkEnd 外部要求立即切句。
func (c *Qwen3AsrHttpClient) MarkEnd() {
	c.mu.Lock()
	c.forceFinal = true
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// UpdateHotwords 更新热词（下次推理即生效）。
func (c *Qwen3AsrHttpClient) UpdateHotwords(words map[string]int) {
	c.mu.Lock()
	c.hotwords = keysOf(words)
	c.mu.Unlock()
}

// StatusInfo 当前状态。
func (c *Qwen3AsrHttpClient) StatusInfo() StatusInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	idle := 0.0
	if !c.lastActive.IsZero() {
		idle = time.Since(c.lastActive).Seconds()
	}
	return StatusInfo{State: c.status, Detail: c.detail, IdleSec: round1(idle)}
}

// Close 停止调度协程并释放资源。
func (c *Qwen3AsrHttpClient) Close() {
	select {
	case <-c.stop:
		return
	default:
	}
	close(c.stop)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
	c.http.CloseIdleConnections()
	c.setStatus("closed")
}

// ---- 内部 ----

func (c *Qwen3AsrHttpClient) setStatus(s string) {
	c.mu.Lock()
	switch {
	case s == "connected":
		c.status, c.detail = "connected", ""
	case strings.HasPrefix(s, "error:"):
		c.status, c.detail = "error", s
	case s == "closed":
		c.status, c.detail = "closed", ""
	default:
		c.status = s
	}
	c.mu.Unlock()
	if c.handlers.OnStatus != nil {
		c.handlers.OnStatus(s)
	}
}

func (c *Qwen3AsrHttpClient) bufSec() float64 {
	return float64(len(c.buf)) / (qwenSampleRate * 2)
}

// infer 整段推理，返回文本（失败返回空串）。
func (c *Qwen3AsrHttpClient) infer(pcm []byte) string {
	c.mu.Lock()
	model, language := c.model, c.language
	hotwords := append([]string(nil), c.hotwords...)
	c.mu.Unlock()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		return ""
	}
	if _, err := part.Write(pcmToWAV(pcm, qwenSampleRate)); err != nil {
		return ""
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("language", language)
	if len(hotwords) > 0 {
		_ = mw.WriteField("hotwords", strings.Join(hotwords, ","))
	}
	if err := mw.Close(); err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.inferTimeoutS*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		c.mu.Lock()
		c.inferFail++
		c.mu.Unlock()
		c.setStatus("error:infer:" + err.Error())
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		c.mu.Lock()
		c.inferFail++
		c.mu.Unlock()
		c.setStatus(fmt.Sprintf("error:infer:HTTP %d", resp.StatusCode))
		return ""
	}

	c.mu.Lock()
	c.inferFail = 0
	c.mu.Unlock()

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Text)
}

// finalize 句尾确认：整段推理 → 终稿。
func (c *Qwen3AsrHttpClient) finalize() {
	c.mu.Lock()
	snap := append([]byte(nil), c.buf...)
	speechSeen := c.speechSeen
	c.mu.Unlock()

	if len(snap) == 0 || !speechSeen {
		c.mu.Lock()
		c.buf = c.buf[:0]
		c.speechSeen = false
		c.lastPartial = 0
		c.mu.Unlock()
		return
	}

	text := c.infer(snap)

	c.mu.Lock()
	c.buf = c.buf[:0]
	c.frameBuf = c.frameBuf[:0]
	c.speechSeen = false
	c.inSpeech = false
	c.lastPartial = 0
	c.round++
	round := c.round
	c.mu.Unlock()

	if text != "" {
		c.mu.Lock()
		c.lastActive = time.Now()
		c.mu.Unlock()
		if c.handlers.OnOffline != nil {
			c.handlers.OnOffline(fmt.Sprintf("h%d", round), text)
		}
	}
	c.setStatus("connected")
}

// partial 周期草稿重推。
func (c *Qwen3AsrHttpClient) partial() {
	c.mu.Lock()
	snap := append([]byte(nil), c.buf...)
	c.mu.Unlock()
	if len(snap) == 0 {
		return
	}

	text := c.infer(snap)

	c.mu.Lock()
	expired := len(c.buf) < len(snap) // 推理期间已被 final 清空
	c.mu.Unlock()
	if expired || text == "" {
		return
	}
	c.mu.Lock()
	c.lastActive = time.Now()
	c.mu.Unlock()
	if c.handlers.OnOnline != nil {
		c.handlers.OnOnline(text)
	}
	c.setStatus("connected")
}

// scheduler 调度协程：partial 周期重推 + VAD 句尾 final + 超长强制切句。
func (c *Qwen3AsrHttpClient) scheduler() {
	defer close(c.done)
	// 兜住调度协程 panic，避免单次推理异常杀掉整个服务
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[asr] qwen3 调度协程 panic 已兜住: %v\n%s", r, debug.Stack())
		}
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-c.wake:
		case <-ticker.C:
		}

		c.mu.Lock()
		bufSec := c.bufSec()
		speechSeen := c.speechSeen
		silenceMS := c.silenceFrames * qwenFrameMS
		force := c.forceFinal
		inSpeech := c.inSpeech
		lastPartial := c.lastPartial
		c.mu.Unlock()

		if !speechSeen && !force {
			continue
		}
		// 1) 句尾静音 / mark_end / 超长 → final
		if (speechSeen && float64(silenceMS) >= float64(c.vadSilenceMS) && !inSpeech) ||
			force || bufSec >= c.maxSegmentS {
			c.mu.Lock()
			c.forceFinal = false
			c.mu.Unlock()
			c.finalize()
			continue
		}
		// 2) 周期草稿重推
		if speechSeen && bufSec-lastPartial >= c.partialInterval {
			c.mu.Lock()
			c.lastPartial = bufSec
			c.mu.Unlock()
			c.partial()
		}
	}
}

// pcmToWAV 为整段推理补 WAV 头。
func pcmToWAV(pcm []byte, sr int) []byte {
	buf := make([]byte, 44+len(pcm))
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+len(pcm)))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)           // fmt 块长度
	binary.LittleEndian.PutUint16(buf[20:22], 1)            // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1)            // mono
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sr))   // 采样率
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sr*2)) // 字节率
	binary.LittleEndian.PutUint16(buf[32:34], 2)            // 块对齐
	binary.LittleEndian.PutUint16(buf[34:36], 16)           // 位深
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(len(pcm)))
	copy(buf[44:], pcm)
	return buf
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
