package contextx

import (
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/yanyiwu/gojieba"
)

// 术语候选提取（分词 + 规则打分，不调用 LLM，零延迟）。
// 对应 Python 版 app/context/terms.py，规则、权重、阈值逐条对齐。

// stop 口语/功能词：本身不是术语，也不能作为术语的组成部分。
var stopWords = map[string]struct{}{
	"我们": {}, "这个": {}, "那个": {}, "就是": {}, "然后": {}, "一个": {}, "可以": {}, "因为": {}, "所以": {},
	"如果": {}, "但是": {}, "现在": {}, "大家": {}, "问题": {}, "方法": {}, "研究": {}, "比较": {}, "可能": {},
	"已经": {}, "还是": {}, "这样": {}, "什么": {}, "怎么": {}, "非常": {}, "关于": {}, "通过": {}, "相当": {},
	"其实": {}, "当然": {}, "刚才": {}, "下面": {}, "上面": {}, "这些": {}, "那些": {}, "自己": {}, "东西": {},
	"时候": {}, "地方": {}, "样子": {}, "情况": {}, "内容": {}, "部分": {}, "方面": {}, "结果": {}, "意思": {},
	"老师": {}, "同学": {}, "报告": {}, "谢谢": {}, "大概": {}, "有点": {}, "只是": {}, "而且": {}, "另外": {},
	"首先": {}, "其次": {}, "最后": {}, "比如": {}, "例如": {}, "所谓": {}, "一下": {}, "一些": {}, "考虑": {},
	"觉得": {}, "认为": {}, "看到": {}, "发现": {}, "得到": {}, "出来": {}, "进来": {}, "起来": {}, "下去": {},
	"工作": {}, "情形": {}, "过程": {}, "阶段": {}, "水平": {}, "程度": {}, "关系": {}, "作用": {}, "影响": {},
}

// academicSuffix 学术术语后缀（中文术语的强信号）。
var academicSuffix = []string{
	"效应", "机制", "机理", "模型", "理论", "定律", "定理", "方程", "公式",
	"张力", "势能", "动能", "能量", "熵", "焓", "梯度", "通量", "速率", "常数",
	"系数", "参数", "变量", "函数", "算法", "网络", "架构", "范式",
	"结构", "构型", "构象", "晶格", "晶体", "分子", "原子", "离子", "电子",
	"细胞", "基因", "蛋白", "酶", "受体", "通道", "信号", "通路", "代谢",
	"谱", "成像", "显微", "衍射", "散射", "共振", "激发", "跃迁",
	"催化", "合成", "反应", "极化", "磁化", "掺杂", "缺陷", "界面",
	"薄膜", "衬底", "器件", "芯片", "传感", "微结构", "纳米结构",
	"接触角", "粘度", "黏度", "浸润", "润湿", "铺展", "毛细", "流量", "湍流",
	"层流", "边界层", "雷诺数", "拓扑", "对称性", "手性", "量子",
	"纠缠", "相干", "隧穿", "自旋", "轨道", "带隙", "载流子", "速度", "密度",
}

// academicPrefix 学术术语前缀。
var academicPrefix = []string{
	"微", "纳米", "量子", "超导", "半导体", "生物", "分子", "原子",
	"化学", "物理", "神经", "免疫", "基因", "柱状", "毛细", "表面",
	"非线性", "高维", "多尺度", "自组装", "各向异性", "介观", "宏观", "微观",
}

var (
	reHasCN  = regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)
	reHasEN  = regexp.MustCompile(`[A-Za-z]`)
	rePureCN = regexp.MustCompile(`^[\x{4e00}-\x{9fff}]+$`)
)

// isAbbreviation 判断英文 token 是否像专业缩写。
//
// 旧实现用正则 ^[A-Za-z][A-Za-z0-9\-]+$ —— 它匹配任何 ≥2 个字母的英文单词，
// 于是 "we use plasma to grow materials" 里**每个词**都被当成缩写抽成候选。
// 真正的缩写有明确形态：
//
//	① 含数字（化学式 / 型号）：TiO2 / CO2 / 2D / H2O
//	② 全大写（≥2 字母）：DFT / XRD / MRI / DNA / CNT
//	③ 含非首位的大写（驼峰 / 化学式）：mRNA / iPSC / SiC / GaAs
//	④ 连字符且有大写：X-ray / T-cell
//
// 纯小写单词（plasma / materials / approach）一律不算 —— 它们若是真术语，
// 应由讲稿解析出的术语表（3.0 分）命中，而不是靠形态蒙。
func isAbbreviation(w string) bool {
	letters, uppers, digits := 0, 0, 0
	hasHyphen := false
	for i, r := range w {
		switch {
		case r == '-':
			hasHyphen = true
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
			if unicode.IsUpper(r) {
				uppers++
				// ③ 大写出现在非首位 → 驼峰或化学式
				if i > 0 {
					return true
				}
			}
		}
	}
	if letters < 2 {
		return false
	}
	if digits > 0 { // ①
		return true
	}
	if uppers == letters { // ②
		return true
	}
	if hasHyphen && uppers > 0 { // ④
		return true
	}
	return false
}

// enStop 英文停用词（避免 the/this/and 被当缩写）。
var enStop = map[string]struct{}{
	"the": {}, "this": {}, "that": {}, "and": {}, "for": {}, "with": {}, "you": {}, "are": {}, "was": {}, "our": {},
	"can": {}, "not": {}, "but": {}, "all": {}, "one": {}, "two": {}, "its": {}, "has": {}, "have": {}, "will": {},
	"ok": {}, "okay": {}, "yes": {}, "no": {}, "so": {}, "we": {}, "it": {}, "is": {}, "of": {}, "to": {}, "in": {},
	"ppt": {}, "ai": {},
}

// termFlags 可作为术语（或其组成部分）的词性。
//
// 比「只认名词」宽：jieba 会把核心学术词标成动词 —— 实测「超导」「纠缠」「相变」
// 「自旋」「拓扑」全是 v。旧实现只认名词，这些词虽然在学术后缀白名单里能命中，
// 却因词性先被挡在门外，整条链路失效（「中文专业术语抽不出来」的主因之一）。
// 放宽后仍必须过 isAcademicCN 白名单，精度不降。
var termFlags = map[string]struct{}{
	"n": {}, "nz": {}, "nt": {}, "ns": {}, "nr": {}, "ng": {}, "nrt": {},
	"vn": {}, "v": {}, "vd": {}, "an": {}, "a": {}, "ad": {},
	"eng": {}, "j": {}, "l": {}, "x": {},
}

// ---- jieba 懒加载 ----

var (
	jiebaOnce sync.Once
	jiebaInst *gojieba.Jieba
	jiebaMu   sync.Mutex // gojieba 的并发安全性未明确保证，串行化调用（每句一次，开销可忽略）
)

func jieba() *gojieba.Jieba {
	jiebaOnce.Do(func() {
		// gojieba 默认用 runtime.Caller 从**编译期源码路径**推导词典目录（config.go），
		// 该路径指向构建时的模块缓存，在运行镜像里不存在（gojieba 会直接 panic）。
		// 因此支持用 JIEBA_DICT_DIR 显式指定运行时可用的词典目录。
		if dir := strings.TrimSpace(os.Getenv("JIEBA_DICT_DIR")); dir != "" {
			gojieba.DICT_DIR = dir
			gojieba.DICT_PATH = filepath.Join(dir, "jieba.dict.utf8")
			gojieba.HMM_PATH = filepath.Join(dir, "hmm_model.utf8")
			gojieba.USER_DICT_PATH = filepath.Join(dir, "user.dict.utf8")
			gojieba.IDF_PATH = filepath.Join(dir, "idf.utf8")
			gojieba.STOP_WORDS_PATH = filepath.Join(dir, "stop_words.utf8")
		}
		// NewJieba() 载入词典（首次约 0.5~2s）。词典缺失时 gojieba 会 panic，
		// 这里兜住并降级为标点切分 —— 与 Python 版 jieba 不可用时的降级路径一致。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[terms] jieba 词典加载失败，退回标点切分: %v", r)
				jiebaInst = nil
			}
		}()
		jiebaInst = gojieba.NewJieba()
	})
	return jiebaInst
}

// CloseTerms 释放 jieba 资源（进程退出时调用）。
func CloseTerms() {
	jiebaOnce.Do(func() {}) // 未初始化则不做任何事
	if jiebaInst != nil {
		jiebaInst.Free()
		jiebaInst = nil
	}
}

type token struct {
	word string
	flag string
}

// taggedTokens 分词 + 词性标注。
// gojieba.Tag 输出形如 "[我们/r 用/p 量子/n]"，此处解析为 (词, 词性) 序列。
func taggedTokens(text string) []token {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	j := jieba()
	if j == nil {
		return fallbackTokens(text)
	}
	jiebaMu.Lock()
	// Tag 返回 ["我们/r" "用/p" "量子/n" ...]，每项为 词/词性
	tagged := j.Tag(text)
	jiebaMu.Unlock()

	out := make([]token, 0, len(tagged))
	for _, f := range tagged {
		// 词本身可能含 "/"，按最后一个 "/" 切分词与词性
		idx := strings.LastIndex(f, "/")
		if idx <= 0 || idx == len(f)-1 {
			if f != "" {
				out = append(out, token{word: f, flag: "x"})
			}
			continue
		}
		out = append(out, token{word: f[:idx], flag: f[idx+1:]})
	}
	return out
}

// fallbackTokens jieba 不可用时按标点切分（词性标为 n），与 Python 版降级路径一致。
func fallbackTokens(text string) []token {
	var out []token
	for _, t := range regexp.MustCompile(`[，。！？；、\s,.!?;]+`).Split(text, -1) {
		if t != "" {
			out = append(out, token{word: t, flag: "n"})
		}
	}
	return out
}

// mergeHeadBlock 不能作为复合术语首部的泛用动词。
//
// 它们是封闭的功能词类（可带任何宾语、不携带学科信息），但 jieba 标成 v，
// 仅靠词性挡不住。放宽 v 词性后实测拼出了「改变载流子」「具有手性」这类假术语，
// 这一层专门拦它们。注意不要收进「控制」「影响」「测量」「观察」这类可作术语首部的词
// （控制理论 / 测量精度 / 观察结果都是正常术语）。
var mergeHeadBlock = map[string]struct{}{
	"改变": {}, "具有": {}, "使用": {}, "采用": {}, "得到": {}, "进行": {}, "实现": {},
	"提供": {}, "包含": {}, "导致": {}, "引起": {}, "形成": {}, "产生": {}, "增加": {},
	"减少": {}, "提高": {}, "降低": {}, "获得": {}, "利用": {}, "需要": {}, "要求": {},
	"表示": {}, "说明": {}, "证明": {}, "给出": {}, "建立": {}, "属于": {}, "存在": {},
	"反映": {}, "对应": {}, "作为": {}, "成为": {}, "变成": {}, "保持": {}, "达到": {},
}

// ---- 规则 ----

func isAcademicCN(w string) bool {
	n := utf8.RuneCountInString(w)
	if n < 2 || n > 10 || !rePureCN.MatchString(w) {
		return false
	}
	if _, bad := stopWords[w]; bad {
		return false
	}
	// 注：这里曾有一条「含 stopWord 子串即否决」的规则，用来挡「这个结构」这类噪声。
	// 但 w 是 jieba 已切好的**单个 token**，口语词在分词阶段就被切开了，该规则实际
	// 拦下的是正常复合术语：作用力(含"作用")、工作温度(含"工作")、关系代数(含"关系")、
	// 过程控制(含"过程")、结果分析(含"结果")——全部被误杀。噪声改由上面的整词相等
	// 检查与合并路径的 stopWords 检查负责，这里不再做子串否决。
	for _, suf := range academicSuffix {
		if strings.HasSuffix(w, suf) {
			return true
		}
	}
	for _, pre := range academicPrefix {
		if strings.HasPrefix(w, pre) {
			return true
		}
	}
	return false
}

// isUpperWord 等价 Python 的 str.isupper()：至少一个字母，且无小写字母。
func isUpperWord(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if unicode.IsLower(r) {
				return false
			}
		}
	}
	return hasLetter
}

// Term 一个候选术语。
type Term struct {
	Term  string  `json:"term"`
	Score float64 `json:"score"`
}

// ScoreText 对一句转写文本提取候选术语并打分，返回按分数降序的结果（最多 10 个）。
func ScoreText(text string, glossary map[string]int) []Term {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	cand := make(map[string]float64)

	// 1) 命中会前/科学家术语表（最高分；表内权重高的更靠前）
	for term, weight := range glossary {
		if utf8.RuneCountInString(term) >= 2 && strings.Contains(text, term) {
			v := 3.0 + float64(weight)/1000.0
			if v > cand[term] {
				cand[term] = v
			}
		}
	}

	tagged := taggedTokens(text)

	// 2) 英文缩写 / 3) 中英混排 / 4) 中文学术名词（仅名词性词参与）
	for _, tk := range tagged {
		w := strings.TrimSpace(tk.word)
		if w == "" {
			continue
		}
		if _, bad := stopWords[w]; bad {
			continue
		}
		if utf8.RuneCountInString(w) < 2 {
			continue
		}
		hasCN := reHasCN.MatchString(w)
		hasEN := reHasEN.MatchString(w)
		switch {
		case hasEN && !hasCN:
			if !isAbbreviation(w) {
				continue
			}
			if _, bad := enStop[strings.ToLower(w)]; bad {
				continue
			}
			bonus := 0.0
			if isUpperWord(w) { // 全大写更像专业缩写
				bonus = 0.4
			}
			setMax(cand, w, 1.6+bonus)
		case hasEN && hasCN:
			setMax(cand, w, 1.4)
		default:
			if _, ok := termFlags[tk.flag]; ok && isAcademicCN(w) {
				setMax(cand, w, 1.0)
			}
		}
	}

	// 5) 相邻术语词组合成复合术语（超导+相变 → 超导相变）
	//    要求两部分均为术语性词性且长度≥2，避免 "的"+"模型"、"观察"+"蛋白" 这类噪声
	for i := 0; i+1 < len(tagged); i++ {
		a, b := tagged[i], tagged[i+1]
		if _, ok := termFlags[a.flag]; !ok {
			continue
		}
		if _, ok := termFlags[b.flag]; !ok {
			continue
		}
		if utf8.RuneCountInString(a.word) < 2 || utf8.RuneCountInString(b.word) < 2 {
			continue
		}
		if _, bad := stopWords[a.word]; bad {
			continue
		}
		if _, bad := stopWords[b.word]; bad {
			continue
		}
		if _, bad := mergeHeadBlock[a.word]; bad {
			continue
		}
		if !rePureCN.MatchString(a.word) || !rePureCN.MatchString(b.word) {
			continue
		}
		merged := a.word + b.word
		n := utf8.RuneCountInString(merged)
		if n >= 4 && n <= 10 && isAcademicCN(merged) {
			setMax(cand, merged, 1.2) // 复合术语信息量更大
		}
	}

	// 去掉被更长候选完全包含且分数不更高的短候选
	keys := make([]string, 0, len(cand))
	for k := range cand {
		keys = append(keys, k)
	}
	// 长度降序；同长度按字典序（Python 版同长度互不包含，结果等价，这里额外保证可复现）
	sort.Slice(keys, func(i, j int) bool {
		li, lj := utf8.RuneCountInString(keys[i]), utf8.RuneCountInString(keys[j])
		if li != lj {
			return li > lj
		}
		return keys[i] < keys[j]
	})
	for i, longT := range keys {
		if _, alive := cand[longT]; !alive {
			continue
		}
		for _, shortT := range keys[i+1:] {
			if sv, ok := cand[shortT]; ok && strings.Contains(longT, shortT) && sv <= cand[longT] {
				delete(cand, shortT)
			}
		}
	}

	type scored struct {
		term  string
		score float64
	}
	ranked := make([]scored, 0, len(cand))
	for k, v := range cand {
		ranked = append(ranked, scored{k, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		li, lj := utf8.RuneCountInString(ranked[i].term), utf8.RuneCountInString(ranked[j].term)
		if li != lj {
			return li > lj
		}
		return ranked[i].term < ranked[j].term
	})
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}

	top := 1.0
	if len(ranked) > 0 {
		top = ranked[0].score
	}
	if top < 1.0 {
		top = 1.0
	}
	out := make([]Term, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, Term{Term: r.term, Score: round2(math.Min(r.score/top, 1.0))})
	}
	return out
}

func setMax(m map[string]float64, k string, v float64) {
	if cur, ok := m[k]; !ok || v > cur {
		m[k] = v
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
