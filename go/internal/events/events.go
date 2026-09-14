// Package events 进程内事件总线：采集/ASR/LLM 协程发布，WebSocket 订阅者消费。
// 对应 Python 版 app/events.py（原版用 asyncio.Queue + call_soon_threadsafe 跨线程投递；
// Go 版用带缓冲 channel 直接投递，无需线程/事件循环桥接）。
package events

import (
	"sync"
	"time"
)

// Event 事件载荷，等价于 Python 版的 dict。
type Event map[string]any

// subBuffer 每个订阅者的缓冲深度。单个订阅者消费过慢时丢弃新事件，
// 而不是无界堆积（Python 版用无界 asyncio.Queue，慢客户端会导致内存增长）。
const subBuffer = 2048

// Bus 线程安全（goroutine 安全）的发布订阅总线，带历史快照。
type Bus struct {
	mu      sync.Mutex
	subs    map[chan Event]struct{}
	history []Event
	max     int
	dropped uint64
}

// New 创建总线，historyMax 为历史事件保留条数。
func New(historyMax int) *Bus {
	return &Bus{subs: make(map[chan Event]struct{}), max: historyMax}
}

// Subscribe 注册订阅者，返回只读通道。调用方不再使用时必须 Unsubscribe。
func (b *Bus) Subscribe() chan Event {
	ch := make(chan Event, subBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe 注销并关闭通道。
func (b *Bus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Publish 广播事件；补 "t" 时间戳，写入历史，非阻塞投递给所有订阅者。
func (b *Bus) Publish(ev Event) {
	if _, ok := ev["t"]; !ok {
		ev["t"] = float64(time.Now().UnixNano()) / 1e9
	}

	b.mu.Lock()
	b.history = append(b.history, ev)
	if len(b.history) > b.max {
		b.history = b.history[len(b.history)-b.max:]
	}
	subs := make([]chan Event, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// 订阅者消费不过来：丢新事件，保证发布方永不被阻塞
			b.mu.Lock()
			b.dropped++
			b.mu.Unlock()
		}
	}
}

// Snapshot 返回历史事件副本（用于 WebSocket 连接建立时补发）。
func (b *Bus) Snapshot() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.history))
	copy(out, b.history)
	return out
}

// Dropped 返回因订阅者积压而丢弃的事件总数。
func (b *Bus) Dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}
