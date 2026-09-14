package asr

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// newUUID 生成 UUID v4 字符串（不引入外部依赖）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下退化为固定模式，保证协议字段非空
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

// HotwordList 取热词列表（按权重降序），供需要词序的 ASR 协议使用。
func HotwordList(words map[string]int) []string { return keysOf(words) }

// keysOf 取词表键（顺序按权重降序，保证 ASR 热词串稳定可复现）。
func keysOf(words map[string]int) []string {
	out := make([]string, 0, len(words))
	for _, kv := range sortedByWeightDesc(words) {
		out = append(out, kv.Key)
	}
	return out
}

// toFloat 把 JSON 数值（float64 / int）统一转 float64。
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}
