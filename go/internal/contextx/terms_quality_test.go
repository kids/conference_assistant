package contextx

import "testing"

func hasTerm(terms []Term, want string) bool {
	for _, tm := range terms {
		if tm.Term == want {
			return true
		}
	}
	return false
}

// TestScoreTextNoEnglishNoise 英文普通单词不得成为候选。
//
// 旧实现用 ^[A-Za-z][A-Za-z0-9\-]+$ 判缩写 —— 它匹配任何 ≥2 个字母的英文单词，
// 于是 "We use plasma-enhanced chemical vapor deposition to grow two-dimensional
// materials" 会抽出 use/grow/materials/plasma/vapor/... 共 9 个噪声。
func TestScoreTextNoEnglishNoise(t *testing.T) {
	const sentence = "We use plasma enhanced chemical vapor deposition to grow materials " +
		"and a novel approach for measuring increases with pressure thermal conductivity " +
		"transition temperature dimensional superconducting"
	terms := ScoreText(sentence, nil)
	for _, w := range []string{
		"use", "grow", "materials", "plasma", "novel", "approach", "measuring",
		"increases", "dimensional", "chemical", "vapor", "deposition", "pressure",
	} {
		if hasTerm(terms, w) {
			t.Errorf("英文普通单词 %q 不应成为候选术语，实际结果 %+v", w, terms)
		}
	}
}

// TestScoreTextRealAbbr 真缩写必须抽得出（形态：全大写 / 含数字 / 驼峰 / 连字符+大写）。
func TestScoreTextRealAbbr(t *testing.T) {
	for _, w := range []string{"DFT", "XRD", "MRI", "DNA", "TiO2", "CO2", "mRNA"} {
		terms := ScoreText("我们用 "+w+" 做了分析", nil)
		if !hasTerm(terms, w) {
			t.Errorf("缩写 %q 应被抽出，实际 %+v", w, terms)
		}
	}
}

// TestScoreTextChineseCompounds 中文复合术语必须抽得出。
//
// 旧实现漏抽的原因：只认名词性词性，而 jieba 把「超导」「纠缠」「相变」「自旋」
// 标成动词 v，这些词虽在白名单里却走不到那一步。
func TestScoreTextChineseCompounds(t *testing.T) {
	cases := map[string][]string{
		"我们用量子纠缠的方法观测了二维材料的超导相变机制": {"量子纠缠", "超导相变"},
		"这项工作揭示了基因表达的调控机制":        {"基因表达", "调控机制"},
		"马约拉纳零能模在超导纳米线中被观测到":      {"超导纳米线"},
		"实验测量了薄膜的接触角和表面张力":        {"表面张力"},
	}
	for text, wants := range cases {
		terms := ScoreText(text, nil)
		for _, w := range wants {
			if !hasTerm(terms, w) {
				t.Errorf("文本 %q 应抽出 %q，实际 %+v", text, w, terms)
			}
		}
	}
}

// TestScoreTextNoVerbNounNoise 泛用动词 + 名词不得拼成假术语。
//
// 放宽 v 词性后实测拼出过「改变载流子」「具有手性」—— 它们通过了白名单后缀检查
// （载流子 / 手性 都在后缀表里），只能靠 mergeHeadBlock 拦住。
func TestScoreTextNoVerbNounNoise(t *testing.T) {
	cases := map[string][]string{
		"通过调控界面极化可以改变载流子的输运性质": {"改变载流子", "可以改变"},
		"这个模型的拓扑绝缘体表面态具有手性自旋结构": {"具有手性"},
		"我们研究了蛋白质折叠过程中的自由能势能面":  {"研究了蛋白质"},
	}
	for text, bads := range cases {
		terms := ScoreText(text, nil)
		for _, b := range bads {
			if hasTerm(terms, b) {
				t.Errorf("文本 %q 不应抽出假术语 %q，实际 %+v", text, b, terms)
			}
		}
	}
}
