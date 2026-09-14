package contextx

import (
	"path/filepath"
	"testing"
)

// TestScoreTextDegradesWithoutDict 守住一个真实踩过的缺陷：
// gojieba 在词典缺失时会 panic，而 Go 里未捕获的 panic 会终止整个进程
// （Python 版 jieba 不可用时只是降级为标点切分）。这里断言词典不可用时
// 不 panic 且术语表命中路径仍然可用。
func TestScoreTextDegradesWithoutDict(t *testing.T) {
	t.Setenv("JIEBA_DICT_DIR", filepath.Join(t.TempDir(), "不存在的词典目录"))

	glossary := map[string]int{"量子纠缠": 70}
	got := ScoreText("我们用量子纠缠的方法观测了二维材料的超导相变机制", glossary)

	if len(got) != 1 {
		t.Fatalf("降级路径应只产出术语表命中项，实际 %d 项: %+v", len(got), got)
	}
	if got[0].Term != "量子纠缠" {
		t.Fatalf("期望命中术语表词「量子纠缠」，实际 %q", got[0].Term)
	}
	if got[0].Score != 1.0 {
		t.Fatalf("命中术语表应归一化为 1.0，实际 %v", got[0].Score)
	}
}

func TestScoreTextEmpty(t *testing.T) {
	if got := ScoreText("   ", nil); len(got) != 0 {
		t.Fatalf("空文本应返回空结果，实际 %+v", got)
	}
}
