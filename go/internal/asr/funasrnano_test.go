package asr

import "testing"

// TestNanoLanguageAutoOmitsCommand：auto/空 不得下发 LANGUAGE 命令。
// funasr_nano 的语种在每轮会话 START 时下发，一旦下发就被服务端强制定死 ——
// 英文报告会被硬解码成中文（空耳/翻译），因此 auto 必须表现为「不发该命令」。
func TestNanoLanguageAutoOmitsCommand(t *testing.T) {
	for _, in := range []string{"", "auto", "自动", "自动检测"} {
		if got := nanoLanguage(NormalizeLanguage(in)); got != "" {
			t.Errorf("nanoLanguage(%q) = %q，期望空串（不发 LANGUAGE 命令）", in, got)
		}
	}
	// 显式语种仍按原样下发（funasr_nano 认中文词，不是 ISO 码）
	if got := nanoLanguage(NormalizeLanguage("英文")); got != "英文" {
		t.Errorf("nanoLanguage(英文) = %q，期望「英文」", got)
	}
	if got := nanoLanguage(NormalizeLanguage("zh")); got != "中文" {
		t.Errorf("nanoLanguage(zh) = %q，期望「中文」", got)
	}
}

// TestNanoSetLanguage：未连接时切语种只改字段、不应崩（disconnect 也须能空转）。
func TestNanoSetLanguage(t *testing.T) {
	c := NewFunAsrNanoStreamClient("ws://127.0.0.1:1", "自动", nil, Handlers{})
	t.Cleanup(c.Close)

	c.SetLanguage("英文")
	if c.language != "英文" {
		t.Fatalf("切到英文后 language 字段应为「英文」，实际 %q", c.language)
	}
	c.SetLanguage("自动")
	if c.language != "" {
		t.Errorf("切回自动后应为空串（不发 LANGUAGE），实际 %q", c.language)
	}
}
