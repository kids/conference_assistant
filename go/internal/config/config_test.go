package config

import "testing"

// clearRecKeys 清空与「录音落盘」相关的环境变量，保证默认值断言不受外部 env 影响。
// 置为空串等价于未设置（读取侧对空串一律回落默认值）。
func clearRecKeys(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"REC_SESSION_ENABLED", "REC_ENABLED",
		"REC_CHUNK_SEC", "REC_SEGMENT_SEC",
		"DIARIZE_SAVE_SEGMENT_AUDIO", "DIARIZE_SAVE_AUDIO",
	} {
		t.Setenv(k, "")
	}
}

// TestAudioSwitchDefaults：三个音频落盘开关都有默认值，且默认是「不落盘」——
// 什么都不配也能正常启动，且不写任何含会议内容的音频。
func TestAudioSwitchDefaults(t *testing.T) {
	clearRecKeys(t)
	s := Load()
	if s.RecSessionEnabled {
		t.Error("REC_SESSION_ENABLED 默认应为 false（不录整场）")
	}
	if s.RecChunkSec != 300 {
		t.Errorf("REC_CHUNK_SEC 默认应为 300，实际 %d", s.RecChunkSec)
	}
	if s.DiarizeSaveSegmentAudio {
		t.Error("DIARIZE_SAVE_SEGMENT_AUDIO 默认应为 false（不存判定段）")
	}
	if s.DiarizeEnabled {
		t.Error("说话人区分默认应为 false（需要 sidecar）")
	}
}

// TestRenamedEnvCompat：改名后的新键优先；旧键仍兼容（命中旧名时打日志提示），
// 避免「代码改名 + 部署 .env 没改名」导致功能静默失效。
func TestRenamedEnvCompat(t *testing.T) {
	clearRecKeys(t)
	t.Setenv("REC_ENABLED", "true")
	t.Setenv("REC_SEGMENT_SEC", "120")
	t.Setenv("DIARIZE_SAVE_AUDIO", "1")
	s := Load()
	if !s.RecSessionEnabled || s.RecChunkSec != 120 || !s.DiarizeSaveSegmentAudio {
		t.Fatalf("旧键名应继续生效：rec=%v chunk=%d save=%v",
			s.RecSessionEnabled, s.RecChunkSec, s.DiarizeSaveSegmentAudio)
	}

	t.Setenv("REC_SESSION_ENABLED", "false")
	t.Setenv("REC_CHUNK_SEC", "600")
	t.Setenv("DIARIZE_SAVE_SEGMENT_AUDIO", "false")
	s = Load()
	if s.RecSessionEnabled || s.RecChunkSec != 600 || s.DiarizeSaveSegmentAudio {
		t.Fatalf("新键名应优先于旧键名：rec=%v chunk=%d save=%v",
			s.RecSessionEnabled, s.RecChunkSec, s.DiarizeSaveSegmentAudio)
	}
}
