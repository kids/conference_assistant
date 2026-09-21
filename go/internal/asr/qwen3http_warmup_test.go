package asr

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// fakeClock 可注入假时钟：帧循环里按 30ms/帧推进，避免测试真实等待数秒。
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

// newWarmupClient 构造带假时钟的客户端（不访问后端，地址不可达也无妨）。
func newWarmupClient(t *testing.T, warmupMS int, clk *fakeClock) *Qwen3AsrHttpClient {
	t.Helper()
	c, err := NewQwen3AsrHttpClient("http://127.0.0.1:1", "qwen3asr17b", "中文", nil,
		1.0, 600, 2, 15, 2.0, warmupMS, Handlers{})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	c.clock = clk.now
	t.Cleanup(c.Close)
	return c
}

// genPCM 生成 frames 帧、幅度 amp（相对满量程）的类语音信号（220Hz 正弦，帧内幅度平稳）。
// amp=0.008 ≈ -45dBFS（环境底噪）；amp=0.2 ≈ -17dBFS（真实说话量级）。
func genPCM(frames int, amp float64) [][]byte {
	out := make([][]byte, frames)
	phase := 0.0
	for f := 0; f < frames; f++ {
		buf := make([]byte, qwenFrameBytes)
		for i := 0; i < qwenFrameBytes/2; i++ {
			v := int16(amp * 32767 * math.Sin(phase))
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
			phase += 2 * math.Pi * 220 / qwenSampleRate
		}
		out[f] = buf
	}
	return out
}

// feedFrames 逐帧送入并推进假时钟（模拟真实 30ms/帧的到达节奏）。
func feedFrames(c *Qwen3AsrHttpClient, clk *fakeClock, frames [][]byte) {
	for _, f := range frames {
		clk.advance(time.Duration(qwenFrameMS) * time.Millisecond)
		c.SendAudio(f)
	}
}

// closeScheduler 停掉调度协程后再断言，避免 final 清空缓冲造成断言竞态。
func closeScheduler(c *Qwen3AsrHttpClient) {
	c.Close()
}

// TestWarmupDropsAmbientAudio：冷静期内的低电平环境声（AGC 冲激类）不进入推理缓冲
// —— 这是静音幻觉（"《小王子》是…"）的源头，必须被拦在模型之外。
func TestWarmupDropsAmbientAudio(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWarmupClient(t, 3000, clk)

	feedFrames(c, clk, genPCM(90, 0.008)) // 2.7s 环境声（<3s 冷静期）
	closeScheduler(c)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) != 0 {
		t.Errorf("冷静期内环境声不应进入推理缓冲，实际 %d 字节", len(c.buf))
	}
	if c.warmupDone {
		t.Errorf("2.7s 时冷静期（3s）不应已结束")
	}
}

// TestWarmupDetectsSpeechOnset：冷静期内检出真实说话起始（电平显著高于基线）应立即
// 提前结束，并保留说话起点前 ~500ms 上下文（不切掉话头）。
func TestWarmupDetectsSpeechOnset(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWarmupClient(t, 3000, clk)

	feedFrames(c, clk, genPCM(20, 0.008)) // 0.6s 环境声（前 10 帧建立基线）
	feedFrames(c, clk, genPCM(10, 0.2))   // 说话：连续 3 帧即触发提前结束
	closeScheduler(c)

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.warmupDone {
		t.Fatalf("应检出说话起始并结束冷静期")
	}
	if len(c.buf) <= 10*qwenFrameBytes {
		t.Errorf("应保留说话起点前上下文 + 说话帧，实际缓冲仅 %d 字节", len(c.buf))
	}
	if len(c.preBuf) != 0 {
		t.Errorf("preBuf 应在提前结束时交出并清空，实际残留 %d 字节", len(c.preBuf))
	}
}

// TestWarmupTimeoutLetsAudioThrough：冷静期内始终未说话（纯环境声）时，超时后应从
// 当前帧起恢复正常处理（丢弃的是启动瞬态，不是长期静音保护）。
func TestWarmupTimeoutLetsAudioThrough(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWarmupClient(t, 300, clk) // 冷静期 300ms = 10 帧

	feedFrames(c, clk, genPCM(20, 0.008)) // 0.6s
	closeScheduler(c)

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.warmupDone {
		t.Fatalf("超过冷静期后应自动结束")
	}
	// 超时点在 300ms（第 10 帧边界），其后约 10 帧应进入缓冲
	if n := len(c.buf); n < 8*qwenFrameBytes || n > 12*qwenFrameBytes {
		t.Errorf("超时后应约 10 帧进入缓冲，实际 %d 字节（%d 帧）", n, n/qwenFrameBytes)
	}
}

// TestWarmupDisabled：QWEN3_WARMUP_MS=0 时行为与改动前一致（音频全部直接进入缓冲）。
func TestWarmupDisabled(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWarmupClient(t, 0, clk)

	feedFrames(c, clk, genPCM(3, 0.008))
	closeScheduler(c)

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) != 3*qwenFrameBytes {
		t.Errorf("冷静期关闭时音频应全部进入缓冲，实际 %d 字节", len(c.buf))
	}
	if c.warmupDone || !c.warmupUntil.IsZero() {
		t.Errorf("关闭状态下不应启动冷静期状态机")
	}
}
