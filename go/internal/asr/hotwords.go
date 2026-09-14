// Package asr 热词解析与流式 ASR 协议客户端。
package asr

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// LoadHotwords 解析热词词典（词<TAB>权重），返回 {词: 权重}。
// 对应 Python 版 app/asr/hotwords.py::load_hotwords。
func LoadHotwords(path string) map[string]int {
	words := make(map[string]int)
	f, err := os.Open(path)
	if err != nil {
		return words
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		w := strings.TrimSpace(parts[0])
		if w == "" {
			continue
		}
		weight := 50
		if len(parts) >= 2 {
			// Python 版先 int(float(x))，即容忍 "60.0" 这类写法
			if f, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
				weight = int(f)
			}
		}
		words[w] = weight
	}
	return words
}

// ToStreamJSON 转换为 FunASR 流式接口的 hotwords 字符串（词表为空返回空串）。
func ToStreamJSON(words map[string]int) string {
	if len(words) == 0 {
		return ""
	}
	b, err := json.Marshal(words)
	if err != nil {
		return ""
	}
	return string(b)
}

// SaveHotwords 写热词词典文件（词<TAB>权重，按权重降序）。
func SaveHotwords(path string, words map[string]int) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	w.WriteString("# AI 自动生成的热词词典\n")
	w.WriteString("# 格式: 热词<TAB>权重(1-100)\n")
	for _, kv := range sortedByWeightDesc(words) {
		w.WriteString(kv.Key)
		w.WriteByte('\t')
		w.WriteString(strconv.Itoa(kv.Val))
		w.WriteByte('\n')
	}
	return w.Flush()
}

type kv struct {
	Key string
	Val int
}

// sortedByWeightDesc 按权重降序（同权重按词序，保证输出稳定可复现）。
func sortedByWeightDesc(m map[string]int) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Val != out[j].Val {
			return out[i].Val > out[j].Val
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ParseHotwordsText 解析控制台热词面板文本（每行一个词，可附权重）：
// 支持 "词<TAB>60" 与 "词,60"；已存在的词保留原权重。
func ParseHotwordsText(text string, old map[string]int) map[string]int {
	out := make(map[string]int)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var parts []string
		for _, p := range strings.Split(strings.ReplaceAll(line, ",", "\t"), "\t") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			continue
		}
		word := parts[0]
		weight := old[word]
		if weight == 0 {
			weight = 60
		}
		if len(parts) >= 2 {
			if f, err := strconv.ParseFloat(parts[1], 64); err == nil {
				weight = int(f)
				if weight < 1 {
					weight = 1
				}
				if weight > 100 {
					weight = 100
				}
			}
		}
		out[word] = weight
	}
	return out
}
