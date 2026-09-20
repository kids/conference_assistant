package server

import "testing"

func TestIsFillerOnly(t *testing.T) {
	// 丢弃：整句只有语气词与标点
	drop := []string{
		"嗯", "嗯。", "嗯嗯。", "呃，", "哦！", "啊？", "嗯呃。", "唔……",
		"嗯嗯嗯嗯", "哈 哈", "唉～", "嗯!",
	}
	for _, s := range drop {
		if !isFillerOnly(s) {
			t.Errorf("%q 应判为纯语气词（丢弃）", s)
		}
	}
	// 保留：含实义字，或语气串超过 4 个字
	keep := []string{
		"嗯，但是这样不行。", "那么。", "好的。", "嗯嗯嗯嗯嗯",
		"呃呃，我想说", "啊哈，原来如此", "哈工大。", "哦，这样。",
	}
	for _, s := range keep {
		if isFillerOnly(s) {
			t.Errorf("%q 含实义字，不应判为纯语气词（保留）", s)
		}
	}
}
