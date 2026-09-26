package audio

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Recorder 会话录音留存（排查用）：把流水线的 16k/mono/int16 PCM 连续写成
// WAV 分片（默认每 5 分钟一片），配合 SweepRecFiles 按保留期清理。
//
// 为什么不用说话人区分的段音频（sessions/<sid>/audio/）：那是「判定用语音段」，
// 按 VAD/段长强切、不含静音，且依赖说话人区分开启；排查 ASR 问题需要连续、
// 完整（含静音上下文）的录音。
//
// 隐私：录音含会议内容，默认关闭（REC_SESSION_ENABLED=1 开启），保留期默认 7 天
// （REC_KEEP_DAYS，0=不清理）。写失败静默停用，绝不影响转写主链路。
type Recorder struct {
	dir        string
	segmentSec int

	mu       sync.Mutex
	file     *os.File
	dataLen  int64 // 当前分片已写音频字节数
	openedAt time.Time
	seq      int
	failed   bool
}

// NewRecorder 创建录音器并打开首个分片（目录自动创建）。
func NewRecorder(dir string, segmentSec int) (*Recorder, error) {
	if segmentSec <= 0 {
		segmentSec = 300
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &Recorder{dir: dir, segmentSec: segmentSec}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open 打开新分片并写入流式占位头（大小 0xFFFFFFFF，Close/滚动时回填）。
func (r *Recorder) open() error {
	r.seq++
	name := filepath.Join(r.dir,
		fmt.Sprintf("rec_%s_%03d.wav", time.Now().Format("20060102_150405"), r.seq))
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(wavHeader(0xFFFFFFFF)); err != nil {
		_ = f.Close()
		return err
	}
	r.file, r.dataLen, r.openedAt = f, 0, time.Now()
	return nil
}

// Write 追加一段 PCM；达到分片时长自动滚动到下一片。
// 在流水线 goroutine 内调用：只做内存追加与文件写，不做网络/锁等待。
func (r *Recorder) Write(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed || r.file == nil {
		return
	}
	// 先按时间边界滚动，再写这一帧：分片切点落在帧边界上，不把触发滚动的那帧写进旧片。
	if time.Since(r.openedAt) >= time.Duration(r.segmentSec)*time.Second {
		if err := r.closeFile(); err != nil {
			r.fail(err.Error())
			return
		}
		if err := r.open(); err != nil {
			r.fail("打开新分片失败: " + err.Error())
			return
		}
	}
	if _, err := r.file.Write(pcm); err != nil {
		r.fail("写入失败: " + err.Error())
		return
	}
	r.dataLen += int64(len(pcm))
}

// Close 收尾：回填 WAV 头并关闭当前分片。
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.closeFile(); err != nil {
		log.Printf("[rec] 关闭录音分片失败: %v", err)
	}
}

// closeFile 回填真实数据长度后关闭当前分片。
func (r *Recorder) closeFile() error {
	if r.file == nil {
		return nil
	}
	f := r.file
	r.file = nil
	if _, err := f.WriteAt(wavHeader(uint32(r.dataLen)), 0); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r *Recorder) fail(msg string) {
	log.Printf("[rec] 录音留存已停用（%s）", msg)
	r.failed = true
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}

// wavHeader 16k/mono/16bit 的 44 字节标准头；dataBytes 传 0xFFFFFFFF 表示流式占位。
func wavHeader(dataBytes uint32) []byte {
	buf := make([]byte, 44)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], dataBytes+36)
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt 块长度
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1)  // mono
	binary.LittleEndian.PutUint32(buf[24:28], 16000)
	binary.LittleEndian.PutUint32(buf[28:32], 32000)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], dataBytes)
	return buf
}

// SweepRecFiles 按保留期清理录音文件，返回删除数。清理范围：
//   - sessions/<sid>/rec/*.wav（本机制的会话录音分片）
//   - sessions/<sid>/audio/*.wav 与 audio/index.jsonl（说话人判定的段音频与元数据，
//     同属「排查用录音」）
//
// keepDays <= 0 表示不清理。判定依据是文件 mtime，删除失败静默跳过。
func SweepRecFiles(sessionsDir string, keepDays int) int {
	if keepDays <= 0 {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	removed := 0
	sids, err := os.ReadDir(sessionsDir)
	if err != nil {
		return 0
	}
	for _, e := range sids {
		if !e.IsDir() {
			continue
		}
		for _, sub := range []string{"rec", "audio"} {
			dir := filepath.Join(sessionsDir, e.Name(), sub)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range entries {
				if f.IsDir() {
					continue
				}
				name := f.Name()
				if !strings.HasSuffix(name, ".wav") && name != "index.jsonl" {
					continue
				}
				fi, err := f.Info()
				if err != nil {
					continue
				}
				if fi.ModTime().Before(cutoff) {
					if os.Remove(filepath.Join(dir, name)) == nil {
						removed++
					}
				}
			}
		}
	}
	return removed
}
