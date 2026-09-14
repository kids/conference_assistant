// Package audio 音频采集、断句与主流水线。
package audio

import "sync"

// RingBuffer 内存 PCM 环形缓冲：保留最近 N 秒原始音频，用于"刚才那个术语"回溯重识别。
type RingBuffer struct {
	mu  sync.Mutex
	cap int
	buf []byte
}

// NewRingBuffer seconds 秒容量（16bit mono）。
func NewRingBuffer(seconds float64, sampleRate int) *RingBuffer {
	return &RingBuffer{cap: int(seconds * float64(sampleRate) * 2)}
}

// Push 追加数据，超出容量时丢弃最旧部分。
func (r *RingBuffer) Push(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, data...)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

// Tail 取最近 seconds 秒数据。
func (r *RingBuffer) Tail(seconds float64, sampleRate int) []byte {
	n := int(seconds * float64(sampleRate) * 2)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.buf) {
		out := make([]byte, len(r.buf))
		copy(out, r.buf)
		return out
	}
	out := make([]byte, n)
	copy(out, r.buf[len(r.buf)-n:])
	return out
}

// Clear 清空缓冲。
func (r *RingBuffer) Clear() {
	r.mu.Lock()
	r.buf = r.buf[:0]
	r.mu.Unlock()
}
