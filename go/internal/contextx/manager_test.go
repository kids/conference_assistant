package contextx

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"seat/internal/store"
)

// TestClipRunesTailKeepsLatest 保尾语义：超预算时保留末尾（最新），丢弃开头。
func TestClipRunesTailKeepsLatest(t *testing.T) {
	s := strings.Repeat("旧", 100) + "最新内容"
	got := clipRunesTail(s, 4)
	if got != "最新内容" {
		t.Errorf("应保留末尾 4 字「最新内容」，实际 %q", got)
	}
	if got := clipRunesTail("短", 10); got != "短" {
		t.Errorf("未超预算应原样返回，实际 %q", got)
	}
}

// TestBuildRecentKeepsLatest 转写总量超出预算时，RECENT 必须保留最新的句子。
// 回归背景：此前用 clipRunes 保头截断，长会议里近期内容会被整段丢掉，
// 「报告总结」读到的其实是会议开头的内容 —— 总结与现场事实不符的根因。
func TestBuildRecentKeepsLatest(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.sqlite"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()
	sid, err := st.CreateSession("t", "", "", true, "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	// 5 条各约 1 万字：合计约 5 万 > 4 万预算，必然触发截断
	for i := 0; i < 5; i++ {
		tag := fmt.Sprintf("第%d段", i+1)
		text := tag + strings.Repeat("填", 9997) + "结尾" + tag
		if _, err := st.AddSegment(sid, i+1, float64(i), float64(i)+1, text); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}
	m := NewManager(st, "")
	ctx := m.Build(sid, nil)

	if n := len([]rune(ctx.Recent)); n > recentContextMaxChars {
		t.Errorf("RECENT 超预算：%d > %d", n, recentContextMaxChars)
	}
	if !strings.Contains(ctx.Recent, "结尾第5段") {
		t.Error("RECENT 应包含最新一条（第 5 段）的结尾")
	}
	if strings.Contains(ctx.Recent, "第1段") {
		t.Error("RECENT 不应包含最早的（第 1 段）")
	}
}
