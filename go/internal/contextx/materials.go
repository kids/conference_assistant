// Package contextx 上下文组装：会前资料 / 最近转写 / 焦点 / 历史。
// 对应 Python 版 app/context/（包名用 contextx 以避免与标准库 context 冲突）。
package contextx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultMaterialsMaxChars = 4000

// LoadMaterials 读取 materials 目录下所有可解析文件，拼接为纯文本（STATIC 上下文）。
func LoadMaterials(materialsDir string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = defaultMaterialsMaxChars
	}
	entries, err := os.ReadDir(materialsDir)
	if err != nil {
		return ""
	}
	// Python 版用 sorted(iterdir())：按文件名字典序
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var chunks []string
	total := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		text := loadFile(filepath.Join(materialsDir, e.Name()))
		if text != "" {
			stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			chunk := "[" + stem + "]\n" + strings.TrimSpace(text)
			chunks = append(chunks, chunk)
			total += len([]rune(chunk))
		}
		if total >= maxChars {
			break
		}
	}
	joined := strings.Join(chunks, "\n\n")
	r := []rune(joined)
	if len(r) > maxChars {
		return string(r[:maxChars])
	}
	return joined
}

// loadFile 按后缀解析单个资料文件；PDF/PPTX 等暂不支持（返回空串）。
func loadFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".text":
		return string(raw)
	case ".json":
		var data any
		if err := json.Unmarshal(raw, &data); err != nil {
			return string(raw)
		}
		b, err := json.MarshalIndent(data, "", " ")
		if err != nil {
			return string(raw)
		}
		return string(b)
	default:
		return ""
	}
}
