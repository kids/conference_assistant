package server

import (
	"context"
	"log"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seat/internal/diarize"
	"seat/internal/events"
)

// 说话人区分（CAM++ sidecar）与主链路的衔接。
//
// 数据流：流水线把「一次连续说话」的音频交给 onSpan → 有界队列 → 工作协程调 sidecar
// 取说话人 → 与已落库的句子按时间对齐 → 写回 segment.speaker_hint + 推事件。
//
// 为什么异步：sidecar 单段推理 0.1~1s，绝不能阻塞音频流水线（那会直接丢音频）。
// 为什么能对上：流水线给出的语音段带精确起止时间，句子落库带 [t_start,t_end] 估计；
// 两边都随时间单调推进，取「重叠最多 / 结束时间最接近」的一段即可。
// 两个方向的到达顺序都支持：本地 VAD 模式句子通常先到，服务端断句模式语音段先到。

const (
	// spanQueue 待判定语音段队列深度。每个元素 ≤ 30s 音频（≤1MB），
	// sidecar 打满时宁可丢段也不能让内存增长（丢段只影响那几句的说话人标记）。
	spanQueue = 8
	// pendingTTL 未配对的句子/语音段保留时长：超过即丢弃，避免长会议内存增长。
	pendingTTL = 120.0
	// pendingMax 两个待配对列表的长度上限。
	pendingMax = 60
	// diarizeHealthTTL 健康探活缓存时长：/api/health 每 3 秒轮询，不必每次都打 sidecar。
	diarizeHealthTTL = 10 * time.Second
	// gapMatch 无重叠时的最大容忍间隔（秒）：服务端语义断句会晚于真实语音结束，
	// 句子结束时间比语音段晚几秒是常态；超过这个间隔就不再往一起配。
	gapMatch = 8.0
	// shareWindow 同段继承的时间窗（秒）：语音段配到句子后，只有在这个窗口内的
	// 后续句子才允许继承它的说话人（否则长会议里最后一段会一直"辐射"到很远的句子）。
	shareWindow = 20.0
	// settleAfter 弱匹配（无重叠、仅时间接近）的等待时长（秒）。
	// 句子的时间窗是按字数估的，能盖住好几秒；若它刚落地就允许弱匹配，会抢在
	// 「真正对应的那段语音」的判定结果到达之前，把它配给更早的一段（实测踩过：
	// 说话人回到 S1 的那段被判成上一段的 S2，并顺着继承污染了后面所有句子）。
	// 有真重叠时不受此限。
	settleAfter = 4.0
	// sweepInterval 定期结算等待中的句子（没有新事件时也要让弱匹配落地）。
	sweepInterval = 2 * time.Second
	// inheritMinOverlap 允许"继承说话人"的最小重叠（秒）。
	// 句子的时间窗是按字数估的，会略微"溢出"到相邻语音段；若只沾一点边就继承，
	// 会把「说下一段的人」的编号提前盖到这一段上（实测踩过：回到 S1 的那段里，
	// 前两句因为与上一段重叠 0.4s 被判成 S2，而 S1 的判定结果 1 秒后才到）。
	inheritMinOverlap = 1.0
	// inheritMinContain 允许继承时「重叠 / 句子估计时长」的最小占比。
	// 造句子的时间窗按字数估得偏宽（一句话能盖住七八秒），常横跨两个语音段；
	// 只有大部分落在这一段里才算"就是这段说的话"，否则再等下一段的判定结果。
	inheritMinContain = 0.5
)

// scoreOverlap / scoreGap 匹配得分量级：重叠 > 接近。
// 只有重叠允许复用（继承），接近只用于「服务端断句滞后」的兜底。
const (
	scoreOverlapBase = 2.0
	scoreGapBase     = 1.0
)

// base0 调试日志的时间原点（进程启动时刻）：日志里显示「相对秒」，
// 与页面上的会议计时口径一致，便于和操作员描述的现象对照。
var base0 = float64(time.Now().UnixNano()) / 1e9

// spanJob 一段待判定的语音。
type spanJob struct {
	sid   string
	dir   string
	start float64
	end   float64
	pcm   []byte
}

// pendingSeg 已落库、尚未拿到说话人的句子。
type pendingSeg struct {
	segID      string
	start, end float64
	at         float64
}

// pendingSpan 已判定、尚未配到句子的语音段。
type pendingSpan struct {
	start, end float64
	res        diarize.Result
	at         float64
}

// matched 一句话与它的说话人判定结果。
type matched struct {
	segID string
	res   diarize.Result
	// inherited 表示这句不是独立判定出来的，而是继承了同一段语音的说话人。
	inherited bool
}

// spkState 说话人区分的运行状态（与 rt.mu 分开，避免与 session 状态互相拖慢）。
type spkState struct {
	mu     sync.Mutex
	segs   []pendingSeg
	spans  []pendingSpan
	used   []pendingSpan // 已配过句子的语音段：供同一段语音内的后续句子继承说话人
	health map[string]any
	at     time.Time
	err    string

	assigned  atomic.Int64 // 成功回填的句子数
	inherited atomic.Int64 // 其中由同一段语音继承（非首次判定）的数量
	expired   atomic.Int64 // 超时未配对而被丢弃的数量
	dropped   atomic.Int64 // 队列满丢弃的语音段数
	failed    atomic.Int64 // sidecar 调用失败次数
}

// startDiarize 启动说话人区分工作协程（幂等；未启用时为空操作）。
func (rt *Runtime) startDiarize() {
	if rt.diarizer == nil {
		return
	}
	rt.spkOnce.Do(func() {
		go rt.diarizeLoop()
		go rt.ensureSidecar()
	})
}

// onSpan 流水线回调：把一段语音交给说话人区分（非阻塞，队列满则丢弃）。
func (rt *Runtime) onSpan(start, end float64, pcm []byte) {
	if rt.diarizer == nil {
		return
	}
	if ms := rt.diarizer.MinMS(); ms > 0 && float64(len(pcm))/2/16000*1000 < float64(ms) {
		return
	}
	sid := rt.SessionID()
	if sid == "" {
		return
	}
	select {
	case rt.spkCh <- spanJob{sid: sid, dir: rt.sessionDir(sid), start: start, end: end, pcm: pcm}:
	default:
		rt.spk.dropped.Add(1)
	}
}

func (rt *Runtime) sessionDir(sid string) string {
	if sid == "" {
		return ""
	}
	return filepath.Join(rt.Settings.DataDirPath(), sid)
}

// diarizeLoop 串行消费语音段：调 sidecar → 与句子对齐 → 写回。
// 另带一个慢心跳：结算那些为了等"真正对应的语音段"而暂时挂起的句子。
func (rt *Runtime) diarizeLoop() {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-rt.spkStop:
			return
		case <-sweep.C:
			rt.diarizeTick()
		case job := <-rt.spkCh:
			ctx, cancel := context.WithTimeout(context.Background(),
				time.Duration(rt.Settings.DiarizeTimeout+10)*time.Second)
			res, err := rt.diarizer.Assign(ctx, job.sid, job.dir, job.pcm)
			cancel()
			if err != nil {
				// sidecar 未启动时会持续失败：只打第一条，其余静默计数（健康接口可见）
				if rt.spk.failed.Add(1) == 1 {
					log.Printf("[diarize] 调用 sidecar 失败（后续静默计数）：%v", err)
				}
				continue
			}
			if res.Speaker == "" {
				continue
			}
			if rt.Settings.DiarizeDebug {
				log.Printf("[diarize:debug] 语音段 %.1f~%.1f（%.1fs）→ %s conf=%.2f new=%v",
					job.start-base0, job.end-base0, (job.end - job.start), res.Speaker, res.Confidence, res.IsNew)
			}
			rt.spk.mu.Lock()
			rt.spk.spans = append(rt.spk.spans, pendingSpan{
				start: job.start, end: job.end, res: res, at: nowSeconds(),
			})
			rt.spk.mu.Unlock()
			rt.diarizeTick()
		}
	}
}

// diarizeTick 触发一次配对并把结果写回（debug 时打印决策明细）。
func (rt *Runtime) diarizeTick() {
	rt.spk.mu.Lock()
	pairs := rt.matchLocked()
	rt.spk.mu.Unlock()
	if rt.Settings.DiarizeDebug {
		rt.spk.mu.Lock()
		if len(pairs) == 0 && len(rt.spk.segs) > 0 {
			for _, s := range rt.spk.segs {
				log.Printf("[diarize:debug] 句子 %s（估计 %.1f~%.1f）暂未配对，待后续语音段",
					s.segID, s.start-base0, s.end-base0)
			}
		}
		rt.spk.mu.Unlock()
		for _, p := range pairs {
			kind := "判定"
			if p.inherited {
				kind = "继承"
			}
			log.Printf("[diarize:debug] %s %s → %s", kind, p.segID, p.res.Speaker)
		}
	}
	rt.applySpeakers(pairs)
}

// noteSegment 登记一句刚落库的转写，等说话人结果到达后回填。
func (rt *Runtime) noteSegment(segID string, start, end float64) {
	if rt.diarizer == nil {
		return
	}
	rt.spk.mu.Lock()
	rt.spk.segs = append(rt.spk.segs, pendingSeg{segID: segID, start: start, end: end, at: nowSeconds()})
	rt.spk.mu.Unlock()
	rt.diarizeTick()
}

// matchLocked 贪心配对：为「最早的未配对句子」找「最合适的未配对语音段」。
//
// 两遍：
//  1. 首次判定——一个语音段配一句话（重叠优先，仅"接近"的弱匹配要等 settleAfter），
//     配上的段进入 used，供第二遍继承；
//  2. 同段继承——一段连续说话里 ASR 可能吐出多句（服务端语义断句也会），
//     这些句子与该段音频实打实重叠（且重叠占句子估计时长的大头）时继承其说话人。
//     擦边重叠、时间上无关的句子都不继承：宁可不标，也不标错人。
//
// 调用方须持有 rt.spk.mu。
func (rt *Runtime) matchLocked() []matched {
	rt.pruneLocked()
	now := nowSeconds()
	var out []matched
	usedSeg := make([]bool, len(rt.spk.segs))
	usedSpan := make([]bool, len(rt.spk.spans))

	for i := range rt.spk.segs {
		seg := rt.spk.segs[i]
		best, bestScore := -1, 0.0
		for j := range rt.spk.spans {
			if usedSpan[j] {
				continue
			}
			score := spanSegScore(seg, rt.spk.spans[j].start, rt.spk.spans[j].end)
			if score > bestScore {
				best, bestScore = j, score
			}
		}
		if best < 0 {
			continue
		}
		// 只有「时间上真重叠」的匹配可以立即生效；仅"接近"的弱匹配要等一会儿 ——
		// 句子刚落地的几秒内，真正对应的那段语音的判定结果可能还在路上。
		if bestScore < scoreOverlapBase && now-seg.at < settleAfter {
			continue
		}
		usedSeg[i], usedSpan[best] = true, true
		// 立即登记到 used：同一段连续说话里被 ASR 拆出的后续句子，
		// 在本次调用里就能继承到说话人（不必等下一次事件）
		rt.spk.used = append(rt.spk.used, rt.spk.spans[best])
		out = append(out, matched{segID: seg.segID, res: rt.spk.spans[best].res})
	}

	for i := range rt.spk.segs {
		if usedSeg[i] {
			continue
		}
		seg := rt.spk.segs[i]
		best, bestScore := -1, 0.0
		for j := range rt.spk.used {
			u := rt.spk.used[j]
			if u.at < seg.at-shareWindow {
				continue // 太老的段不再往外"扩散"说话人
			}
			// 继承只认「实打实的重叠」：同一段连续说话被 ASR 拆成多句时才继承，
			// 不允许凭"擦边"或"接近"把说话人扩散到相邻时段的句子上。
			overlap := spanOverlap(seg, u.start, u.end)
			if overlap < inheritMinOverlap {
				continue
			}
			// 且重叠要占这句估计时长的大头：窗口横跨两段时，等下一段的判定结果
			if span := seg.end - seg.start; span > 0 && overlap/span < inheritMinContain {
				continue
			}
			if score := spanSegScore(seg, u.start, u.end); score > bestScore {
				best, bestScore = j, score
			}
		}
		if best < 0 {
			continue
		}
		usedSeg[i] = true
		out = append(out, matched{segID: seg.segID, res: rt.spk.used[best].res, inherited: true})
	}

	if len(out) == 0 {
		return nil
	}
	segs := make([]pendingSeg, 0, len(rt.spk.segs))
	for i, s := range rt.spk.segs {
		if !usedSeg[i] {
			segs = append(segs, s)
		}
	}
	rt.spk.segs = segs
	spans := make([]pendingSpan, 0, len(rt.spk.spans))
	for j, s := range rt.spk.spans {
		if !usedSpan[j] {
			spans = append(spans, s)
		}
	}
	rt.spk.spans = spans
	if len(rt.spk.used) > pendingMax {
		rt.spk.used = rt.spk.used[len(rt.spk.used)-pendingMax:]
	}
	return out
}

// spanSegScore 语音段与句子的匹配度：重叠优先，其次看结束时间接近程度。
// 返回 0 表示不匹配（宁可不标说话人，也不要标错人）。
// 得分 >= scoreOverlapBase 才叫「重叠」（可复用/可继承），低于它是「接近」的兜底匹配。
func spanSegScore(seg pendingSeg, spanStart, spanEnd float64) float64 {
	if overlap := spanOverlap(seg, spanStart, spanEnd); overlap > 0.05 {
		return scoreOverlapBase + overlap // 有重叠：重叠越多越可信
	}
	gap := math.Abs(seg.end - spanEnd)
	if gap <= gapMatch {
		// 无重叠但时间接近（服务端断句滞后 / 短句时长估计偏短）
		return scoreGapBase - gap/100.0
	}
	return 0
}

// spanOverlap 句子估计时间窗与语音段的重叠秒数（<=0 表示不重叠）。
func spanOverlap(seg pendingSeg, spanStart, spanEnd float64) float64 {
	return math.Min(seg.end, spanEnd) - math.Max(seg.start, spanStart)
}

// applySpeakers 回填说话人标记并通知前端。
func (rt *Runtime) applySpeakers(pairs []matched) {
	for _, p := range pairs {
		if err := rt.Store.SetSegmentSpeaker(p.segID, p.res.Speaker); err != nil {
			log.Printf("[diarize] 写回说话人失败 %s: %v", p.segID, err)
			continue
		}
		label := p.res.Label
		if label == "" {
			label = speakerLabel(p.res.Speaker)
		}
		rt.spk.assigned.Add(1)
		if p.inherited {
			rt.spk.inherited.Add(1)
		}
		rt.Bus.Publish(events.Event{
			"type": "SPEAKER_ASSIGNED", "seg_id": p.segID,
			"speaker": p.res.Speaker, "label": label,
			"confidence": p.res.Confidence, "is_new": p.res.IsNew,
			"speaker_count": p.res.SpeakerCount,
		})
	}
}

// pruneLocked 丢弃过老的未配对项（长会议里它们永远配不上，留着只占内存）。
func (rt *Runtime) pruneLocked() {
	now := nowSeconds()
	segs := rt.spk.segs[:0]
	for _, s := range rt.spk.segs {
		if now-s.at <= pendingTTL {
			segs = append(segs, s)
		} else {
			rt.spk.expired.Add(1)
		}
	}
	rt.spk.segs = segs
	spans := rt.spk.spans[:0]
	for _, s := range rt.spk.spans {
		if now-s.at <= pendingTTL {
			spans = append(spans, s)
		} else {
			rt.spk.expired.Add(1)
		}
	}
	rt.spk.spans = spans
	used := rt.spk.used[:0]
	for _, s := range rt.spk.used {
		if now-s.at <= pendingTTL {
			used = append(used, s)
		}
	}
	rt.spk.used = used
	if len(rt.spk.segs) > pendingMax {
		rt.spk.segs = rt.spk.segs[len(rt.spk.segs)-pendingMax:]
	}
	if len(rt.spk.spans) > pendingMax {
		rt.spk.spans = rt.spk.spans[len(rt.spk.spans)-pendingMax:]
	}
	if len(rt.spk.used) > pendingMax {
		rt.spk.used = rt.spk.used[len(rt.spk.used)-pendingMax:]
	}
}

// resetDiarize 新建 session 时调用：清空待配对列表并重置 sidecar 的说话人表。
func (rt *Runtime) resetDiarize(sid string) {
	if rt.diarizer == nil {
		return
	}
	rt.spk.mu.Lock()
	rt.spk.segs = nil
	rt.spk.spans = nil
	rt.spk.used = nil
	rt.spk.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rt.diarizer.Reset(ctx, sid, rt.sessionDir(sid)); err != nil {
			log.Printf("[diarize] 重置 session %s 说话人表失败（不影响转写）：%v", sid, err)
		}
	}()
}

// DiarizeStatus 供 /api/health 使用：off / ok / offline。
// 探活结果缓存 10 秒，避免前端轮询把 sidecar 打满。
func (rt *Runtime) DiarizeStatus() (string, map[string]any) {
	if rt.diarizer == nil {
		return "off", map[string]any{"enabled": false}
	}
	detail := map[string]any{
		"enabled":   true,
		"url":       rt.Settings.DiarizeURL,
		"assigned":  rt.spk.assigned.Load(),
		"inherited": rt.spk.inherited.Load(),
		"expired":   rt.spk.expired.Load(),
		"dropped":   rt.spk.dropped.Load(),
		"failed":    rt.spk.failed.Load(),
	}
	rt.spk.mu.Lock()
	cached, at, errMsg := rt.spk.health, rt.spk.at, rt.spk.err
	rt.spk.mu.Unlock()
	if (cached != nil || errMsg != "") && time.Since(at) < diarizeHealthTTL {
		for k, v := range cached {
			detail[k] = v
		}
		if errMsg != "" {
			detail["error"] = errMsg
			return "offline", detail
		}
		return "ok", detail
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := rt.diarizer.Health(ctx)
	rt.spk.mu.Lock()
	rt.spk.at = time.Now()
	if err != nil {
		rt.spk.health, rt.spk.err = nil, err.Error()
	} else {
		rt.spk.health, rt.spk.err = info, ""
	}
	rt.spk.mu.Unlock()
	if err != nil {
		detail["error"] = err.Error()
		return "offline", detail
	}
	for k, v := range info {
		detail[k] = v
	}
	return "ok", detail
}

// ---- sidecar 自动拉起 ----

// ensureSidecar 若 sidecar 不可达且配置允许，则拉起 tools/diarize/run.sh。
// 首次运行该脚本会自动建 venv、装依赖、下模型（约 3~5 分钟），期间说话人标记自然缺失，
// 就绪后无需重启主程序即可自动生效（每段都会重试连接）。
func (rt *Runtime) ensureSidecar() {
	s := rt.Settings
	if !s.DiarizeAutostart {
		return
	}
	host, port, err := splitURL(s.DiarizeURL)
	if err != nil || !isLoopback(host) {
		return // 只自动拉起本机 sidecar；远端地址由部署方自行保证
	}
	script := filepath.Join(s.BaseDir, "tools", "diarize", "run.sh")
	if _, err := os.Stat(script); err != nil {
		log.Printf("[diarize] %s 不可达，且未找到 %s：说话人区分不可用"+
			"（可手动 `bash tools/diarize/run.sh --port %s` 启动）", s.DiarizeURL, script, port)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = rt.diarizer.Health(ctx)
	cancel()
	if err == nil {
		return
	}
	logPath := filepath.Join(s.DataDirPath(), "diarize.log")
	if err := mkdirAll(filepath.Dir(logPath)); err != nil {
		log.Printf("[diarize] 创建日志目录失败: %v", err)
	}
	// 路径即 BASE_DIR 所在仓库，脚本用 bash 执行（无需可执行位）
	cmd := exec.Command("bash", script, "--port", port)
	cmd.Env = os.Environ()
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[diarize] 拉起 sidecar 失败: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }() // 回收子进程，避免僵尸
	log.Printf("[diarize] 已拉起 CAM++ sidecar（首次运行会装依赖+下模型，日志：%s）", logPath)
}

func splitURL(raw string) (host, port string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	host, port = u.Hostname(), u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return host, port, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Speakers 本场已识别出的说话人（按编号排序），供控制台展示。
func (rt *Runtime) Speakers() []map[string]any {
	sid := rt.SessionID()
	out := []map[string]any{}
	if sid == "" {
		return out
	}
	stats, err := rt.Store.SessionSpeakers(sid)
	if err != nil {
		log.Printf("[diarize] 查询说话人失败: %v", err)
		return out
	}
	sort.SliceStable(stats, func(i, j int) bool { return stats[i].Speaker < stats[j].Speaker })
	for _, st := range stats {
		out = append(out, map[string]any{
			"speaker": st.Speaker, "label": speakerLabel(st.Speaker),
			"segments": st.Segments, "seconds": math.Round(st.Seconds*10) / 10,
		})
	}
	return out
}

// speakerLabel S1 → 说话人 1（与 sidecar 的 label 口径一致，前端只认 id）。
func speakerLabel(id string) string {
	if len(id) > 1 && (id[0] == 'S' || id[0] == 's') {
		if n := strings.TrimLeft(id[1:], "0"); n != "" {
			return "说话人 " + n
		}
	}
	return id
}
