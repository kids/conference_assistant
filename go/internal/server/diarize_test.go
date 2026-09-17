package server

import (
	"testing"

	"seat/internal/diarize"
)

// 说话人结果与句子的对齐规则：「标错人」比「不标」更糟，因此这里把边界钉死。

func TestSpanSegScorePrefersOverlap(t *testing.T) {
	seg := pendingSeg{segID: "s1", start: 100, end: 110}

	// 重叠的分数量级必须高于「仅接近」，否则弱匹配会抢在真正的那段语音之前
	if got := spanSegScore(seg, 101, 109); got < scoreOverlapBase {
		t.Errorf("重叠段应得 >=%v，实际 %v", scoreOverlapBase, got)
	}
	partial := spanSegScore(seg, 108, 116)
	full := spanSegScore(seg, 100, 110)
	if partial <= scoreOverlapBase || partial >= full {
		t.Errorf("部分重叠应 >=底线且小于完全重叠：partial=%v full=%v", partial, full)
	}
	// 无重叠、相距 3s：服务端断句滞后可容忍，给保守分（低于任何重叠）
	if got := spanSegScore(seg, 113, 118); got <= 0 || got >= scoreOverlapBase {
		t.Errorf("接近但无重叠应给低于重叠的保守分，实际 %v", got)
	}
	// 无重叠且相距很远：不匹配（宁可不标）
	if got := spanSegScore(seg, 130, 140); got != 0 {
		t.Errorf("相距 20s 不应匹配，实际 %v", got)
	}
}

func TestSpanSegScoreBoundaries(t *testing.T) {
	seg := pendingSeg{segID: "s1", start: 100, end: 110}
	// 判定依据是「句子结束时间与语音段结束时间的间隔」，因此构造时保持段长 2s 只挪结束点
	if got := spanSegScore(seg, 110+gapMatch+0.5-2, 110+gapMatch+0.5); got != 0 {
		t.Errorf("超出容忍间隔应不匹配，实际 %v", got)
	}
	if got := spanSegScore(seg, 110+gapMatch-0.5-2, 110+gapMatch-0.5); got == 0 {
		t.Errorf("容忍间隔内应匹配，实际 %v", got)
	}
	// 恰好擦边重叠（重叠 0.05 以下不按重叠计分，避免用一帧噪声绑定两句）
	if got := spanSegScore(seg, 110.01, 120); got >= scoreOverlapBase {
		t.Errorf("极小重叠不应按重叠计分，实际 %v", got)
	}
}

// 弱匹配（无重叠、仅时间接近）必须等一会儿：刚落地的句子先把机会留给
// 「真正那段语音」的判定结果，否则会配错人并顺着继承污染后面的句子。
func TestMatchLockedDefersWeakMatch(t *testing.T) {
	now := nowSeconds()
	rt := &Runtime{}
	// 句子窗口 [100,110]，语音段在 3 秒后结束：无重叠、但落在容忍间隔内
	rt.spk.segs = []pendingSeg{{segID: "fresh", start: 100, end: 110, at: now}}
	rt.spk.spans = []pendingSpan{{start: 111, end: 113, res: diarize.Result{Speaker: "S2"}, at: now}}
	if pairs := rt.matchLocked(); len(pairs) != 0 {
		t.Fatalf("刚落地的句子不应立刻吃弱匹配，实际 %+v", pairs)
	}
	// 过了等待期仍未等到重叠的段，才接受弱匹配（服务端断句滞后的兜底）
	rt.spk.segs[0].at = now - settleAfter - 1
	if pairs := rt.matchLocked(); len(pairs) != 1 || pairs[0].res.Speaker != "S2" {
		t.Fatalf("等待期后应接受弱匹配，实际 %+v", pairs)
	}
}

func TestMatchLockedPairsBothDirections(t *testing.T) {
	// 句子先到（本地 VAD 模式）：标记后句子列表应清空
	rt := &Runtime{}
	rt.spk.segs = []pendingSeg{{segID: "seg-1", start: 0, end: 5, at: nowSeconds()}}
	rt.spk.spans = []pendingSpan{{start: 0.2, end: 5.2, res: diarize.Result{Speaker: "S1"}, at: nowSeconds()}}
	pairs := rt.matchLocked()
	if len(pairs) != 1 || pairs[0].segID != "seg-1" || pairs[0].res.Speaker != "S1" {
		t.Fatalf("应配对出 seg-1→S1，实际 %+v", pairs)
	}
	if len(rt.spk.segs) != 0 || len(rt.spk.spans) != 0 {
		t.Errorf("配对后两侧都应清空：segs=%d spans=%d", len(rt.spk.segs), len(rt.spk.spans))
	}

	// 语音段先到（服务端断句滞后）：同样要能配上
	rt2 := &Runtime{}
	rt2.spk.spans = []pendingSpan{{start: 10, end: 16, res: diarize.Result{Speaker: "S2"}, at: nowSeconds()}}
	rt2.spk.segs = []pendingSeg{{segID: "seg-9", start: 12, end: 20, at: nowSeconds()}}
	if pairs := rt2.matchLocked(); len(pairs) != 1 || pairs[0].res.Speaker != "S2" {
		t.Fatalf("语音段先到也应配对，实际 %+v", pairs)
	}

	// 同一段语音不会被两句话共用
	rt3 := &Runtime{}
	rt3.spk.segs = []pendingSeg{
		{segID: "a", start: 0, end: 6, at: nowSeconds()},
		{segID: "b", start: 6.5, end: 12, at: nowSeconds()},
	}
	rt3.spk.spans = []pendingSpan{{start: 0, end: 6, res: diarize.Result{Speaker: "S1"}, at: nowSeconds()}}
	if pairs := rt3.matchLocked(); len(pairs) != 1 || pairs[0].segID != "a" {
		t.Fatalf("一段语音只能配给一句话（最早那句），实际 %+v", pairs)
	}
	if len(rt3.spk.segs) != 1 || rt3.spk.segs[0].segID != "b" {
		t.Errorf("未配对的句子应保留待配，实际 %+v", rt3.spk.segs)
	}
}

// 一段连续说话里 ASR 常吐多句（服务端语义断句也会），它们必须继承同一说话人，
// 否则页面上只有"第一句有标记"，看起来像标记丢了。
func TestMatchLockedInheritsWithinSameSpan(t *testing.T) {
	now := nowSeconds()
	rt := &Runtime{}
	// 同一段语音（0~10s）里的三句话
	rt.spk.segs = []pendingSeg{
		{segID: "a", start: 0, end: 4, at: now},
		{segID: "b", start: 3, end: 7, at: now},
		{segID: "c", start: 6, end: 10, at: now},
	}
	rt.spk.spans = []pendingSpan{{start: 0, end: 10, res: diarize.Result{Speaker: "S1"}, at: now}}
	pairs := rt.matchLocked()
	if len(pairs) != 3 {
		t.Fatalf("同一段语音里的三句应一次配齐（1 次判定 + 2 次继承），实际 %+v", pairs)
	}
	primary, inherited := 0, 0
	for _, m := range pairs {
		if m.res.Speaker != "S1" {
			t.Errorf("说话人应一致：%+v", m)
		}
		if m.inherited {
			inherited++
		} else {
			primary++
		}
	}
	if primary != 1 || inherited != 2 {
		t.Errorf("应为 1 次判定 + 2 次继承，实际 primary=%d inherited=%d", primary, inherited)
	}
	if len(rt.spk.segs) != 0 {
		t.Errorf("三句都应有标记，剩余 %+v", rt.spk.segs)
	}
	if len(rt.spk.used) != 1 {
		t.Errorf("配过的语音段应留一条供后续继承，实际 %d", len(rt.spk.used))
	}
}

// 与已用语音段只沾一点点边时不许继承：那通常是"下一段说话人"的判定结果还没到，
// 而估算时间窗略微溢出到了上一段上（实测这种边缘重叠会把下一段的人盖成上一段的人）。
func TestMatchLockedRejectsMarginalInheritance(t *testing.T) {
	now := nowSeconds()
	rt := &Runtime{}
	rt.spk.segs = []pendingSeg{{segID: "edge", start: 25.9, end: 26.1, at: now}}
	rt.spk.used = []pendingSpan{{start: 20.0, end: 26.0, res: diarize.Result{Speaker: "S2"}, at: now}}
	if pairs := rt.matchLocked(); len(pairs) != 0 {
		t.Fatalf("仅重叠 0.2 秒不该继承，实际 %+v", pairs)
	}
	// 窗口横跨两段（只沾到这一段的尾巴）：多半属于下一段，等下一段的判定结果
	rt.spk.segs[0] = pendingSeg{segID: "straddle", start: 23.8, end: 30.1, at: now}
	if pairs := rt.matchLocked(); len(pairs) != 0 {
		t.Fatalf("窗口大部分落在该段之外时不该继承，实际 %+v", pairs)
	}
	// 大部分落在这一段内：应当继承
	rt.spk.segs[0] = pendingSeg{segID: "inside", start: 22.0, end: 26.0, at: now}
	if pairs := rt.matchLocked(); len(pairs) != 1 || !pairs[0].inherited || pairs[0].res.Speaker != "S2" {
		t.Fatalf("重叠占大头时应继承，实际 %+v", pairs)
	}
}

func TestMatchLockedDoesNotInheritAcrossGap(t *testing.T) {
	now := nowSeconds()
	rt := &Runtime{}
	rt.spk.segs = []pendingSeg{{segID: "far", start: 100, end: 105, at: now}}
	// 已配过的段在 40 秒之前：既没有重叠、也超出继承窗口
	rt.spk.used = []pendingSpan{{start: 40, end: 50, res: diarize.Result{Speaker: "S9"}, at: now - 40}}
	if pairs := rt.matchLocked(); len(pairs) != 0 {
		t.Fatalf("时间无关的句子不应继承说话人，实际 %+v", pairs)
	}
}

func TestPruneLockedDropsExpired(t *testing.T) {
	now := nowSeconds()
	rt := &Runtime{}
	rt.spk.segs = []pendingSeg{
		{segID: "old", start: 0, end: 1, at: now - pendingTTL - 5},
		{segID: "new", start: 0, end: 1, at: now},
	}
	rt.spk.spans = []pendingSpan{{start: 0, end: 1, at: now - pendingTTL - 5}}
	pairs := rt.matchLocked()
	if len(pairs) != 0 {
		t.Fatalf("过老项不应参与配对，实际 %+v", pairs)
	}
	if len(rt.spk.segs) != 1 || rt.spk.segs[0].segID != "new" {
		t.Errorf("只应保留未过期句子，实际 %+v", rt.spk.segs)
	}
	if len(rt.spk.spans) != 0 {
		t.Errorf("过期语音段应被丢弃，实际 %+v", rt.spk.spans)
	}
	if rt.spk.expired.Load() != 2 {
		t.Errorf("过期计数应为 2，实际 %d", rt.spk.expired.Load())
	}
}

func TestSpeakerLabel(t *testing.T) {
	cases := map[string]string{
		"S1": "说话人 1", "S12": "说话人 12", "": "", "主持人": "主持人",
	}
	for in, want := range cases {
		if got := speakerLabel(in); got != want {
			t.Errorf("speakerLabel(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestSplitURLAndLoopback(t *testing.T) {
	host, port, err := splitURL("http://127.0.0.1:18901")
	if err != nil || host != "127.0.0.1" || port != "18901" {
		t.Fatalf("解析本地地址失败：%v %v %v", host, port, err)
	}
	if !isLoopback(host) {
		t.Error("127.0.0.1 应判定为本机")
	}
	// 远端地址不自动拉起 sidecar（不能在本机乱起进程）
	host2, _, _ := splitURL("http://10.0.0.5:18901")
	if isLoopback(host2) {
		t.Error("10.0.0.5 不应判定为本机")
	}
	if _, _, err := splitURL("://bad"); err == nil {
		t.Error("非法 URL 应报错")
	}
}
