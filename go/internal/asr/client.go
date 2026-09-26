package asr

import "strings"

// StatusInfo 归一化状态，供 /api/health 与前端展示。
type StatusInfo struct {
	State   string
	Detail  string
	IdleSec float64
	// InferFail 连续推理失败次数（任一次成功即清零）。
	// 用来区分两种外观完全相同的故障：「确实没有语音」与「有语音但推理一直失败」——
	// 旧版两者都只表现为「无语音」，从页面上无法分辨。
	InferFail int
}

// Handlers 客户端回调。
//
// 与 Python 版的差异（有意为之）：句键（r{轮次}i{序号} / h{轮次}）通过参数直接传入回调，
// 而 Python 版是回调内部去读 asr_client.last_sentence_key 属性——那是"先写属性再回调"
// 的隐式约定，在多线程下不可靠。这里改为显式传参。
type Handlers struct {
	OnOnline  func(text string)      // 实时草稿
	OnOffline func(key, text string) // 确认终稿
	OnRevise  func(key, text string) // 服务端回退修正
	OnStatus  func(status string)    // 原始状态串（前端状态条）
}

// Client 流式 ASR 客户端统一接口。
type Client interface {
	// Connect 预热连接（不阻塞）。流水线启动时调用，避免开头丢帧。
	Connect()
	// SendAudio 送一帧 16k/mono/int16 PCM。
	SendAudio(pcm []byte)
	// MarkEnd 本地 VAD 判定句尾。
	MarkEnd()
	// UpdateHotwords 更新热词（内部重连以携带新词表）。
	UpdateHotwords(words map[string]int)
	// StatusInfo 当前连接状态。
	StatusInfo() StatusInfo
	// Close 释放资源。
	Close()
}

// LanguageAuto 归一化后的「自动检测」值：不向服务端指定语种，由模型自行判定。
//
// 为什么把 auto 做成一等公民：Qwen3-ASR / Fun-ASR 这类 LLM 式 ASR 在被强指定语种时，
// 会把另一种语言的语音硬解码成该语言 —— 英文报告被「空耳」成中文、个别句子甚至被
// 整句翻译成中文（现场「英文报告变中文」即此现象）。Qwen3-ASR 官方模型卡：
// language=None → 自动语种识别（30 语种 + 22 种中文方言，1.7B 语种识别准确率 97.9%）。
// 实测线上服务：不传 language 时中英文均正常，传 language=zh 时英文在干净音频上尚可，
// 但复杂现场（远场、口音、串音）会被中文先验带偏。
const LanguageAuto = "auto"

// LanguageSetter 支持运行期切换语种的可选接口。
// qwen3_http（下一次推理现取语种）与 funasr_nano（下一句重开会话时下发）已实现；
// hy_stream / funasr 协议不带语种参数，未实现 —— 切换将在下次开始 Session 时生效。
type LanguageSetter interface {
	SetLanguage(language string)
}

// languageAliases 语种别名 → 项目内规范值（中文名）。各协议再转成自己要的写法：
// qwen3_http 只认 ISO 码（zh/en/…），funasr_nano 认中文词（中文/英文/…）。
var languageAliases = map[string]string{
	"auto": LanguageAuto, "自动": LanguageAuto, "自动识别": LanguageAuto, "自动检测": LanguageAuto,
	"zh": "中文", "中文": "中文", "汉语": "中文", "普通话": "中文", "chinese": "中文",
	"en": "英文", "英文": "英文", "英语": "英文", "english": "英文",
	"ja": "日语", "日语": "日语", "日文": "日语", "japanese": "日语",
	"ko": "韩语", "韩语": "韩语", "韩文": "韩语", "korean": "韩语",
}

// knownLanguages 规范语种集合（运行期切换的白名单，防止误输入被拼进请求）。
var knownLanguages = map[string]struct{}{"中文": {}, "英文": {}, "日语": {}, "韩语": {}}

// NormalizeLanguage 归一化语种配置：空/auto/自动 → LanguageAuto；已知别名 → 中文/英文/…；
// 未知值原样返回（由各协议自行转换或回退）。
func NormalizeLanguage(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return LanguageAuto
	}
	if canon, ok := languageAliases[strings.ToLower(v)]; ok {
		return canon
	}
	return v
}

// KnownLanguage 是否为已知语种（含 LanguageAuto）。
func KnownLanguage(language string) bool {
	if language == LanguageAuto {
		return true
	}
	_, ok := knownLanguages[language]
	return ok
}

// languageLabel 语种的可读标签（内部空串即「自动」，日志/页面统一显示 auto）。
func languageLabel(language string) string {
	if language == "" {
		return LanguageAuto
	}
	return language
}

// normalizeStatus 把原始状态串映射为稳定状态机（对齐 Python 版 _status）。
// strict 为 true 时（qwen3_http），error 分支保留完整原始串作为 detail。
func normalizeStatus(raw string) (state, detail string) {
	switch {
	case raw == "connected":
		return "connected", ""
	case strings.HasPrefix(raw, "connect_failed:"):
		return "connecting", strings.TrimPrefix(raw, "connect_failed:")
	case strings.HasPrefix(raw, "error:"):
		return "error", strings.TrimPrefix(raw, "error:")
	case raw == "closed":
		return "closed", ""
	default:
		return raw, ""
	}
}
