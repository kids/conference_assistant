// Package state AI 展示状态机 + 全局急停开关。
// 状态流转（本版无 TTS，AI 输出仅投屏展示）：
//
//	IDLE → GENERATING → PENDING_APPROVAL → SHOWING → IDLE
package state

import (
	"context"
	"sync"
	"sync/atomic"
)

// AIState 展示状态。
type AIState string

const (
	IDLE            AIState = "IDLE"
	GENERATING      AIState = "GENERATING"
	PENDINGAPPROVAL AIState = "PENDING_APPROVAL"
	SHOWING         AIState = "SHOWING"
)

// Display 状态机。kill 用 atomic.Bool，读取无需加锁，便于在流式生成循环里高频检查。
type Display struct {
	mu      sync.Mutex
	state   AIState
	current string // invocation_id
	task    string
	kill    atomic.Bool
	// cancelGen 当前在飞生成的取消函数。
	// 必需：hy3 思考阶段只流 reasoning_content，正文 delta 一个都没有，
	// 靠"每个 delta 查一次急停标志"在思考期间完全失效 —— 实测按下急停后
	// 仍要等 10~30s 思考结束才真正中止。用 context 取消才能真正立即中断。
	cancelGen context.CancelFunc
}

// SetCancel 注册当前生成的中止函数；传 nil 表示生成已结束。
func (d *Display) SetCancel(fn context.CancelFunc) {
	d.mu.Lock()
	d.cancelGen = fn
	d.mu.Unlock()
}

// New 创建初始为 IDLE 的状态机。
func New() *Display { return &Display{state: IDLE} }

// State 当前状态。
func (d *Display) State() AIState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// Current 当前 invocation_id（无则空串）。
func (d *Display) Current() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

// Task 当前任务键（无则空串）。
func (d *Display) Task() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.task
}

// BeginGenerate 进入生成态并清除上一次的急停标记。
func (d *Display) BeginGenerate(invocationID, task string) {
	d.kill.Store(false)
	d.mu.Lock()
	d.state = GENERATING
	d.current = invocationID
	d.task = task
	d.mu.Unlock()
}

// Ready 生成完成，等待操作员确认投屏。
func (d *Display) Ready() {
	d.mu.Lock()
	d.state = PENDINGAPPROVAL
	d.mu.Unlock()
}

// Show 进入投屏态。
func (d *Display) Show() {
	d.mu.Lock()
	d.state = SHOWING
	d.mu.Unlock()
}

// Clear 只清展示状态（state/current/task → IDLE），**不动急停标记**。
//
// 用于急停/丢弃：既要把卡片从大屏撤下、让 state/current 不再指向那张卡，
// 又必须保留急停标记 —— 若在这里把它清掉，正在飞的那次生成就永远不会中止，
// 急停的主功能反而失效。
func (d *Display) Clear() {
	d.mu.Lock()
	d.state = IDLE
	d.current = ""
	d.task = ""
	d.mu.Unlock()
}

// Reset 回到 IDLE 并清除急停标记。
func (d *Display) Reset() {
	d.kill.Store(false)
	d.mu.Lock()
	d.state = IDLE
	d.current = ""
	d.task = ""
	d.mu.Unlock()
}

// Kill 立即中止生成 / 下屏。幂等。
// 除了置标志，还会直接取消在飞生成的 context —— 思考阶段没有正文 delta，
// 只置标志的话要等思考结束才生效。
func (d *Display) Kill() {
	d.kill.Store(true)
	d.mu.Lock()
	fn := d.cancelGen
	d.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Killed 是否已急停。
func (d *Display) Killed() bool { return d.kill.Load() }
