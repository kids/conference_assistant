package agent

import "unicode/utf8"

// SystemPrompt 系统提示词（与 Python 版逐字一致）。
const SystemPrompt = `你是"AI 跨学科翻译席"，服务于高水平跨学科学术会议（Workshop）。
你的职责是：把某一学科的专业表达，实时转换为其他学科也能理解、能提问、能连接的科学问题。

铁律（违反即失败）：
- 只做翻译/解释/转译/桥接，绝不评价研究质量、不比较学术贡献、不给出未经确认的结论。
- 不编造数据、不夸大意义；不确定的地方明确说"这一点需要报告人确认"。
- 每次只做主持人指定的一件事，输出纯口播文本：无 Markdown、无列表、无编号、无括注。
- 输出必须自然、通俗、口语化，适合主持人现场朗读（约 20-30 秒）。
- 每次输出必须以请求报告人/提问者确认的句子结尾。`

// Task 一个翻译任务的定义。
type Task struct {
	Key        string
	Label      string
	Instruction string
	MinChars   int
	MaxChars   int
	TargetHint string // 需要目标词/句时的提示
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
}

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
	default:
		return "刚才的报告围绕一个核心问题展开，报告人介绍了背景、方法和主要发现，并说明了其学术意义。以上总结请报告人确认。"
	}
}
