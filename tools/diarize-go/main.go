// seat-diarize：CAM++ 说话人区分 sidecar 的 Go 实现（替代 tools/diarize 的 Python 版）。
//
// 主程序（seat）通过 DIARIZE_URL 调用本服务，HTTP 协议与 Python 版**完全一致**，
// 主程序无需任何改动：
//
//	GET  /health                                                  → 服务与模型状态
//	POST /assign?session=<sid>[&dir=<会话目录>][&threshold=0.6]   body=PCM(16k/mono/int16 LE)
//	     → {"speaker":"S1","label":"说话人 1","confidence":0.71,"margin":0.08,
//	        "is_new":false,"seconds":3.2,"speaker_count":2}
//	POST /reset?session=<sid>[&dir=<会话目录>]                     → 清空该场次说话人表
//	POST /roster?session=<sid>[&dir=<会话目录>]                    → 当前说话人名单
//
// 与 Python 版的行为差异：
//   - 默认聚类行为与 Python 版一致（中心按累计时长加权、全量漂移）；另有实验开关
//     --centroid-update-segs（只在前 N 段内更新中心、之后冻结）。离线实验曾显示
//     冻结能减少"链式吸收"（组内最差相似度 0.29→0.39），但真实重放中触发了新的
//     失效模式 —— 新建说话人的前几段若混了不同人，冻结出的"平均中心"会吸走更多段，
//     故默认关闭（-1），仅在明确验证过的场景下打开；
//   - 无 Python/无 PyTorch：模型推理经 sherpa-onnx（C API，cgo），二进制自包含。
//
// 构建 / 运行（先跑一次 fetch_deps.sh 拉取 sherpa-onnx 与模型）：
//
//	cd tools/diarize-go
//	./fetch_deps.sh
//	go build -o seat-diarize .
//	./seat-diarize --port 18901
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxBody = 8 << 20 // 单次音频上限 8MB（16k/mono/int16 ≈ 4 分钟）

// Service 服务状态。
type Service struct {
	embedder   *Embedder
	threshold  float64
	updateSegs int
	minMS      int
	stateDir   string
	saveAudio  bool

	mu       sync.Mutex
	sessions map[string]*SessionState
	seq      int // 音频留存的段序号

	startedAt time.Time
	calls     atomic.Int64
	errors    atomic.Int64
}

func (s *Service) session(sid, statePath string) *SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sessions[sid]
	if st == nil {
		st = NewSessionState(sid, s.threshold, s.updateSegs, statePath)
		s.sessions[sid] = st
	} else {
		// 主程序新建 session 时会先发一次 /reset（那时还没带会话目录），
		// 之后 /assign 才带 dir —— 补上路径，否则本场说话人表不会落盘。
		st.SetPathIfEmpty(statePath)
	}
	return st
}

// statePath 声纹状态落盘位置：优先主程序给的会话目录，否则用 --state-dir。
func (s *Service) statePath(sessDir string) string {
	base := sessDir
	if base == "" {
		base = s.stateDir
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "speakers.json")
}

func (s *Service) assign(sid, sessDir string, pcm []byte, thr float64) any {
	if len(pcm) < 2 {
		return map[string]any{"error": "空音频"}
	}
	seconds := float64(len(pcm)) / 2.0 / 16000.0
	if seconds*1000 < float64(s.minMS) {
		return map[string]any{"speaker": "", "reason": "too_short", "seconds": seconds}
	}
	if thr <= 0 {
		thr = s.threshold
	}
	st := s.session(sid, s.statePath(sessDir))
	st.SetThreshold(thr)

	emb, err := s.embedder.Embed(pcm)
	if err != nil {
		s.errors.Add(1)
		return map[string]any{"error": err.Error()}
	}
	res := st.Assign(emb, seconds)
	s.calls.Add(1)
	st.Save()
	if s.saveAudio {
		s.saveSegment(sessDir, pcm, res)
	}
	// 每次判定一行日志（与 Python 版一致）：现场排查"编号乱跳/标记缺失"的第一现场
	fmt.Printf("[diarize] %s %5.1fs → %s conf=%.4f new=%v 本场 %d 人\n",
		sid, seconds, res.Speaker, res.Confidence, res.IsNew, res.SpeakerCount)
	return res
}

// saveSegment 调试用：把每段音频与判定结果存到 <会话目录>/audio/（--save-audio）。
func (s *Service) saveSegment(sessDir string, pcm []byte, res AssignResult) {
	base := sessDir
	if base == "" {
		base = s.stateDir
	}
	if base == "" {
		return
	}
	outdir := filepath.Join(base, "audio")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		fmt.Printf("[diarize] 保存音频失败: %v\n", err)
		return
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	speaker := res.Speaker
	if speaker == "" {
		speaker = "none"
	}
	name := fmt.Sprintf("seg%04d_%s_conf%.4f.wav", seq, speaker, res.Confidence)
	if err := writeWAV(filepath.Join(outdir, name), pcm); err != nil {
		fmt.Printf("[diarize] 保存音频失败: %v\n", err)
		return
	}
	meta := map[string]any{
		"seq": seq, "file": name, "ts": float64(time.Now().UnixNano()) / 1e9,
		"seconds": res.Seconds, "speaker": res.Speaker, "confidence": res.Confidence,
		"margin": res.Margin, "is_new": res.IsNew, "speaker_count": res.SpeakerCount,
	}
	raw, _ := json.Marshal(meta)
	f, err := os.OpenFile(filepath.Join(outdir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// writeWAV 写 16k/mono/int16 WAV（标准 44 字节头）。
func writeWAV(path string, pcm []byte) error {
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)     // PCM
	binary.LittleEndian.PutUint16(hdr[22:], 1)     // mono
	binary.LittleEndian.PutUint32(hdr[24:], 16000) // sample rate
	binary.LittleEndian.PutUint32(hdr[28:], 32000) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:], 2)     // block align
	binary.LittleEndian.PutUint16(hdr[34:], 16)    // bits per sample
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	_, err = f.Write(pcm)
	return err
}

func writeJSON(w http.ResponseWriter, code int, obj any) {
	body, err := json.Marshal(obj)
	if err != nil {
		http.Error(w, `{"error":"marshal failed"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sessionCount := len(s.sessions)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"ok":                   true,
		"model":                s.embedder.Model(),
		"dim":                  s.embedder.Dim(),
		"threshold":            s.threshold,
		"update_segs":          s.updateSegs,
		"min_ms":               s.minMS,
		"save_audio":           s.saveAudio,
		"uptime_sec":           round2(time.Since(s.startedAt).Seconds()),
		"calls":                s.calls.Load(),
		"errors":               s.errors.Load(),
		"sessions":             sessionCount,
	})
}

func (s *Service) handleAssign(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := strings.TrimSpace(q.Get("session"))
	if sid == "" {
		sid = "default"
	}
	sessDir := strings.TrimSpace(q.Get("dir"))
	thr, _ := strconv.ParseFloat(q.Get("threshold"), 64)

	pcm, err := readBody(r)
	if err != nil {
		s.errors.Add(1)
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	res := s.assign(sid, sessDir, pcm, thr)
	if m, ok := res.(map[string]any); ok {
		if _, hasErr := m["error"]; hasErr {
			writeJSON(w, 400, res)
			return
		}
	}
	writeJSON(w, 200, res)
}

func (s *Service) handleReset(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := strings.TrimSpace(q.Get("session"))
	if sid == "" {
		sid = "default"
	}
	st := s.session(sid, s.statePath(strings.TrimSpace(q.Get("dir"))))
	st.Reset()
	writeJSON(w, 200, map[string]any{"ok": true, "session": sid})
}

func (s *Service) handleRoster(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := strings.TrimSpace(q.Get("session"))
	if sid == "" {
		sid = "default"
	}
	st := s.session(sid, s.statePath(strings.TrimSpace(q.Get("dir"))))
	writeJSON(w, 200, map[string]any{"ok": true, "speakers": st.Roster()})
}

func readBody(r *http.Request) ([]byte, error) {
	if r.ContentLength > maxBody {
		return nil, fmt.Errorf("请求体过大：%d 字节（上限 %d）", r.ContentLength, maxBody)
	}
	body := make([]byte, 0, 1<<16)
	buf := make([]byte, 1<<16)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
			if len(body) > maxBody {
				return nil, fmt.Errorf("请求体过大（上限 %d）", maxBody)
			}
		}
		if err != nil {
			break
		}
	}
	return body, nil
}

// ---- 参数与入口 ----

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func main() {
	host := flag.String("host", envStr("DIARIZE_HOST", "127.0.0.1"), "监听地址")
	port := flag.Int("port", envInt("DIARIZE_PORT", 18901), "监听端口")
	model := flag.String("model", envStr("DIARIZE_MODEL", "third_party/models/campplus.onnx"), "CAM++ ONNX 模型路径")
	threshold := flag.Float64("threshold", envFloat("DIARIZE_THRESHOLD", 0.6), "同一说话人判定阈值（余弦）")
	minMS := flag.Int("min-ms", envInt("DIARIZE_MIN_MS", 600), "过短片段不判定（毫秒）")
	updateSegs := flag.Int("centroid-update-segs", envInt("DIARIZE_CENTROID_UPDATE_SEGS", -1),
		"声纹中心更新策略：-1=全量漂移（默认，与 Python 版一致）；>0=只在前 N 段内更新、之后冻结（实验性）")
	stateDir := flag.String("state-dir", os.Getenv("DIARIZE_STATE_DIR"), "默认状态目录（主程序会给会话目录时优先用它）")
	saveAudio := flag.Bool("save-audio", envBool("DIARIZE_SAVE_AUDIO", false), "把每段音频与判定结果存到会话目录 audio/（调试用）")
	threads := flag.Int("threads", envInt("DIARIZE_THREADS", 2), "推理线程数")
	flag.Parse()

	emb, err := NewEmbedder(*model, *threads)
	if err != nil {
		log.Fatalf("[diarize] %v（先跑 ./fetch_deps.sh 拉取模型？）", err)
	}
	defer emb.Close()

	svc := &Service{
		embedder:   emb,
		threshold:  *threshold,
		updateSegs: *updateSegs,
		minMS:      *minMS,
		stateDir:   *stateDir,
		saveAudio:  *saveAudio,
		sessions:   map[string]*SessionState{},
		startedAt:  time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", svc.handleHealth)
	mux.HandleFunc("POST /assign", svc.handleAssign)
	mux.HandleFunc("POST /reset", svc.handleReset)
	mux.HandleFunc("POST /roster", svc.handleRoster)

	addr := fmt.Sprintf("%s:%d", *host, *port)
	saveTag := ""
	if *saveAudio {
		saveTag = "；音频留存=开"
	}
	log.Printf("[diarize] 监听 http://%s（模型 %s dim=%d 阈值 %.2f 中心更新段数 %d 最短 %dms%s）",
		addr, *model, emb.Dim(), *threshold, *updateSegs, *minMS, saveTag)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[diarize] 服务退出: %v", err)
	}
}
