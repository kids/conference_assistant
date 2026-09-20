package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecorderRollsOnSegment 分片滚动：超过分片时长应自动开新文件，
// 且旧分片的 WAV 头被回填成真实长度。
func TestRecorderRollsOnSegment(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, 300)
	if err != nil {
		t.Fatalf("创建录音器失败: %v", err)
	}
	defer r.Close()

	pcm := make([]byte, 32000) // 1 秒 16k/mono/int16
	r.Write(pcm)
	// 手动把打开时间拨早，触发滚动（避免测试真等待）
	r.mu.Lock()
	r.openedAt = time.Now().Add(-time.Hour)
	r.mu.Unlock()
	r.Write(pcm) // 这一写发现已超时 → 关旧片、开新片、写新片

	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	if len(files) != 2 {
		t.Fatalf("应滚动出 2 个分片，实际 %d 个: %v", len(files), files)
	}
	// 第一个分片应恰好 44+32000 字节（回填后的头 + 1 秒音频）
	var first string
	for _, f := range files {
		if fi, _ := os.Stat(f); fi.Size() == 44+32000 {
			first = f
		}
	}
	if first == "" {
		t.Error("未找到大小为 44+32000 的已回填分片")
	}
}

// TestRecorderWavHeader 收尾回填：Close 后 WAV 头中的 RIFF/data 长度必须是真实值，
// 否则播放器读到的是流式占位（0xFFFFFFFF）。
func TestRecorderWavHeader(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, 300)
	if err != nil {
		t.Fatalf("创建录音器失败: %v", err)
	}
	r.Write(make([]byte, 1000))
	r.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	if len(files) != 1 {
		t.Fatalf("应有 1 个分片，实际 %d", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(raw) != 44+1000 {
		t.Fatalf("文件长度应为 1044，实际 %d", len(raw))
	}
	if got := binary.LittleEndian.Uint32(raw[4:8]); got != 36+1000 {
		t.Errorf("RIFF 长度应为 %d，实际 %d", 36+1000, got)
	}
	if got := binary.LittleEndian.Uint32(raw[40:44]); got != 1000 {
		t.Errorf("data 长度应为 1000，实际 %d", got)
	}
	if string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Error("WAV 魔数不正确")
	}
}

// TestSweepRecFiles 保留期清理：过期文件（含 audio/index.jsonl）删除、未过期保留。
func TestSweepRecFiles(t *testing.T) {
	sessions := t.TempDir()
	old := time.Now().AddDate(0, 0, -8)
	fresh := time.Now()

	mk := func(rel string, mt time.Time) string {
		p := filepath.Join(sessions, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldRec := mk("s1/rec/rec_old.wav", old)
	newRec := mk("s1/rec/rec_new.wav", fresh)
	oldSeg := mk("s2/audio/seg0001_S1_conf0.9.wav", old)
	idx := mk("s2/audio/index.jsonl", old) // 会与同日段音频一起过期
	oldSeg2 := mk("s3/audio/seg0001.wav", old)
	idx2 := mk("s3/audio/index.jsonl", fresh) // 近期还有段写入时应保留

	if n := SweepRecFiles(sessions, 7); n != 4 { // oldRec + oldSeg + idx + oldSeg2
		t.Errorf("应删除 4 个过期文件，实际 %d", n)
	}
	for _, p := range []string{oldRec, oldSeg, idx, oldSeg2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("过期文件未删除: %s", p)
		}
	}
	for _, p := range []string{newRec, idx2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("未过期文件被误删: %s", p)
		}
	}
	// keepDays<=0：不清理
	if n := SweepRecFiles(sessions, 0); n != 0 {
		t.Errorf("keepDays=0 不应删除，实际 %d", n)
	}
}
