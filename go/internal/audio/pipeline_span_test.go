package audio

import (
	"bytes"
	"testing"
)

// 说话人区分的音频采集（Pipeline.trackSpan）是「标记换人」的数据来源：
// 它出错的表现是"说话人标错/缺失"，从页面上很难归因，故用测试钉住。

func testFrame(b byte) []byte { return bytes.Repeat([]byte{b}, 640) }

func newSpanPipeline(sink func(float64, float64, []byte)) *Pipeline {
	// 直接构造，不跑采集/VAD：这里只验证 trackSpan 的缓冲与回调语义
	return &Pipeline{spanSink: sink}
}

func TestTrackSpanEmitsWholeSegment(t *testing.T) {
	var got [][]byte
	var start, end float64
	p := newSpanPipeline(func(s, e float64, pcm []byte) {
		start, end, got = s, e, append(got, pcm)
	})
	p.preRoll = [][]byte{testFrame(1), testFrame(2)} // 起句前 2 帧

	p.trackSpan(EventStart, testFrame(3))
	p.trackSpan(EventNone, testFrame(4))
	p.trackSpan(EventEnd, testFrame(5))

	if len(got) != 1 {
		t.Fatalf("段尾应回调一次，实际 %d 次", len(got))
	}
	// pre-roll 2 帧 + 中间 1 帧 + 段尾 1 帧（起句帧本身就在 pre-roll 里：
	// run() 先 pushPreRoll 再 Process，事件触发时该帧已入队）
	if want := 4 * 640; len(got[0]) != want {
		t.Errorf("段音频应含 pre-roll+中间+段尾共 %d 字节，实际 %d", want, len(got[0]))
	}
	if start <= 0 || end < start {
		t.Errorf("起止时间异常：start=%v end=%v", start, end)
	}
	// 段尾回调后缓冲必须清空，避免下一段把上一段音频带进去
	if p.spanBuf != nil {
		t.Errorf("段尾后 spanBuf 应清空，实际 %d 字节", len(p.spanBuf))
	}
	if p.spanOn {
		t.Error("段尾后 spanOn 应为 false")
	}
}

func TestTrackSpanIgnoresEndWithoutStart(t *testing.T) {
	calls := 0
	p := newSpanPipeline(func(float64, float64, []byte) { calls++ })
	p.trackSpan(EventEnd, testFrame(1))
	p.trackSpan(EventNone, testFrame(2))
	if calls != 0 {
		t.Errorf("未起句就段尾时不应回调，实际 %d 次", calls)
	}
}

func TestTrackSpanDisabledIsNoop(t *testing.T) {
	p := newSpanPipeline(nil)
	p.trackSpan(EventStart, testFrame(1))
	p.trackSpan(EventNone, testFrame(2))
	p.trackSpan(EventEnd, testFrame(3))
	if p.spanBuf != nil {
		t.Errorf("未启用时应完全不缓冲，实际 %d 字节", len(p.spanBuf))
	}
}

func TestTrackSpanCapsMemory(t *testing.T) {
	p := newSpanPipeline(func(float64, float64, []byte) {})
	p.trackSpan(EventStart, testFrame(1))
	// 灌入远超 30 秒的音频：缓冲必须封顶，否则异常情况下长会议内存无界增长
	for i := 0; i < maxSpanSeconds*50+200; i++ {
		p.trackSpan(EventNone, testFrame(2))
	}
	if max := maxSpanSeconds * 16000 * 2; len(p.spanBuf) > max {
		t.Errorf("段缓冲应封顶在 %d 字节，实际 %d", max, len(p.spanBuf))
	}
}

func TestSetSpanSinkKeepsExplicitMonitor(t *testing.T) {
	v, err := NewVadSegmenter(16000, 20, 600, 2, 15)
	if err != nil {
		t.Skipf("webrtcvad 不可用：%v", err)
	}
	p := &Pipeline{}
	p.SetSpanSink(v, func(float64, float64, []byte) {})
	if p.monitor != v {
		t.Error("显式传入的 monitor 应被保留（serverVAD 场景靠它产出语音段）")
	}
	// monitor 为 nil 时不应覆盖已有 monitor（复用 p.vad 的情形）
	p2 := &Pipeline{}
	p2.SetSpanSink(v, nil)
	p2.SetSpanSink(nil, nil)
	if p2.monitor != v {
		t.Error("monitor 传 nil 时不应清掉已设置的监测 VAD")
	}
}
