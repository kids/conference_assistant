package asr

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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
//   - 保护：单段达 max_segment_s 强制切句；纯静音段不推理（避免整段静音识别出「嗯。」）；
//     启动冷静期（见下方 warmup 常量）：丢弃采集启动瞬态的「类语音」环境声，避免静音幻觉。
const (
	qwenFrameMS    = 30
	qwenSampleRate = 16000
	qwenFrameBytes = qwenSampleRate * 2 * qwenFrameMS / 1000

	// qwenInferAttempts 单次推理的最大尝试次数（=1 次重试）。
	// 服务端在并发时会秒级到分钟级地排队：实测同一段 20s 音频的延迟在 0.8s~60s 之间波动。
	// 旧版单次超时即丢一整句，观感就是「字幕时有时无」；重试一次常能命中空闲窗口。
	qwenInferAttempts = 2
	// qwenRetryBackoff 重试前等待，给服务端一点排空时间。
	qwenRetryBackoff = 400 * time.Millisecond

	// 启动冷静期参数（总时长由配置 QWEN3_WARMUP_MS 控制，0=关闭）。
	// 采集启动瞬间（浏览器 AGC 冲激、设备瞬态）会产生 2~5 秒的「类语音」环境声：
	// 实测 webrtcvad 判其 99% 为语音（真实说话仅 83%，比真语音还像语音，故 VAD 门槛类
	// 方案无效）；整段送推理则触发 LLM 式 ASR 的百科式幻觉（"《小王子》是法国作家…"，
	// 同一段音频衰减 30dB 仍复现，与电平无关）。对策：启动后的前 warmupMS 毫秒音频
	// 不进推理；一旦检出「真实说话起始」（显著高于环境基线的电平突增）立即提前结束。
	// 判据经真实录音校准（浏览器采集实测：启动瞬态 -36~-42dBFS、稳定底噪 -50~-58、
	// 真实说话元音 -30 以上；判据过松会在 0.8s 就把瞬态误判为说话起始，幻觉源漏过）。
	qwenWarmupDeltaDB    = 12.0  // 说话起始判据：高于环境基线该幅度
	qwenWarmupFloorDB    = -32.0 // 绝对下限：启动瞬态/底噪在 -35 以下，说话元音在 -30 以上
	qwenWarmupRunFrames  = 5     // 连续超阈帧数（150ms），防偶发尖峰误触发
	qwenWarmupBaseFrames = 10    // 环境基线有效帧数下限（300ms）
	qwenWarmupValidDB    = -60.0 // 基线只统计高于该电平的帧（排除启动时的数字静音）
	qwenWarmupPreBufMS   = 500   // 提前结束时保留的说话起点前上下文（避免切掉话头）
)

// inferHTTPError 非 200 响应。单独成类型是为了区分「值得重试」与「重试也没用」：
// 4xx 是请求本身的问题（模型名、参数非法），重试只会得到同样结果。
type inferHTTPError struct {
	Status int
	Body   string
}

func (e *inferHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, e.Body)
}

// retryableInferErr 超时/连接失败/响应解析失败/5xx → 重试；4xx → 不重试。
func retryableInferErr(err error) bool {
	var he *inferHTTPError
	if errors.As(err, &he) {
		return he.Status >= 500
	}
	return true
}

// langMap 语言别名 → ISO 639-1（vLLM transcriptions 仅接受 ISO 码）。
var langMap = map[string]string{
	"中文": "zh", "汉语": "zh", "英语": "en", "英文": "en", "日语": "ja", "韩语": "ko",
}

// Qwen3AsrHttpClient Qwen3-ASR HTTP 转写客户端（模拟流式）。
type Qwen3AsrHttpClient struct {
	baseURL         string
	model           string
	language        string
	partialInterval float64
	vadSilenceMS    int
	maxSegmentS     float64
	inferTimeoutS   float64
	handlers        Handlers

	http *http.Client

	mu            sync.Mutex
	hotwords      []string
	buf           []byte // 自上一确认句起的音频
	frameBuf      []byte // 任意 chunk → 30ms 帧重组
	inSpeech      bool
	speechFrames  int
	silenceFrames int
	speechSeen    bool
	lastPartial   float64
	round         int
	forceFinal    bool
	status        string
	detail        string
	lastActive    time.Time
	inferFail     int

	// 启动冷静期状态（见 warmup 常量注释）。clock 可注入，测试用假时钟推进。
	warmupMS    int
	warmupUntil time.Time
	warmupDone  bool
	warmupBase  float64
	warmupBaseN int
	warmupRun   int
	preBuf      []byte
	clock       func() time.Time

	vad  *webrtcvad.VAD
	stop chan struct{}
	done chan struct{}
	wake chan struct{}
}

// NewQwen3AsrHttpClient 创建客户端并启动调度协程。
// warmupMS 为启动冷静期时长（0=关闭，见 warmup 常量注释）。
func NewQwen3AsrHttpClient(baseURL, model, language string, hotwords []string,
	partialIntervalS float64, vadSilenceMS, vadAggressiveness int, maxSegmentS, inferTimeoutS float64,
	warmupMS int, h Handlers) (*Qwen3AsrHttpClient, error) {

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
		warmupMS:        warmupMS,
		clock:           time.Now,
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

// feedFrame 帧入口：冷静期内走 warmupFrame（丢弃采集启动瞬态），否则正常消费。
func (c *Qwen3AsrHttpClient) feedFrame(frame []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warmupActive() {
		c.warmupFrame(frame)
		return
	}
	c.consumeFrame(frame)
}

// consumeFrame 正常路径（持锁调用）：累积缓冲并更新 VAD 状态机。
func (c *Qwen3AsrHttpClient) consumeFrame(frame []byte) {
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

// warmupActive 是否处于启动冷静期（持锁调用）。懒启动：首帧到达时开始计时。
// 超时未见说话起始 → 结束冷静期并丢弃期间收集的瞬态音频，当前帧起恢复正常处理。
func (c *Qwen3AsrHttpClient) warmupActive() bool {
	if c.warmupMS <= 0 || c.warmupDone {
		return false
	}
	now := c.clock()
	if c.warmupUntil.IsZero() {
		c.warmupUntil = now.Add(time.Duration(c.warmupMS) * time.Millisecond)
		log.Printf("[asr] qwen3 启动冷静期 %dms：丢弃采集启动瞬态（AGC 冲激等环境声会诱发静音幻觉）", c.warmupMS)
	}
	if !now.Before(c.warmupUntil) {
		c.warmupDone = true
		c.preBuf = nil
		log.Printf("[asr] qwen3 冷静期结束：未检出说话起始，启动瞬态音频已丢弃")
		return false
	}
	return true
}

// warmupFrame 冷静期内的帧处理：估计环境电平基线；检出真实说话起始则提前结束
// 冷静期，并把起点前 qwenWarmupPreBufMS 的上下文一并交出（避免切掉话头）。
func (c *Qwen3AsrHttpClient) warmupFrame(frame []byte) {
	db := frameDBFS(frame)
	if c.warmupBaseN < qwenWarmupBaseFrames {
		// 基线只统计有效帧：采集刚启动时可能是数字静音/极低电平（实测 -90dBFS 以下），
		// 若计入会把基线拉到 -75 之类，使相对判据失效。
		if db > qwenWarmupValidDB {
			c.warmupBase += (db - c.warmupBase) / float64(c.warmupBaseN+1)
			c.warmupBaseN++
		}
		c.preBufAppend(frame)
		return
	}
	if db > c.warmupBase+qwenWarmupDeltaDB && db > qwenWarmupFloorDB {
		c.warmupRun++
	} else {
		c.warmupRun = 0
	}
	if c.warmupRun < qwenWarmupRunFrames {
		c.preBufAppend(frame)
		return
	}
	c.warmupDone = true
	log.Printf("[asr] qwen3 冷静期检出说话起始，提前结束（保留起点前 %dms 上下文）",
		len(c.preBuf)*1000/(qwenSampleRate*2))
	pend := c.preBuf
	c.preBuf = nil
	for i := 0; i+qwenFrameBytes <= len(pend); i += qwenFrameBytes {
		c.consumeFrame(pend[i : i+qwenFrameBytes])
	}
	c.consumeFrame(frame)
}

// preBufAppend 冷静期内滚动保留最近 qwenWarmupPreBufMS 的音频。
func (c *Qwen3AsrHttpClient) preBufAppend(frame []byte) {
	c.preBuf = append(c.preBuf, frame...)
	max := qwenWarmupPreBufMS * qwenSampleRate * 2 / 1000
	if len(c.preBuf) > max {
		c.preBuf = append(c.preBuf[:0], c.preBuf[len(c.preBuf)-max:]...)
	}
}

// frameDBFS 30ms 帧的 RMS 电平（dBFS，满量程 0dB）。
func frameDBFS(frame []byte) float64 {
	n := len(frame) / 2
	if n == 0 {
		return -120
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[i*2:])))
		sum += s * s
	}
	rms := math.Sqrt(sum / float64(n))
	if rms < 1 {
		return -120
	}
	return 20 * math.Log10(rms/32768)
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
	return StatusInfo{State: c.status, Detail: c.detail, IdleSec: round1(idle), InferFail: c.inferFail}
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
//
// 鲁棒性：失败重试一次，并把结果反映到状态与日志上。旧版单次超时就静默返回空串，
// 且紧随其后的 setStatus("connected") 会把错误覆盖掉 —— 结果「有语音但推理一直失败」
// 与「确实没语音」在页面上长得一模一样，只能靠猜。
func (c *Qwen3AsrHttpClient) infer(pcm []byte) string {
	var lastErr error
	for attempt := 1; attempt <= qwenInferAttempts; attempt++ {
		text, err := c.inferOnce(pcm)
		if err == nil {
			c.mu.Lock()
			prevFail := c.inferFail
			c.inferFail = 0
			c.mu.Unlock()
			if prevFail > 0 {
				log.Printf("[asr] qwen3 推理恢复正常（此前连续失败 %d 次）", prevFail)
			}
			c.setStatus("connected")
			return text
		}
		lastErr = err
		if !retryableInferErr(err) {
			break
		}
		if attempt < qwenInferAttempts {
			log.Printf("[asr] qwen3 推理失败（第 %d/%d 次，超时 %.0fs）：%v —— 重试",
				attempt, qwenInferAttempts, c.inferTimeoutS, err)
			select {
			case <-time.After(qwenRetryBackoff):
			case <-c.stop:
				return ""
			}
		}
	}
	c.mu.Lock()
	c.inferFail++
	fails := c.inferFail
	c.mu.Unlock()
	log.Printf("[asr] qwen3 推理失败（连续 %d 次，本段丢弃）：%v", fails, lastErr)
	c.setStatus("error:infer:" + lastErr.Error())
	return ""
}

// inferOnce 单次推理尝试；err 非空表示这次尝试失败（含 HTTP 非 200）。
func (c *Qwen3AsrHttpClient) inferOnce(pcm []byte) (string, error) {
	c.mu.Lock()
	model, language := c.model, c.language
	hotwords := append([]string(nil), c.hotwords...)
	c.mu.Unlock()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "a.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(pcmToWAV(pcm, qwenSampleRate)); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("language", language)
	if len(hotwords) > 0 {
		_ = mw.WriteField("hotwords", strings.Join(hotwords, ","))
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.inferTimeoutS*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", &inferHTTPError{Status: resp.StatusCode, Body: msg}
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("响应解析失败: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
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
	// 不在这里设 "connected"：状态由 infer 统一负责，否则会把刚发生的推理错误覆盖掉。
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
	// 同上：不覆盖 infer 已设好的状态。
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
