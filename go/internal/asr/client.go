package asr

import "strings"

// StatusInfo 归一化状态，供 /api/health 与前端展示。
type StatusInfo struct {
	State   string
	Detail  string
	IdleSec float64
}

// Handlers 客户端回调。
//
// 与 Python 版的差异（有意为之）：句键（r{轮次}i{序号} / h{轮次}）通过参数直接传入回调，
// 而 Python 版是回调内部去读 asr_client.last_sentence_key 属性——那是"先写属性再回调"
// 的隐式约定，在多线程下不可靠。这里改为显式传参。
type Handlers struct {
	OnOnline  func(text string)          // 实时草稿
	OnOffline func(key, text string)     // 确认终稿
	OnRevise  func(key, text string)     // 服务端回退修正
	OnStatus  func(status string)        // 原始状态串（前端状态条）
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
