package agent

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// SystemPrompt 系统提示词（与 Python 版逐字一致）。
const SystemPrompt = `你是"AI 跨学科翻译席"，服务于高水平跨学科学术会议（Workshop）。
你的职责是：把某一学科的专业表达，实时转换为其他学科也能理解、能提问、能连接的科学问题。

铁律（违反即失败）：
- 只做翻译/解释/转译/桥接，绝不评价研究质量、不比较学术贡献、不给出未经确认的结论。
- 不编造数据、不夸大意义；不确定的地方明确说"这一点需要报告人确认"。
- 每次只做主持人指定的一件事，输出纯口播文本：无 Markdown、无列表、无编号、无括注。
- 输出必须自然、通俗、口语化，适合主持人现场朗读（约 20-30 秒）。
- 每次输出必须以请求报告人/提问者确认的句子结尾。`

// TaskSpeech 「发言」任务键：按立场 + 会议上下文起草发言稿。
// 它与 5 个翻译任务走同一条生成/落库/投屏链路，但提示词、校验与展示位置不同
// （前端的展示区在控制台右下角，见 web/console.html）。
const TaskSpeech = "SPEECH"

// Task 一个翻译任务的定义。
type Task struct {
	Key         string
	Label       string
	Instruction string
	MinChars    int
	MaxChars    int
	TargetHint  string // 需要目标词/句时的提示
}

// Tasks 5 个任务的注册表。
var Tasks = map[string]Task{
	"TERM": {
		Key:   "TERM",
		Label: "术语翻译",
		Instruction: "请用约 20 秒解释主持人指定的这个术语，只做解释，不做评价。" +
			"模板：这个术语可以理解为……；它在本报告中重要，是因为……。" +
			"结尾：这个解释请报告人确认。",
		MinChars:   60,
		MaxChars:   120,
		TargetHint: "需要解释的目标术语",
	},
	"Q_TRANSLATE": {
		Key:   "Q_TRANSLATE",
		Label: "问题转译",
		Instruction: "请把刚才听众提出的这个专业问题，转译成其他学科也能理解的科学问题。" +
			"模板：刚才的问题更一般地说是在问……；它可能与……领域也有关。" +
			"结尾：这个转译请提问者和报告人确认。",
		MinChars:   60,
		MaxChars:   120,
		TargetHint: "需要转译的问题",
	},
	"A_TRANSLATE": {
		Key:   "A_TRANSLATE",
		Label: "回答转译",
		Instruction: "请把报告人刚才的回答，转述成非本领域听众容易理解的通俗版本。" +
			"保留原意与关键术语，去除过于专业的表达。" +
			"结尾：这个转述请报告人确认。",
		MinChars:   60,
		MaxChars:   120,
		TargetHint: "需要转译的回答",
	},
	"BRIDGE": {
		Key:   "BRIDGE",
		Label: "桥接提问",
		Instruction: "请提出一个跨学科桥接问题，把当前讨论引向与其他领域的连接，不评价研究质量。" +
			"模板：一个可以追问的问题是……；这个问题可能连接……和……。" +
			"结尾：这个连接是否成立，请报告人判断。",
		MinChars: 50,
		MaxChars: 110,
	},
	"SUMMARY": {
		Key:   "SUMMARY",
		Label: "报告总结",
		Instruction: "请用约 30 秒对刚才的报告做一个通俗易懂的总结，突出关键问题、核心概念与学术意义。" +
			"结尾：以上总结请报告人确认。",
		MinChars: 90,
		MaxChars: 150,
	},
	// 「发言」不在此表驱动（长度/语言由控制台参数决定，见 SpeechLengths），
	// 保留注册项是为了让同样的 task 键在别处（标签、导出、日志）有统一出处。
	TaskSpeech: {
		Key:         TaskSpeech,
		Label:       "发言",
		Instruction: "请按发言者给出的立场，结合会议上下文起草一段发言稿。",
		MinChars:    180,
		MaxChars:    330,
	},
}

// SpeechOptions 「发言」的生成参数（控制台中间栏填写）。
type SpeechOptions struct {
	Stance   string // 立场：发言者的观点基础（必填）
	Language string // 语言：中文 / English / 中英双语（默认中文）
	Length   string // 长度档位：short / medium / long（默认 medium）
	Extra    string // 补充要求（可选）
}

// SpeechLength 长度档位 → 口播时长与字数窗口。
type SpeechLength struct {
	Key      string
	Label    string
	Seconds  int
	MinChars int
	MaxChars int
}

// SpeechLengths 三档长度。字数按中文口播约 4.5 字/秒估（与 EstimateSecs 同一口径）。
var SpeechLengths = []SpeechLength{
	{Key: "short", Label: "约 30 秒", Seconds: 30, MinChars: 90, MaxChars: 165},
	{Key: "medium", Label: "约 1 分钟", Seconds: 60, MinChars: 180, MaxChars: 330},
	{Key: "long", Label: "约 2 分钟", Seconds: 120, MinChars: 360, MaxChars: 660},
}

// SpeechLanguages 语言档位。
var SpeechLanguages = []string{"中文", "English", "中英双语"}

// Normalize 补默认值并归一化非法档位。
func (o SpeechOptions) Normalize() SpeechOptions {
	o.Stance = trimSpace(o.Stance)
	o.Extra = trimSpace(o.Extra)
	o.Language = trimSpace(o.Language)
	if o.Language == "" {
		o.Language = SpeechLanguages[0]
	}
	switch o.Length {
	case "short", "medium", "long":
	default:
		o.Length = "medium"
	}
	return o
}

// SpeechLengthOf 取长度档位（非法值按 medium）。
func SpeechLengthOf(key string) SpeechLength {
	for _, l := range SpeechLengths {
		if l.Key == key {
			return l
		}
	}
	return SpeechLengths[1]
}

// SpeechWindow 依据长度档位与语言给出字数窗口（校验用）与目标秒数。
// 非中文时按语言放宽：英文按 rune 计数天然更长（60 秒约 700~800 字符），
// 若沿用中文字数窗口会把正常英文发言判成「过短」。
func SpeechWindow(length, language string) (minChars, maxChars, seconds int) {
	l := SpeechLengthOf(length)
	minChars, maxChars, seconds = l.MinChars, l.MaxChars, l.Seconds
	switch language {
	case "English":
		minChars, maxChars = l.MinChars*2, int(float64(l.MaxChars)*2.4)
	case "中英双语":
		minChars, maxChars = int(float64(l.MinChars)*1.6), int(float64(l.MaxChars)*1.9)
	}
	return
}

// SpeechCPS 口播速度（rune/秒），用于时长校验。
func SpeechCPS(language string) float64 {
	switch language {
	case "English":
		return 13.0
	case "中英双语":
		return 8.0
	default:
		return 4.5
	}
}

// SpeechInstruction 组装「发言」任务指令（补充要求单独成块，见 buildSpeechMessages）。
func SpeechInstruction(language string, seconds, minChars, maxChars int) string {
	return "请以发言者本人的身份，起草一段会议现场发言稿。\n\n" +
		"写作要求：\n" +
		"- 立场是这段发言的观点基础：通篇服务于该立场，可以表达观点、追问、建议或提出合作意向；\n" +
		"- 必须扣住上面的会议上下文（报告内容、问答、出现的术语与人名），让发言接得上现场；\n" +
		"- 不编造会议中没有出现的数据、结论、人名机构；不确定的用「如果我没理解错」这类措辞带过；\n" +
		"- 语言：" + language + "；篇幅：约 " + strconv.Itoa(seconds) + " 秒口播（" +
		strconv.Itoa(minChars) + "~" + strconv.Itoa(maxChars) + " 字）；\n" +
		"- 若上面给了补充要求，优先满足它；\n" +
		"- 直接输出发言稿正文：不要标题、不要 Markdown 标记、不要括号里的舞台提示；\n" +
		"- 口语化、可朗读，句子之间自然衔接。"
}

// SpeechSystemPrompt 「发言」任务的系统提示词。
// 与翻译席的铁律不同：这里产出的不是「跨学科翻译尝试」，而是发言者本人的发言稿，
// 因此允许表达立场与观点、允许分段；但同样不允许编造事实、不替听众评判研究质量。
const SpeechSystemPrompt = `你是"AI 发言助理"，为学术会议现场的与会者起草发言稿。
输入是：会议材料摘要、最近转写、发言者立场，以及语言与篇幅要求。
输出是：一段可以直接站起来念的发言稿。

铁律（违反即失败）：
- 只依据给定的会议上下文来写，绝不编造会议中未出现的数据、结论、人名或机构；
- 立场由发言者给出：你要把该立场与现场讨论接起来，而不是替听众评判研究质量；
- 不写标题、不写 Markdown 标记、不写括号舞台提示，直接输出可朗读的正文；
- 严格按要求的语言与篇幅，宁可少说，不为凑字数注水。`

func trimSpace(s string) string { return strings.TrimSpace(s) }

// EstimateSecs 中文口播时长估算：约 4.5 字/秒。
func EstimateSecs(text string) float64 {
	v := float64(utf8.RuneCountInString(text)) / 4.5
	if v < 1.0 {
		return 1.0
	}
	return v
}

// MockOutput LLM 未配置时的占位输出，便于本地联调前端链路。
func MockOutput(taskKey, target string) string {
	switch taskKey {
	case "TERM":
		if target == "" {
			target = "该概念"
		}
		return "这个术语“" + target + "”可以理解为：用其他学科更容易把握的方式来看待它，抓住它的核心作用即可。它在本报告中重要，是因为它贯穿了作者要解决的关键问题。这个解释请报告人确认。"
	case "Q_TRANSLATE":
		return "刚才的问题更一般地说是在问：这个机制背后的普遍规律是什么，它可能与更广泛的研究领域也有关。这个转译请提问者和报告人确认。"
	case "A_TRANSLATE":
		return "报告人刚才的回答，通俗地说，是在解释为什么会得到这样的结果，以及这个结果意味着什么。这个转述请报告人确认。"
	case "BRIDGE":
		return "一个可以追问的问题是：这个方法背后的思路，能否用于解决另一个领域的相似困难？这个问题可能连接当前领域和更广泛的交叉领域。这个连接是否成立，请报告人判断。"
	case TaskSpeech:
		return "（LLM 未配置，这是占位发言稿）我关注的是刚才报告中提到的方向，和我自己的工作有不少可以对接的地方。我的立场是：这条路线要真正落地，还需要在工程可验证性上多下功夫。如果我没理解错，报告里的关键指标是在理想条件下得到的，那么下一步能不能一起看看更接近真实场景的验证方案？这也是我今天特别想和大家讨论的一点。"
	default:
		return "刚才的报告围绕一个核心问题展开，报告人介绍了背景、方法和主要发现，并说明了其学术意义。以上总结请报告人确认。"
	}
}
