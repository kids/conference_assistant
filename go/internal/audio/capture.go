package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// Capture 音频源统一接口：输出固定 20ms 的 16k/mono/int16 PCM 帧。
type Capture interface {
	// Read 读取一帧；超时返回 (nil, false)。
	Read(timeout time.Duration) ([]byte, bool)
	// Stop 停止采集并释放资源。
	Stop()
	// KeepAlive 无帧时是否无限等待（浏览器收音：远端未连接也不结束流水线）。
	KeepAlive() bool
}

// 采集队列深度：500 帧 = 10 秒。超出后丢弃最旧帧。
// Python 版用无界 queue.Queue，长时间断连会持续堆积（约 32KB/s），此处改为有界丢帧：
// 实时转写场景下，丢弃陈旧音频远优于让转写落后几十秒。
const captureQueueFrames = 500

// ---------------------------------------------------------------------------
// 浏览器收音
// ---------------------------------------------------------------------------

// BrowserCapture 远端浏览器收音：前端 getUserMedia 采集 PCM，经 /ws/audio 推入本类队列。
// 浏览器推来的音频可以是任意块大小，内部重切成固定帧。
type BrowserCapture struct {
	sampleRate int
	blockBytes int

	mu     sync.Mutex
	buf    []byte
	q      chan []byte
	closed bool

	connectedMu sync.Mutex
	connected   bool
}

// NewBrowserCapture 创建浏览器收音源。
func NewBrowserCapture(sampleRate, blockMS int) *BrowserCapture {
	return &BrowserCapture{
		sampleRate: sampleRate,
		blockBytes: sampleRate * blockMS / 1000 * 2,
		q:          make(chan []byte, captureQueueFrames),
	}
}

// KeepAlive 恒为 true。
func (c *BrowserCapture) KeepAlive() bool { return true }

// Feed 供 /ws/audio 端点调用：追加任意大小 PCM 块，重切成固定帧入队。
func (c *BrowserCapture) Feed(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.buf = append(c.buf, pcm...)
	n := c.blockBytes
	var frames [][]byte
	for len(c.buf) >= n {
		f := make([]byte, n)
		copy(f, c.buf[:n])
		frames = append(frames, f)
		c.buf = c.buf[n:]
	}
	c.mu.Unlock()

	for _, f := range frames {
		select {
		case c.q <- f:
		default:
			// 队列满：丢最旧一帧再入队，保证内存有界
			select {
			case <-c.q:
			default:
			}
			select {
			case c.q <- f:
			default:
			}
		}
	}
}

// Read 读取一帧。
func (c *BrowserCapture) Read(timeout time.Duration) ([]byte, bool) {
	select {
	case f := <-c.q:
		return f, true
	case <-time.After(timeout):
		return nil, false
	}
}

// MarkConnected 标记浏览器已连上。
func (c *BrowserCapture) MarkConnected() {
	c.connectedMu.Lock()
	c.connected = true
	c.connectedMu.Unlock()
}

// MarkDisconnected 标记浏览器已断开。
func (c *BrowserCapture) MarkDisconnected() {
	c.connectedMu.Lock()
	c.connected = false
	c.connectedMu.Unlock()
}

// Connected 浏览器收音是否在线。
func (c *BrowserCapture) Connected() bool {
	c.connectedMu.Lock()
	defer c.connectedMu.Unlock()
	return c.connected
}

// Stop 停止采集。
func (c *BrowserCapture) Stop() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.MarkDisconnected()
}

// ---------------------------------------------------------------------------
// 本机声卡采集（malgo / miniaudio）
// ---------------------------------------------------------------------------

// MicrophoneCapture 从声卡/调音台/USB 麦采集。
// miniaudio 会自动做格式转换与重采样，因此直接请求 16k/mono/int16。
type MicrophoneCapture struct {
	sampleRate int
	blockMS    int
	q          chan []byte

	ctx  *malgo.AllocatedContext
	dev  *malgo.Device
	once sync.Once
}

// NewMicrophoneCapture 创建并启动采集。无音频设备时返回 error，由调用方降级为浏览器收音。
func NewMicrophoneCapture(sampleRate, blockMS int) (*MicrophoneCapture, error) {
	m := &MicrophoneCapture{
		sampleRate: sampleRate,
		blockMS:    blockMS,
		q:          make(chan []byte, captureQueueFrames),
	}
	if err := m.start(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *MicrophoneCapture) start() error {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("初始化音频上下文失败: %w", err)
	}
	m.ctx = ctx

	deviceCfg := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceCfg.Capture.Format = malgo.FormatS16
	deviceCfg.Capture.Channels = 1
	deviceCfg.SampleRate = uint32(m.sampleRate)
	deviceCfg.Alsa.NoMMap = 1

	blockFrames := uint32(m.sampleRate * m.blockMS / 1000)
	onRecv := func(_, input []byte, framecount uint32) {
		if len(input) == 0 {
			return
		}
		// 按固定块大小重切（设备回调帧数可能与请求块不等）
		buf := make([]byte, len(input))
		copy(buf, input)
		for off := 0; off+int(blockFrames)*2 <= len(buf); off += int(blockFrames) * 2 {
			f := make([]byte, blockFrames*2)
			copy(f, buf[off:off+int(blockFrames)*2])
			select {
			case m.q <- f:
			default:
				select {
				case <-m.q:
				default:
				}
				select {
				case m.q <- f:
				default:
				}
			}
		}
	}

	dev, err := malgo.InitDevice(ctx.Context, deviceCfg, malgo.DeviceCallbacks{Data: onRecv})
	if err != nil {
		ctx.Uninit()
		ctx.Free()
		m.ctx = nil
		return fmt.Errorf("打开音频输入设备失败: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		ctx.Uninit()
		ctx.Free()
		m.ctx = nil
		return fmt.Errorf("启动音频输入失败: %w", err)
	}
	m.dev = dev
	return nil
}

// KeepAlive 恒为 false（无声卡时不应无限空转）。
func (m *MicrophoneCapture) KeepAlive() bool { return false }

// Read 读取一帧。
func (m *MicrophoneCapture) Read(timeout time.Duration) ([]byte, bool) {
	select {
	case f := <-m.q:
		return f, true
	case <-time.After(timeout):
		return nil, false
	}
}

// Stop 停止并释放音频设备。
func (m *MicrophoneCapture) Stop() {
	m.once.Do(func() {
		if m.dev != nil {
			m.dev.Uninit()
			m.dev = nil
		}
		if m.ctx != nil {
			m.ctx.Uninit()
			m.ctx.Free()
			m.ctx = nil
		}
	})
}

// ---------------------------------------------------------------------------
// 本地文件回放（离线彩排 / 回归基线）
// ---------------------------------------------------------------------------

// FileReplayCapture 按真实时间轴回放本地 WAV/RAW PCM。
type FileReplayCapture struct {
	sampleRate int
	blockBytes int

	raw         []byte
	pos         int
	start       time.Time
	tailSilence []byte
}

// NewFileReplayCapture 打开回放文件。仅支持 16k/mono/int16 的 WAV 或裸 PCM。
func NewFileReplayCapture(path string, sampleRate, blockMS int) (*FileReplayCapture, error) {
	raw, err := loadReplayFile(path, sampleRate)
	if err != nil {
		return nil, err
	}
	tail := make([]byte, sampleRate*3*2) // 3 秒尾静音
	return &FileReplayCapture{
		sampleRate:  sampleRate,
		blockBytes:  sampleRate * blockMS / 1000 * 2,
		raw:         raw,
		start:       time.Now(),
		tailSilence: tail,
	}, nil
}

// KeepAlive 恒为 false（回放结束即结束流水线）。
func (c *FileReplayCapture) KeepAlive() bool { return false }

// Read 按真实时间节流读取一帧。
func (c *FileReplayCapture) Read(timeout time.Duration) ([]byte, bool) {
	elapsed := time.Since(c.start).Seconds()
	played := float64(c.pos/2) / float64(c.sampleRate)
	if played > elapsed {
		d := played - elapsed
		if d > 0.1 {
			d = 0.1
		}
		time.Sleep(time.Duration(d * float64(time.Second)))
	}

	if c.pos < len(c.raw) {
		end := c.pos + c.blockBytes
		if end > len(c.raw) {
			end = len(c.raw)
		}
		chunk := c.raw[c.pos:end]
		c.pos = end
		return chunk, true
	}
	if len(c.tailSilence) > 0 {
		n := c.blockBytes
		if n > len(c.tailSilence) {
			n = len(c.tailSilence)
		}
		chunk := c.tailSilence[:n]
		c.tailSilence = c.tailSilence[n:]
		time.Sleep(time.Duration(float64(c.blockBytes/2) / float64(c.sampleRate) * float64(time.Second)))
		return chunk, true
	}
	return nil, false
}

// Stop 无需释放资源。
func (c *FileReplayCapture) Stop() {}

// loadReplayFile 读 WAV（校验采样率）或裸 PCM。
func loadReplayFile(path string, sampleRate int) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(strings.ToLower(path), ".wav") {
		return raw, nil // 视为 RAW PCM int16 mono
	}
	return parseWAV(raw, sampleRate)
}

// parseWAV 最小 RIFF/WAVE 解析：只取 fmt 与 data 块。
func parseWAV(b []byte, wantRate int) ([]byte, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("不是合法的 WAV 文件")
	}
	var (
		data     []byte
		rate     int
		channels int
		bits     int
	)
	off := 12
	for off+8 <= len(b) {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		if body+size > len(b) {
			size = len(b) - body
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				channels = int(binary.LittleEndian.Uint16(b[body+2 : body+4]))
				rate = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
				bits = int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
			}
		case "data":
			data = b[body : body+size]
		}
		off = body + size + (size & 1) // 块按偶数字节对齐
	}
	if data == nil {
		return nil, errors.New("WAV 缺少 data 块")
	}
	if rate != wantRate {
		return nil, fmt.Errorf("回放文件采样率需为 %d，实际 %d", wantRate, rate)
	}
	if channels != 1 || bits != 16 {
		return nil, fmt.Errorf("回放文件需为 mono/16bit，实际 channels=%d bits=%d", channels, bits)
	}
	return data, nil
}
