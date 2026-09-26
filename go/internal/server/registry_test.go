package server

import (
	"testing"

	"seat/internal/config"
)

// testSettings 构造最小可用的配置（只填构造会场运行时必需的字段）。
func testSettings(dir string) *config.Settings {
	return &config.Settings{
		BaseDir:           dir,
		SampleRate:        16000,
		FrameMS:           20,
		VADSilenceMS:      600,
		VADAggressiveness: 2,
		MaxSegmentS:       15,
		DiarizeMaxSegS:    6,
		RecKeepDays:       7,
		RecChunkSec:       300,
		RefineEnabled:     false,
		RefineMinChars:    12,
	}
}

// TestRegistryLRURecycle 会场上限触顶时，应回收「没有活跃流水线且最久未访问」的
// 会场而不是直接报错；被回收会场的 DB 记录保留（之后按 sid 访问可恢复）。
func TestRegistryLRURecycle(t *testing.T) {
	reg, err := NewRegistry(testSettings(t.TempDir()))
	if err != nil {
		t.Fatalf("创建注册表失败: %v", err)
	}
	defer reg.Close()
	reg.max = 2

	a, err := reg.Create("A", "", "", "", true)
	if err != nil {
		t.Fatalf("创建 A 失败: %v", err)
	}
	b, err := reg.Create("B", "", "", "", true)
	if err != nil {
		t.Fatalf("创建 B 失败: %v", err)
	}

	// A 刚刚被访问过；B 从未被访问 → 应优先回收 B
	a.Touch()
	c, err := reg.Create("C", "", "", "", true)
	if err != nil {
		t.Fatalf("达上限后应回收再创建，实际报错: %v", err)
	}
	if got := len(reg.List()); got != 2 {
		t.Fatalf("回收后应仍有 2 个会场，实际 %d", got)
	}
	if reg.Get(b.SessionID()) != nil {
		t.Error("最久未访问的 B 应被回收")
	}
	if reg.Get(a.SessionID()) == nil || reg.Get(c.SessionID()) == nil {
		t.Error("A 与新建的 C 都应保留")
	}
	// 被回收会场的 DB 记录保留：Ensure 能按 sid 恢复
	rt, err := reg.Ensure(b.SessionID())
	if err != nil || rt == nil {
		t.Fatalf("被回收会场应可按 sid 恢复，实际 rt=%v err=%v", rt, err)
	}
}
