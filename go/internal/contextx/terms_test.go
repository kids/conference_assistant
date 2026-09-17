package contextx

import (
	"path/filepath"
	"sync"
	"testing"
)

// resetJiebaForTest 复位 jieba 单例并返回还原函数。
//
// jieba 是包级单例、只初始化一次，若同进程内已有更早的测试触发过加载，
// 「词典缺失」场景就测不到了 —— 结果取决于执行顺序。
// 只保存 jiebaInst 指针、不复用 sync.Once 值：Once 含 noCopy，拷贝会触发 vet 警告。
func resetJiebaForTest() func() {
	prevInst := jiebaInst
	jiebaOnce = sync.Once{}
	jiebaInst = nil
	return func() {
		jiebaOnce = sync.Once{}
		jiebaOnce.Do(func() {}) // 标记为已触发，否则下次 jieba() 会重复加载并泄漏 prevInst
		jiebaInst = prevInst
	}
}

// TestScoreTextDegradesWithoutDict 守住一个真实踩过的缺陷：
// gojieba 在词典缺失时会 panic，而 Go 里未捕获的 panic 会终止整个进程
// （Python 版 jieba 不可用时只是降级为标点切分）。这里断言词典不可用时
// 不 panic 且术语表命中路径仍然可用。
func TestScoreTextDegradesWithoutDict(t *testing.T) {
	t.Setenv("JIEBA_DICT_DIR", filepath.Join(t.TempDir(), "不存在的词典目录"))
	t.Cleanup(resetJiebaForTest())

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
