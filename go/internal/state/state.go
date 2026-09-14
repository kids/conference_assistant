// Package state AI 展示状态机 + 全局急停开关。
// 状态流转（本版无 TTS，AI 输出仅投屏展示）：
//
//	IDLE → GENERATING → PENDING_APPROVAL → SHOWING → IDLE
package state

import (
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
func (d *Display) Kill() { d.kill.Store(true) }

// Killed 是否已急停。
func (d *Display) Killed() bool { return d.kill.Load() }
