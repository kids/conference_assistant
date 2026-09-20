package main

import (
	"path/filepath"
	"testing"
)

func vec(vals ...float32) []float32 {
	v := append([]float32(nil), vals...)
	normalize(v)
	return v
}

func TestAssignBasic(t *testing.T) {
	st := NewSessionState("t", 0.6, 3, "")
	r1 := st.Assign(vec(1, 0, 0), 6)
	if r1.Speaker != "S1" || !r1.IsNew || r1.SpeakerCount != 1 {
		t.Fatalf("首段应新建 S1: %+v", r1)
	}
	r2 := st.Assign(vec(0.98, 0.02, 0), 6)
	if r2.Speaker != "S1" || r2.IsNew {
		t.Fatalf("同方向应归 S1: %+v", r2)
	}
	if r2.Confidence < 0.9 {
		t.Fatalf("同方向相似度应较高: %+v", r2)
	}
	r3 := st.Assign(vec(0, 1, 0), 6)
	if r3.Speaker != "S2" || !r3.IsNew || r3.SpeakerCount != 2 {
		t.Fatalf("正交方向应新建 S2: %+v", r3)
	}
}

// TestCentroidFreezeAfterN 验证"前 N 段平均后冻结中心"：
// updateSegs=2 时，第 3 段起中心不再更新（防止外来段把中心拉走）。
func TestCentroidFreezeAfterN(t *testing.T) {
	st := NewSessionState("t", 0.5, 2, "")
	st.Assign(vec(1, 0), 6)       // 新建 S1（Count 0→1）
	st.Assign(vec(0.95, 0.05), 6) // 并入；Count=1 < 2 → 更新中心（Count 1→2）
	before := append([]float32(nil), st.speakers[0].Centroid...)
	st.Assign(vec(0.9, 0.1), 6) // 并入；Count=2 不 < 2 → 中心应冻结
	after := st.speakers[0].Centroid
	if before[0] != after[0] || before[1] != after[1] {
		t.Fatalf("中心应冻结：%v -> %v", before, after)
	}
	if st.speakers[0].Count != 3 {
		t.Fatalf("计数应为 3: %d", st.speakers[0].Count)
	}
}

// TestStatePersistence 验证 speakers.json 落盘与恢复（编号续接、JSON 与 Python 版兼容）。
func TestStatePersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "speakers.json")
	st := NewSessionState("s1", 0.6, 3, p)
	st.Assign(vec(1, 0), 6)
	st.Save()

	st2 := NewSessionState("s1", 0.6, 3, p)
	if len(st2.speakers) != 1 || st2.speakers[0].ID != "S1" {
		t.Fatalf("恢复失败: %+v", st2.speakers)
	}
	r := st2.Assign(vec(0.99, 0.01), 6)
	if r.Speaker != "S1" || r.IsNew {
		t.Fatalf("恢复后编号应续接: %+v", r)
	}
}

// TestMaxSpeakers 超过上限后不再新建编号（归到最接近的一位）。
func TestMaxSpeakers(t *testing.T) {
	st := NewSessionState("t", 0.9, 3, "")
	// 造 12 个互相正交的方向（用 12 维空间）
	for i := 0; i < MaxSpeakers; i++ {
		v := make([]float32, MaxSpeakers)
		v[i] = 1
		st.Assign(v, 6)
	}
	if len(st.speakers) != MaxSpeakers {
		t.Fatalf("应建满 %d 人: %d", MaxSpeakers, len(st.speakers))
	}
	v := make([]float32, MaxSpeakers)
	for i := range v {
		v[i] = 0.1
	}
	r := st.Assign(v, 6)
	if r.IsNew || r.SpeakerCount != MaxSpeakers {
		t.Fatalf("超上限不应再新建: %+v", r)
	}
}
