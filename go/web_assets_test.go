package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// 前端资源里 JS 引用的 DOM id 必须真实存在。
//
// 这条断言来自一次真实事故：console.html 的 JS 引用了 $("targetDiscipline") 共 6 次，
// 但页面里从来没有这个元素。浏览器执行到 `$("targetDiscipline").onchange = ...` 抛
// TypeError，<script> 就此中断，排在它后面的 connect() 没有执行 —— 于是「开始 Session」
// 永久卡在「启动中…」，转写区也是空的，而且页面上没有任何报错。
// Go 版和 Python 版各中过一次，故用测试钉住。
//
// 为什么必须自动化：元素定义与引用分散在几百行 HTML/JS 里，人眼 review 极易漏。
var (
	reIDRef  = regexp.MustCompile(`\$\("([^"]+)"\)|getElementById\("([^"]+)"\)`)
	reIDDef  = regexp.MustCompile(`id="([^"]+)"`)
	reScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
)

// 由 JS 动态创建、因此不出现在 HTML 里的 id
var jsCreatedIDs = map[string]bool{
	"seg-draft": true, // 草稿行，由 draftSeg() 创建
}

func TestWebAssetsDOMRefsResolve(t *testing.T) {
	// 第一项必须存在（go:embed 资源）；其余为可选目录，仓库里没有就跳过
	dirs := []struct {
		path     string
		required bool
	}{
		{"web", true},         // Go 版前端（//go:embed web）
		{"../app/web", false}, // 早期 Python 版前端（已移除；将来若恢复则一并守住）
	}

	checked := 0
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d.path, "*.html"))
		if len(files) == 0 {
			if d.required {
				t.Fatalf("%s 下没有 html（embed 资源丢了？）", d.path)
			}
			continue
		}
		for _, f := range files {
			checked++
			t.Run(f, func(t *testing.T) { checkDOMRefs(t, f) })
		}
	}
	if checked == 0 {
		t.Fatal("没有校验到任何 html 文件")
	}
}

func checkDOMRefs(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	src := string(raw)

	defined := make(map[string]bool)
	for _, m := range reIDDef.FindAllStringSubmatch(src, -1) {
		defined[m[1]] = true
	}

	seen := make(map[string]bool)
	var missing []string
	// 只扫 <script> 内的引用：HTML 属性里的 id 不会去「引用」别的 id
	for _, block := range reScript.FindAllStringSubmatch(src, -1) {
		for _, m := range reIDRef.FindAllStringSubmatch(block[1], -1) {
			id := m[1]
			if id == "" {
				id = m[2]
			}
			if defined[id] || jsCreatedIDs[id] || seen[id] {
				continue
			}
			seen[id] = true
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Errorf("%s：JS 引用了不存在的 DOM id %v\n"+
		"  后果：浏览器在绑定处抛 TypeError，整个 <script> 中断，后面的初始化（含 connect()）都不执行。\n"+
		"  修法：① 在 HTML 里补上该元素（首选）；② 把引用包进 if(el){...} 保护；\n"+
		"        ③ 若该 id 确由 JS 动态创建，加进 jsCreatedIDs 白名单。", path, missing)
}
