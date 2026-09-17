package contextx

import (
	"fmt"
	"strings"
	"testing"
)

// TestProbeScoreText 打印候选抽取结果，用于人工评估质量（无断言）。
func TestProbeScoreText(t *testing.T) {
	for _, c := range []string{
		"我们用量子纠缠的方法观测了二维材料的超导相变机制",
		"通过调控界面极化可以改变载流子的输运性质",
		"这个模型的拓扑绝缘体表面态具有手性自旋结构",
		"实验测量了薄膜的接触角和表面张力",
		"马约拉纳零能模在超导纳米线中被观测到",
		"我们研究了蛋白质折叠过程中的自由能势能面",
		"这项工作揭示了基因表达的调控机制",
		"下面我介绍一下实验方法和结果分析",
		"我们测量了器件的工作温度和界面作用力",
		"We use plasma-enhanced chemical vapor deposition to grow two-dimensional materials",
		"The superconducting transition temperature increases with pressure",
		"This paper reports a novel approach for measuring thermal conductivity",
		"我们用 DFT 和 XRD 分析了 TiO2 薄膜的 band structure",
	} {
		terms := ScoreText(c, nil)
		fmt.Printf("\n输入：%s\n", c)
		if len(terms) == 0 {
			fmt.Printf("  → （无候选）\n")
			continue
		}
		for _, tm := range terms {
			fmt.Printf("  %-26s %.2f\n", tm.Term, tm.Score)
		}
	}
}

// TestProbeTokens 打印分词与词性，用于定位「中文术语抽不出来」的原因。
func TestProbeTokens(t *testing.T) {
	for _, c := range []string{
		"我们用量子纠缠的方法观测了二维材料的超导相变机制",
		"这个模型的拓扑绝缘体表面态具有手性自旋结构",
		"马约拉纳零能模在超导纳米线中被观测到",
		"通过调控界面极化可以改变载流子的输运性质",
	} {
		fmt.Printf("\n%s\n", c)
		for _, tk := range taggedTokens(c) {
			ok := "  "
			if _, isNoun := termFlags[tk.flag]; isNoun {
				ok = "N "
			}
			fmt.Printf("  %s%-14s %s\n", ok, tk.word, tk.flag)
		}
	}
}

// TestProbeAcademicCN 检查白名单命中情况。
func TestProbeAcademicCN(t *testing.T) {
	for _, w := range []string{
		"量子纠缠", "超导相变", "拓扑绝缘体", "表面态", "自旋", "马约拉纳", "零能模",
		"绝缘体", "相变", "超导", "纠缠", "界面极化", "载流子", "自由能", "势能面",
		"作用力", "工作温度", "关系代数", "过程控制", "结果分析", "薄膜", "接触角",
	} {
		fmt.Printf("  %-14s isAcademicCN=%v\n", w, isAcademicCN(w))
	}
}

// TestProbeEnglish 单独看英文抽取规则。
func TestProbeEnglish(t *testing.T) {
	for _, w := range []string{
		"plasma", "enhanced", "chemical", "vapor", "deposition",
		"use", "grow", "materials", "novel", "approach", "measuring",
		"DFT", "XRD", "SEM", "MRI", "DNA", "AI", "ML", "CNT",
	} {
		hasEN, hasCN := reHasEN.MatchString(w), reHasCN.MatchString(w)
		isAbbr := isAbbreviation(w)
		_, stopped := enStop[strings.ToLower(w)]
		fmt.Printf("  %-12s hasEN=%v abbr=%v stop=%v → 抽=%v\n",
			w, hasEN, isAbbr, stopped, hasEN && !hasCN && isAbbr && !stopped)
	}
}
