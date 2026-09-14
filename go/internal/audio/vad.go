package audio

import (
	"fmt"

	"github.com/maxhawkins/go-webrtcvad"
)

// VadSegmenter 基于 webrtcvad 的断句器：语音起止 + 句尾静音 + 超长强切。
// 与 Python 版 app/audio/vad.py 使用同一个 webrtcvad C 库，阈值语义完全一致。
type VadSegmenter struct {
	vad        *webrtcvad.VAD
	sampleRate int
	frameMS   int

	silenceThresh int // 句尾静音帧数
	startThresh   int // 起句连续语音帧数
	maxFrames     int // 超长强切

	inSpeech  bool
	silentRun int
	speechRun int
	segFrames int
}

// NewVadSegmenter 创建断句器。frameMS 仅支持 10/20/30（webrtcvad 限制）。
func NewVadSegmenter(sampleRate, frameMS, silenceMS, aggressiveness, maxSegmentS int) (*VadSegmenter, error) {
	if frameMS != 10 && frameMS != 20 && frameMS != 30 {
		return nil, fmt.Errorf("frame_ms 仅支持 10/20/30，当前 %d", frameMS)
	}
	if aggressiveness < 0 || aggressiveness > 3 {
		aggressiveness = 2
	}
	v, err := webrtcvad.New()
	if err != nil {
		return nil, fmt.Errorf("初始化 webrtcvad 失败: %w", err)
	}
	if err := v.SetMode(aggressiveness); err != nil {
		return nil, fmt.Errorf("设置 VAD 模式失败: %w", err)
	}

	silenceThresh := silenceMS / frameMS
	if silenceThresh < 1 {
		silenceThresh = 1
	}
	return &VadSegmenter{
		vad:           v,
		sampleRate:    sampleRate,
		frameMS:       frameMS,
		silenceThresh: silenceThresh,
		startThresh:   3,
		maxFrames:     maxSegmentS * 1000 / frameMS,
	}, nil
}

// Event 断句事件。
type Event int

const (
	EventNone Event = iota
	EventStart
	EventEnd
)

// Process 处理一帧，返回断句事件。
func (s *VadSegmenter) Process(frame []byte) Event {
	voice := s.isVoice(frame)
	if !s.inSpeech {
		if voice {
			s.speechRun++
			if s.speechRun >= s.startThresh {
				s.inSpeech = true
				s.silentRun = 0
				s.segFrames = s.speechRun
				return EventStart
			}
		} else {
			s.speechRun = 0
		}
		return EventNone
	}

	s.segFrames++
	if voice {
		s.silentRun = 0
	} else {
		s.silentRun++
	}
	if s.silentRun >= s.silenceThresh || s.segFrames >= s.maxFrames {
		s.inSpeech = false
		s.silentRun = 0
		s.speechRun = 0
		return EventEnd
	}
	return EventNone
}

// Reset 重置状态。
func (s *VadSegmenter) Reset() {
	s.inSpeech = false
	s.silentRun = 0
	s.speechRun = 0
	s.segFrames = 0
}

// isVoice 静音/异常帧视为非语音（与 Python 版 try/except 行为一致）。
func (s *VadSegmenter) isVoice(frame []byte) bool {
	ok, err := s.vad.Process(s.sampleRate, frame)
	if err != nil {
		return false
	}
	return ok
}
