// Package agent AI 输出后置校验与任务编排。
package agent

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 评价类禁用词（命中即拒绝）
var evalWords = []string{
	"很有价值", "水平很高", "突破性", "颠覆", "领先于", "不如", "更优秀",
	"权威", "顶尖", "一流", "里程碑", "重大贡献", "解决了难题", "远超",
	"最好", "最强", "第一", "优秀", "卓越",
}

// 确认句关键词
var confirmWords = []string{
	"请报告人确认", "请提问者和报告人确认", "请报告人判断", "请专家确认", "请报告人确认。",
}

var reMarkdown = regexp.MustCompile("[#*_>`\\-]{2,}|\n|```")

func checkEval(text string) (bool, string) {
	for _, w := range evalWords {
		if strings.Contains(text, w) {
			return false, "含评价性词汇：" + w
		}
	}
	return true, "ok"
}

func checkConfirm(text string) (bool, string) {
	for _, w := range confirmWords {
		if strings.Contains(text, w) {
			return true, "ok"
		}
	}
	return false, "缺少确认句"
}

func checkMarkdown(text string) (bool, string) {
	if reMarkdown.MatchString(text) {
		return false, "含 Markdown/换行"
	}
	return true, "ok"
}

func checkLength(text string, minChars, maxChars int) (bool, string) {
	n := utf8.RuneCountInString(text)
	if n < minChars {
		return false, fmt.Sprintf("过短(%d<%d)", n, minChars)
	}
	if n > maxChars {
		return false, fmt.Sprintf("过长(%d>%d)", n, maxChars)
	}
	return true, "ok"
}

func checkDuration(text string, maxSecs, cps float64) (bool, string) {
	secs := float64(utf8.RuneCountInString(text)) / cps
	if secs > maxSecs {
		return false, fmt.Sprintf("口播超时(%.0fs>%.0fs)", secs, maxSecs)
	}
	return true, "ok"
}

// Result 校验结果。
type Result struct {
	OK     bool
	Checks map[string]string
	Text   string
}

// Validate 校验 AI 输出：字数窗口 / 评价词 / 确认句 / Markdown / 时长。
// 过长时先截断到最后一个完整句再复检。
func Validate(text string, minChars, maxChars int) Result {
	checks := make(map[string]string)

	ok, msg := checkLength(text, minChars, maxChars)
	checks["len"] = msg
	if !ok && strings.Contains(msg, "过长") {
		text = truncateSentence(text, maxChars)
		ok, msg = checkLength(text, minChars, maxChars)
		checks["len"] = msg
	}

	ok2, m2 := checkEval(text)
	checks["no_eval"] = m2
	ok3, m3 := checkConfirm(text)
	checks["confirm"] = m3
	ok4, m4 := checkMarkdown(text)
	checks["markdown"] = m4
	ok5, m5 := checkDuration(text, 30.0, 4.5)
	checks["duration"] = m5

	return Result{
		OK:     ok && ok2 && ok3 && ok4 && ok5,
		Checks: checks,
		Text:   strings.TrimSpace(text),
	}
}

// ValidateSpeech 校验发言稿：只校验字数窗口与口播时长。
//
// 刻意不套用短翻译任务的四道铁律：
//   - 「禁评价词」——发言稿是发言者本人的立场表达，允许表达观点；
//   - 「必带确认句」——确认句是给"AI 翻译尝试"用的，发言稿本身就要被念出来；
//   - 「禁 Markdown/换行」——发言稿允许多段，分段反而更好念。
//
// 而长度与时长必须守住：它决定现场要占用多少时间，是操作员唯一可控的量化约束。
// 过长时先截到最后一个完整句；截断后仍超则按「过长」报错，由操作员人工处理。
func ValidateSpeech(text string, minChars, maxChars int, maxSecs, cps float64) Result {
	checks := make(map[string]string)

	ok, msg := checkLength(text, minChars, maxChars)
	if !ok && strings.Contains(msg, "过长") {
		text = truncateSentence(text, maxChars)
		ok, msg = checkLength(text, minChars, maxChars)
	}
	checks["len"] = msg

	ok2, m2 := checkDuration(text, maxSecs, cps)
	checks["duration"] = m2

	return Result{
		OK:     ok && ok2,
		Checks: checks,
		Text:   strings.TrimSpace(text),
	}
}

// truncateSentence 截断到最后一个完整句号，仍超则硬截。
func truncateSentence(text string, maxChars int) string {
	r := []rune(text)
	if len(r) <= maxChars {
		return text
	}
	head := r[:maxChars]
	for _, punc := range []rune{'。', '？', '！', '；'} {
		idx := lastIndexRune(head, punc)
		if float64(idx) > float64(maxChars)*0.6 {
			return string(head[:idx+1])
		}
	}
	return string(head)
}

func lastIndexRune(r []rune, target rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == target {
			return i
		}
	}
	return -1
}
