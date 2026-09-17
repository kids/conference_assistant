package agent

import (
	"strings"
	"testing"

	"seat/internal/contextx"
)

// 「发言」的字数窗口既进提示词、又做后置校验：两处口径不一致的表现是
// 「模型按 2 分钟写、系统按 30 秒判」这类难查的偏差，故把窗口与校验钉在一起。

func TestSpeechWindowByLength(t *testing.T) {
	for _, l := range SpeechLengths {
		min, max, secs := SpeechWindow(l.Key, "中文")
		if min != l.MinChars || max != l.MaxChars || secs != l.Seconds {
			t.Errorf("中文档位 %s 应与预设一致，实际 %d~%d/%ds", l.Key, min, max, secs)
		}
	}
	// 非法档位回落到 medium，而不是 0 字窗口
	min, max, secs := SpeechWindow("nonsense", "中文")
	med := SpeechLengthOf("nonsense")
	if min != med.MinChars || max != med.MaxChars || secs != med.Seconds {
		t.Errorf("非法档位应回落 medium，实际 %d~%d/%ds", min, max, secs)
	}
	// 英文按 rune 计数天然更长，窗口必须放宽，否则正常英文发言被判「过短」
	enMin, enMax, _ := SpeechWindow("medium", "English")
	if enMin <= med.MinChars || enMax <= med.MaxChars {
		t.Errorf("英文窗口应宽于中文：%d~%d vs %d~%d", enMin, enMax, med.MinChars, med.MaxChars)
	}
}

func TestValidateSpeechAllowsParagraphsAndStance(t *testing.T) {
	// 一段 200 字、带换行与观点的发言稿：短翻译任务的三道铁律都会拒绝它，
	// 发言稿必须放行（它本来就要被念出来，且允许表达立场）
	draft := strings.Repeat("我认为这条路线值得投入，因为现场的验证方案还有改进空间。", 9) +
		"\n" + strings.Repeat("希望会后能和报告人继续讨论界面稳定性的表征方法。", 2)
	if n := len([]rune(draft)); n < 180 || n > 330 {
		t.Fatalf("测试用稿长度应在 medium 窗口内，实际 %d", n)
	}
	r := ValidateSpeech(draft, 180, 330, 60*1.35, 4.5)
	if !r.OK {
		t.Errorf("发言稿应通过校验，实际 %+v", r.Checks)
	}
	// 同一段文本走短任务校验会被拒（对照，说明两套规则确实不同）
	if short := Validate(draft, 180, 330); short.OK {
		t.Error("短任务校验应拒绝含换行的发言稿（对照用）")
	}
}

func TestValidateSpeechRejectsBadLength(t *testing.T) {
	if r := ValidateSpeech("太短了。", 180, 330, 90, 4.5); r.OK {
		t.Error("过短应被拒绝")
	}
	long := strings.Repeat("这是一句用来测试超长截断的话。", 60)
	r := ValidateSpeech(long, 180, 330, 90, 4.5)
	if !r.OK {
		// 超长会先按句截断到窗口内；截断后仍超才报错
		if !strings.Contains(r.Checks["len"], "过长") {
			t.Errorf("超长稿应报过长或截断后通过，实际 %+v", r.Checks)
		}
	}
	if n := len([]rune(r.Text)); n > 330 {
		t.Errorf("截断后不应超过上限，实际 %d 字", n)
	}
}

func TestSpeechOptionsNormalize(t *testing.T) {
	o := SpeechOptions{Stance: "  我有不同看法  ", Length: "weird", Language: ""}.Normalize()
	if o.Stance != "我有不同看法" {
		t.Errorf("立场应去空白，实际 %q", o.Stance)
	}
	if o.Length != "medium" {
		t.Errorf("长度应回落 medium，实际 %q", o.Length)
	}
	if o.Language != "中文" {
		t.Errorf("语言应回落中文，实际 %q", o.Language)
	}
}

func TestSpeechInstructionCarriesRequirements(t *testing.T) {
	s := SpeechInstruction("English", 60, 360, 792)
	for _, want := range []string{"English", "60", "360", "792"} {
		if !strings.Contains(s, want) {
			t.Errorf("指令应包含 %q：%s", want, s)
		}
	}
}

func TestBuildSpeechMessagesUsesSpeechPrompt(t *testing.T) {
	msgs := buildSpeechMessages(SpeechOptions{Stance: "支持合作", Language: "中文", Extra: "面向政府听众"},
		contextx.Context{Recent: "报告人介绍了固态电解质"}, "白话", 180, 330, 60)
	if len(msgs) != 2 || msgs[0].Content != SpeechSystemPrompt {
		t.Fatalf("应使用发言系统提示词，实际 %+v", msgs)
	}
	user := msgs[1].Content
	for _, want := range []string{"支持合作", "面向政府听众", "报告人介绍了固态电解质", "白话"} {
		if !strings.Contains(user, want) {
			t.Errorf("用户消息应包含 %q", want)
		}
	}
	if strings.Contains(user, "【本场已展示过的 AI 输出") {
		t.Error("没有历史输出时不应出现空的历史块")
	}
}
