package agent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"seat/internal/llm"
)

// 转写终稿的 LLM 顺句改写（后台队列，失败静默降级）。
//
// 设计要点（对齐 Python 版 app/asr/refine.py）：
//   - 独立后台协程 + 有界队列：不阻塞 ASR 接收协程，队列满则丢弃（保实时性优先）；
//   - 只改写达到长度门槛的句子；
//   - 附带会前热词作为参考，帮助纠正专业术语；
//   - 任何异常都静默跳过，绝不影响原始转写显示。
const refinePrompt = `你是会议速记校对员。请修正下面这句会议语音识别（ASR）转写文本中的明显错误。

严格要求：
1. 只做修正，不做改写：不得增删内容、不得补充解释、不得改变说话人原意。
2. 修正范围仅限：同音错别字、缺失或错误的标点、明显的识别错词。
3. 删除无意义口语赘词（呃、啊、那个、就是说 等），但保留正常语气。
4. 若参考术语表中的术语被识别成了近音词，请改回术语表中的写法。
5. 直接输出修正后的句子本身，不要输出任何解释、引号或前后缀。
6. 若原句已无明显错误，原样输出。

{{HOTWORDS}}原句：{{TEXT}}`

type refineTask struct {
	segID    string
	text     string
	hotwords map[string]int
}

// Refiner 后台 LLM 顺句改写器。
type Refiner struct {
	llm       *llm.Client
	onRefined func(segID, text string)
	minChars  int
	timeout   time.Duration

	q       chan refineTask
	stop    chan struct{}
	done    chan struct{}
	started sync.Once
	stopped sync.Once

	dropped      atomic.Int64
	refinedCount atomic.Int64
}

// NewRefiner 创建改写器。llm 为 nil 时整体禁用。
func NewRefiner(client *llm.Client, onRefined func(segID, text string), minChars int) *Refiner {
	if minChars <= 0 {
		minChars = 12
	}
	return &Refiner{
		llm:       client,
		onRefined: onRefined,
		minChars:  minChars,
		timeout:   8 * time.Second,
		q:         make(chan refineTask, 8),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start 启动后台协程（幂等）。
func (r *Refiner) Start() {
	if r == nil || r.llm == nil {
		return
	}
	r.started.Do(func() { go r.loop() })
}

// Stop 停止后台协程。
func (r *Refiner) Stop() {
	if r == nil {
		return
	}
	r.stopped.Do(func() { close(r.stop) })
}

// Submit 提交一句待改写文本（非阻塞；队列满或不满足门槛则跳过）。
func (r *Refiner) Submit(segID, text string, hotwords map[string]int) {
	if r == nil || r.llm == nil {
		return
	}
	if utf8.RuneCountInString(text) < r.minChars {
		return
	}
	select {
	case r.q <- refineTask{segID: segID, text: text, hotwords: hotwords}:
	default:
		r.dropped.Add(1) // 高峰期丢弃，优先保证实时转写
	}
}

func (r *Refiner) loop() {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			return
		case task := <-r.q:
			refined := r.refine(task)
			if refined != "" && refined != task.text {
				r.refinedCount.Add(1)
				if r.onRefined != nil {
					func() {
						defer func() { _ = recover() }()
						r.onRefined(task.segID, refined)
					}()
				}
			}
		}
	}
}

func (r *Refiner) refine(task refineTask) string {
	// 只挑权重高的少量术语作参考，控制 prompt 体积
	block := ""
	if len(task.hotwords) > 0 {
		top := make([]string, 0, len(task.hotwords))
		type kv struct {
			w string
			v int
		}
		items := make([]kv, 0, len(task.hotwords))
		for w, v := range task.hotwords {
			items = append(items, kv{w, v})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].v != items[j].v {
				return items[i].v > items[j].v
			}
			return items[i].w < items[j].w
		})
		if len(items) > 20 {
			items = items[:20]
		}
		for _, it := range items {
			top = append(top, it.w)
		}
		block = "参考术语表：" + strings.Join(top, "、") + "\n\n"
	}

	prompt := strings.ReplaceAll(refinePrompt, "{{HOTWORDS}}", block)
	prompt = strings.ReplaceAll(prompt, "{{TEXT}}", task.text)

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	out, err := r.llm.Chat(ctx, []llm.Message{{Role: "user", Content: prompt}}, 200, 0.0)
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, "\"“”")
	// 防御：LLM 跑偏（输出过长/空）时放弃改写
	if out == "" || utf8.RuneCountInString(out) > utf8.RuneCountInString(task.text)*2+20 {
		return ""
	}
	return out
}

// Stats 统计信息（队列丢弃数 / 成功改写数）。
func (r *Refiner) Stats() (dropped, refined int64) {
	if r == nil {
		return 0, 0
	}
	return r.dropped.Load(), r.refinedCount.Load()
}
