// 说话人聚类的核心状态与增量分配逻辑（无外部依赖，可单测）。
//
// 与 Python sidecar（tools/diarize/server.py）行为对齐，并带上现场音频验证过的改进：
// 中心只在「前 N 段」内更新、之后冻结。全量漂移会让"勉强过线"（阈值边缘）的外来段
// 慢慢把中心拉走 —— 链式吸收，最终多个不同的人并进一个编号（实测组内最差相似度
// 只有 0.29）；前几段平均后冻结把该指标提升到 0.39，且不再受后续噪声段影响。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxSpeakers 单场会议最多区分多少个说话人（超出则归到最接近的一位）。
const MaxSpeakers = 12

// Speaker 一位说话人在本场会议中的声纹中心与统计。
// JSON 字段与 Python sidecar 的 speakers.json 完全兼容（切换实现时历史场次编号不丢）。
type Speaker struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Count    int       `json:"count"`
	Seconds  float64   `json:"seconds"`
	FirstAt  float64   `json:"first_at"`
	LastAt   float64   `json:"last_at"`
	Centroid []float32 `json:"centroid"`
}

// AssignResult 一次判定的结果（HTTP 响应体）。
type AssignResult struct {
	Speaker      string  `json:"speaker"`
	Label        string  `json:"label"`
	Confidence   float64 `json:"confidence"`
	Margin       float64 `json:"margin"`
	IsNew        bool    `json:"is_new"`
	Seconds      float64 `json:"seconds"`
	SpeakerCount int     `json:"speaker_count"`
}

// SessionState 一个 session 的说话人表（含持久化）。
type SessionState struct {
	mu         sync.Mutex
	sid        string
	threshold  float64
	updateSegs int // 中心最多参与更新的段数；<0 = 无限漂移（旧行为）
	path       string
	speakers   []*Speaker
	dirty      bool
}

type stateFile struct {
	Session   string     `json:"session"`
	Threshold float64    `json:"threshold"`
	SavedAt   float64    `json:"saved_at"`
	Speakers  []*Speaker `json:"speakers"`
}

// NewSessionState 创建并尝试从 statePath 恢复（同 session 编号续接）。
func NewSessionState(sid string, threshold float64, updateSegs int, statePath string) *SessionState {
	st := &SessionState{sid: sid, threshold: threshold, updateSegs: updateSegs, path: statePath}
	st.load()
	return st
}

func (st *SessionState) load() {
	if st.path == "" {
		return
	}
	raw, err := os.ReadFile(st.path)
	if err != nil {
		return // 首次运行没有该文件：正常
	}
	var obj stateFile
	if err := json.Unmarshal(raw, &obj); err != nil {
		fmt.Printf("[diarize] 读取 %s 失败（忽略）: %v\n", st.path, err)
		return
	}
	st.speakers = obj.Speakers
}

// Save 落盘（仅在有变更时）。失败不影响判定主链路。
func (st *SessionState) Save() {
	if st.path == "" || !st.dirty {
		return
	}
	obj := stateFile{Session: st.sid, Threshold: st.threshold, SavedAt: nowSec(), Speakers: st.speakers}
	raw, err := json.MarshalIndent(obj, "", " ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		fmt.Printf("[diarize] 创建目录失败: %v\n", err)
		return
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		fmt.Printf("[diarize] 写 %s 失败（忽略）: %v\n", st.path, err)
		return
	}
	if err := os.Rename(tmp, st.path); err != nil {
		fmt.Printf("[diarize] 替换 %s 失败（忽略）: %v\n", st.path, err)
		return
	}
	st.dirty = false
}

// Assign 为一段音频的声纹向量分配说话人编号。emb 必须是单位向量。
func (st *SessionState) Assign(emb []float32, seconds float64) AssignResult {
	now := nowSec()
	st.mu.Lock()
	defer st.mu.Unlock()

	best, bestSim, secondSim := -1, -1.0, -1.0
	for i, sp := range st.speakers {
		sim := dot(sp.Centroid, emb)
		if sim > bestSim {
			best, secondSim, bestSim = i, bestSim, sim
		} else if sim > secondSim {
			secondSim = sim
		}
	}

	var sp *Speaker
	isNew := false
	switch {
	case best >= 0 && bestSim >= st.threshold:
		sp = st.speakers[best]
		// 中心更新策略：updateSegs<0（默认）＝时长加权全量漂移，与 Python 版一致；
		// >0＝只在前 N 段内更新、之后冻结（实验开关：离线音频上能减少"链式吸收"，
		// 但真实重放中触发过新失效模式 —— 新建说话人前几段混人时，冻结出的
		// "平均中心"会吸走更多段，故不默认开启）。
		if st.updateSegs < 0 || sp.Count < st.updateSegs {
			w := math.Max(0.2, seconds)
			newC := make([]float32, len(sp.Centroid))
			for i := range newC {
				newC[i] = sp.Centroid[i]*float32(sp.Seconds) + emb[i]*float32(w)
			}
			normalize(newC)
			sp.Centroid = newC
		}
	case len(st.speakers) >= MaxSpeakers:
		// 已到上限：归到最接近的一位，避免编号无限增长
		if best >= 0 {
			sp = st.speakers[best]
		} else {
			sp = st.newSpeaker(emb, now)
		}
	default:
		sp = st.newSpeaker(emb, now)
		isNew = true
		bestSim = dot(sp.Centroid, emb)
	}

	sp.Count++
	sp.Seconds += seconds
	sp.LastAt = now
	st.dirty = true

	return AssignResult{
		Speaker:      sp.ID,
		Label:        sp.Label,
		Confidence:   round4(bestSim),
		Margin:       round4(bestSim - math.Max(secondSim, 0)),
		IsNew:        isNew,
		Seconds:      round2(seconds),
		SpeakerCount: len(st.speakers),
	}
}

func (st *SessionState) newSpeaker(emb []float32, now float64) *Speaker {
	cp := make([]float32, len(emb))
	copy(cp, emb)
	sp := &Speaker{
		ID:       fmt.Sprintf("S%d", len(st.speakers)+1),
		Label:    fmt.Sprintf("说话人 %d", len(st.speakers)+1),
		Centroid: cp,
		FirstAt:  now,
		LastAt:   now,
	}
	st.speakers = append(st.speakers, sp)
	return sp
}

// SetThreshold 更新本 session 的判定阈值（主程序每次请求都会带上，可能现场调参）。
func (st *SessionState) SetThreshold(t float64) {
	if t <= 0 {
		return
	}
	st.mu.Lock()
	st.threshold = t
	st.mu.Unlock()
}

// SetPathIfEmpty 补上落盘路径（主程序先发 /reset 再发 /assign 时才拿得到会话目录；
// 不补的话本场说话人表永远不会落盘 —— 对齐 Python 版的同类处理）。
func (st *SessionState) SetPathIfEmpty(p string) {
	if p == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.path == "" {
		st.path = p
		st.dirty = true
	}
}

// Reset 清空某 session 的说话人表（新建 session 时调用）。
func (st *SessionState) Reset() {
	st.mu.Lock()
	st.speakers = nil
	st.dirty = true
	st.mu.Unlock()
	if st.path != "" {
		_ = os.Remove(st.path)
	}
}

// Roster 当前说话人名单。
type RosterItem struct {
	Speaker string  `json:"speaker"`
	Label   string  `json:"label"`
	Count   int     `json:"count"`
	Seconds float64 `json:"seconds"`
}

// Roster 名单快照。
func (st *SessionState) Roster() []RosterItem {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]RosterItem, 0, len(st.speakers))
	for _, s := range st.speakers {
		out = append(out, RosterItem{Speaker: s.ID, Label: s.Label, Count: s.Count, Seconds: round2(s.Seconds)})
	}
	return out
}

// nowSec 当前 Unix 时间（秒，含小数）。
func nowSec() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ---- 向量工具 ----

// normalize L2 归一化（原地）。
func normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n) + 1e-9
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
}

// dot 两个单位向量的点积（= 余弦相似度）。
func dot(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float64
	for i := 0; i < n; i++ {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
