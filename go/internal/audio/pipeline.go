package audio

import (
	"log"
	"runtime/debug"
	"time"

	"seat/internal/asr"
)

// preRollFrames 起句前保留的帧数：300ms / 20ms = 15 帧，避免吞掉句首。
const preRollFrames = 15

// maxSpanSeconds 单段语音最多保留的音频秒数（说话人区分用）。
// 声纹判定用不着更长的音频，但长会议里「一句话说 15s 以上」很常见，
// 留个上限避免异常情况下内存无界增长。
const maxSpanSeconds = 30

// Pipeline 音频主流水线：采集 → VAD 断句 → ASR 流式 → 事件总线。
// 单 goroutine 顺序处理，帧级处理开销实测约 0.08% 实时，无需并行。
type Pipeline struct {
	capture   Capture
	vad       *VadSegmenter
	asr       asr.Client
	ring      *RingBuffer
	serverVAD bool // true：服务端自动断句，持续透传（含静音帧）

	// 说话人区分（可选）：把「一次连续说话」的音频交给 spanSink。
	// serverVAD 为 true 时本地 VAD 不参与断句，此时用 monitor 单独监测语音边界
	// （只做统计，不影响 ASR；webrtcvad 开销约 0.08% 实时）。
	spanSink func(start, end float64, pcm []byte)
	monitor  *VadSegmenter
	spanOn   bool
	spanFrom float64
	spanBuf  []byte

	preRoll [][]byte
	stop    chan struct{}
	done    chan struct{}

	segID    int
	tStart   float64
	inSpeech bool
}

// NewPipeline 创建流水线。serverVAD 为 true 时 vad 可为 nil。
func NewPipeline(capture Capture, vad *VadSegmenter, asrClient asr.Client, ring *RingBuffer, serverVAD bool) *Pipeline {
	return &Pipeline{
		capture:   capture,
		vad:       vad,
		asr:       asrClient,
		ring:      ring,
		serverVAD: serverVAD,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// SetSpanSink 启用「语音段音频」采集（供说话人区分用）。
// monitor 非空时用它监测语音边界（serverVAD 场景：本地 VAD 不参与 ASR 断句，
// 但仍需要「一次连续说话」的起止）；为空则复用 p.vad。
// 回调在流水线 goroutine 内同步执行，实现方必须立即返回（不得阻塞）。
func (p *Pipeline) SetSpanSink(monitor *VadSegmenter, sink func(start, end float64, pcm []byte)) {
	p.spanSink = sink
	if monitor != nil {
		p.monitor = monitor
	}
}

// Start 启动流水线 goroutine。
func (p *Pipeline) Start() { go p.run() }

// Stop 请求停止（非阻塞）。
func (p *Pipeline) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// Done 流水线退出信号。
func (p *Pipeline) Done() <-chan struct{} { return p.done }

// Alive 流水线是否仍在运行。
func (p *Pipeline) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *Pipeline) stopped() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func (p *Pipeline) run() {
	defer close(p.done)
	// 兜住流水线协程的 panic：转写链路出问题时应记录并停止，而不是杀掉整个进程
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[audio] 流水线 panic 已兜住: %v\n%s", r, debug.Stack())
		}
	}()
	defer func() {
		p.asr.Close()
		p.capture.Stop()
	}()

	emptyRun := 0
	for !p.stopped() {
		frame, ok := p.capture.Read(500 * time.Millisecond)
		if p.stopped() {
			break
		}
		if !ok {
			// 浏览器收音：等待远端连接期间无限等待，不因无帧自动结束
			if p.capture.KeepAlive() {
				continue
			}
			// 回放结束等无帧输入时，空转超过 15 秒自动结束流水线，
			// 避免 ASR 长连接悬挂到服务端超时
			emptyRun++
			if emptyRun > 30 {
				break
			}
			continue
		}
		emptyRun = 0
		p.ring.Push(frame)
		p.pushPreRoll(frame)

		if p.serverVAD {
			// 服务端自动断句：持续透传（含静音帧）
			p.asr.SendAudio(frame)
			if p.monitor != nil {
				p.trackSpan(p.monitor.Process(frame), frame)
			}
			continue
		}
		if p.vad == nil {
			continue
		}

		ev := p.vad.Process(frame)
		p.trackSpan(ev, frame)
		switch ev {
		case EventStart:
			p.beginSegment()
		case EventEnd:
			if p.inSpeech {
				p.asr.SendAudio(frame)
				p.endSegment()
			}
		case EventNone:
			if p.inSpeech {
				p.asr.SendAudio(frame)
			}
		}
	}
}

// trackSpan 收集「一次连续说话」的音频，段尾回调 spanSink。
// 与 ASR 断句相互独立：即使 ASR 走服务端断句（serverVAD），这里仍按本地 VAD 切段，
// 因为说话人区分只需要「这一句是谁说的」，不需要与 ASR 的服务端语义断句完全一致。
func (p *Pipeline) trackSpan(ev Event, frame []byte) {
	if p.spanSink == nil {
		return
	}
	switch ev {
	case EventStart:
		p.spanOn = true
		p.spanFrom = nowSeconds()
		// 回放 pre-roll（含触发 start 的那几帧），避免吞掉句首影响声纹
		p.spanBuf = p.spanBuf[:0]
		for _, f := range p.preRoll {
			p.appendSpan(f)
		}
	case EventEnd:
		if !p.spanOn {
			return
		}
		p.spanOn = false
		p.appendSpan(frame)
		from, pcm := p.spanFrom, p.spanBuf
		p.spanBuf = nil
		p.spanSink(from, nowSeconds(), pcm)
	case EventNone:
		if p.spanOn {
			p.appendSpan(frame)
		}
	}
}

// appendSpan 追加一帧到当前语音段，超过 maxSpanSeconds 后不再增长。
func (p *Pipeline) appendSpan(frame []byte) {
	if len(p.spanBuf) >= maxSpanSeconds*16000*2 {
		return
	}
	p.spanBuf = append(p.spanBuf, frame...)
}

func (p *Pipeline) pushPreRoll(frame []byte) {
	p.preRoll = append(p.preRoll, frame)
	if len(p.preRoll) > preRollFrames {
		p.preRoll = p.preRoll[len(p.preRoll)-preRollFrames:]
	}
}

func (p *Pipeline) beginSegment() {
	p.inSpeech = true
	p.segID++
	p.tStart = nowSeconds()
	// 回放 pre-roll（含触发 start 的那几帧）
	for _, f := range p.preRoll {
		p.asr.SendAudio(f)
	}
	p.preRoll = p.preRoll[:0]
}

func (p *Pipeline) endSegment() {
	p.inSpeech = false
	p.asr.MarkEnd()
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
