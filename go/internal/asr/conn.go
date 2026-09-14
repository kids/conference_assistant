package asr

import (
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// safeCall 执行回调并兜住 panic。
// Go 里任何协程的未捕获 panic 都会终止整个进程（Python 版线程异常只影响该线程），
// 因此所有从协程里回调进业务代码的入口都必须显式 recover —— 否则一句转写的处理
// 出问题就会让整个直播服务挂掉。
func safeCall(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[asr] %s 回调 panic 已兜住: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}

// 连接参数。与 Python 版语义对齐处：
//   - 拨号超时 10s、退避公式 min(2^(retry-2), 15s) 完全一致；
//   - 退避上限 15s 是为了避免高频重试触发网关限流雪崩（Python 版注释中的实测结论）。
//
// 有意改进处（对应 Python 版的两个稳定性缺陷）：
//   - Python 版在持锁状态下 sleep 退避（最坏 ~25s），阻塞音频流水线线程，
//     且会让 /api/hotwords 请求挂住。此处改为"到点才允许重连"的时间戳判定，
//     重连在独立 goroutine 完成，调用方永不阻塞。
//   - 连接不可用期间直接丢帧，而不是无界堆积后补发（实时转写不应落后几十秒）。
const (
	dialTimeout     = 10 * time.Second
	writeTimeout    = 10 * time.Second
	readIdleTimeout = 3 * time.Minute // 读到任何消息即刷新；写失败是主检测手段，此值只兜底半开连接
	maxBackoff      = 15 * time.Second
)

// Conn 带异步重连的 WebSocket 连接管理器。
type Conn struct {
	url     string
	headers http.Header

	// handshake 连接建立后、开始收包前执行（发送协议初始化帧）。返回错误则视为连接失败。
	handshake func(ws *websocket.Conn) error
	// onConnected 握手成功后的本地状态复位（轮次递增、句子计数清零等）。
	onConnected func()
	onText      func(string)
	onBinary    func([]byte)
	onStatus    func(raw string)

	mu          sync.Mutex
	ws          *websocket.Conn
	state       string // idle / connecting / connected / error / closed
	detail      string
	lastActive  time.Time
	retry       int
	nextRetryAt time.Time
	connecting  bool
	closed      bool

	wmu sync.Mutex // 串行化写操作（gorilla 允许单读单写）
}

func newConn(url string, headers http.Header) *Conn {
	return &Conn{
		url:     url,
		headers: headers,
		state:   "idle",
	}
}

// State 归一化状态（供 /api/health）。
func (c *Conn) State() (state, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.detail
}

// IdleSec 距最近一次收到服务端消息的秒数（无消息返回 0）。
func (c *Conn) IdleSec() float64 {
	c.mu.Lock()
	last := c.lastActive
	c.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	return math.Round(time.Since(last).Seconds()*10) / 10
}

func (c *Conn) setState(state, detail string) {
	c.mu.Lock()
	c.state, c.detail = state, detail
	c.mu.Unlock()
}

// Connect 主动建立连接（不阻塞调用方）。流水线启动时预热，避免开头丢帧。
func (c *Conn) Connect() {
	c.mu.Lock()
	if c.closed || c.ws != nil || c.connecting {
		c.mu.Unlock()
		return
	}
	c.connecting = true
	c.mu.Unlock()
	go c.dial()
}

// get 返回可用连接；不可用时按退避策略触发一次异步重连并返回 nil。
func (c *Conn) get() *websocket.Conn {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.ws != nil {
		ws := c.ws
		c.mu.Unlock()
		return ws
	}
	if !c.connecting && !time.Now().Before(c.nextRetryAt) {
		c.connecting = true
		c.mu.Unlock()
		go c.dial()
		return nil
	}
	c.mu.Unlock()
	return nil
}

func backoffDelay(retry int) time.Duration {
	// 对应 Python: delay = min(2 ** max(self._retry - 2, 0), 15.0)
	e := retry - 2
	if e < 0 {
		e = 0
	}
	d := time.Duration(math.Pow(2, float64(e))) * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func (c *Conn) dial() {
	// 兜住建连/握手期间的 panic，避免单个连接的异常拖垮整个服务
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[asr] 建连协程 panic 已兜住: %v\n%s", r, debug.Stack())
		}
	}()

	handshakeOK := false
	defer func() {
		c.mu.Lock()
		c.connecting = false
		c.mu.Unlock()
		if !handshakeOK {
			// 失败：安排下次重连时间（不阻塞任何调用方）
			c.mu.Lock()
			delay := backoffDelay(c.retry)
			c.retry++
			c.nextRetryAt = time.Now().Add(delay)
			attempt := c.retry
			c.mu.Unlock()
			_ = attempt
		}
	}()

	dialer := websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		Proxy:            http.ProxyFromEnvironment,
	}
	ws, resp, err := dialer.Dial(c.url, c.headers)
	if err != nil {
		detail := err.Error()
		if resp != nil {
			detail = resp.Status
		}
		c.setState("connecting", detail)
		c.emitStatus("connect_failed:" + detail)
		return
	}

	if c.handshake != nil {
		if err := c.handshake(ws); err != nil {
			ws.Close()
			c.setState("connecting", err.Error())
			c.emitStatus("connect_failed:" + err.Error())
			return
		}
	}
	handshakeOK = true

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		ws.Close()
		return
	}
	c.ws = ws
	c.retry = 0
	c.nextRetryAt = time.Time{}
	c.state = "connected"
	c.detail = ""
	c.lastActive = time.Now()
	c.mu.Unlock()

	if c.onConnected != nil {
		c.onConnected()
	}
	c.emitStatus("connected")
	go c.readLoop(ws)
}

func (c *Conn) readLoop(ws *websocket.Conn) {
	for {
		_ = ws.SetReadDeadline(time.Now().Add(readIdleTimeout))
		mt, data, err := ws.ReadMessage()
		if err != nil {
			c.fail(ws, err)
			return
		}
		c.mu.Lock()
		c.lastActive = time.Now()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}
		switch mt {
		case websocket.TextMessage:
			if c.onText != nil {
				safeCall("onText", func() { c.onText(string(data)) })
			}
		case websocket.BinaryMessage:
			if c.onBinary != nil {
				safeCall("onBinary", func() { c.onBinary(data) })
			}
		}
	}
}

// fail 标记连接失败：立刻允许下次重连（由下一次 send 触发）。
func (c *Conn) fail(ws *websocket.Conn, err error) {
	c.mu.Lock()
	if c.ws == ws {
		c.ws = nil
	}
	if c.closed {
		c.mu.Unlock()
		ws.Close()
		return
	}
	c.state = "error"
	c.detail = err.Error()
	c.nextRetryAt = time.Now()
	c.mu.Unlock()

	ws.Close()
	c.emitStatus("error:" + err.Error())
}

// send 发送一帧。返回 false 表示当前连接不可用（帧被丢弃）。
func (c *Conn) send(data []byte, binary bool) bool {
	ws := c.get()
	if ws == nil {
		return false
	}
	mt := websocket.TextMessage
	if binary {
		mt = websocket.BinaryMessage
	}
	c.wmu.Lock()
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := ws.WriteMessage(mt, data)
	c.wmu.Unlock()
	if err != nil {
		c.fail(ws, err)
		return false
	}
	return true
}

// sendText 发送文本帧。
func (c *Conn) sendText(s string) bool { return c.send([]byte(s), false) }

// sendBinary 发送二进制帧。
func (c *Conn) sendBinary(b []byte) bool { return c.send(b, true) }

// disconnect 主动断开当前连接（热词变更后触发重连用），不关闭管理器。
func (c *Conn) disconnect() {
	c.mu.Lock()
	ws := c.ws
	c.ws = nil
	c.closed = false
	c.state = "connecting"
	c.detail = ""
	c.nextRetryAt = time.Now()
	c.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
}

// Close 关闭管理器与连接。
func (c *Conn) Close() {
	c.mu.Lock()
	ws := c.ws
	c.ws = nil
	c.closed = true
	c.state = "closed"
	c.detail = ""
	c.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
}

func (c *Conn) emitStatus(raw string) {
	if c.onStatus != nil {
		c.onStatus(raw)
	}
}
