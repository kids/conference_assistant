// Package config 读取 .env / 环境变量。
// 与 Python 版 app/config.py 的字段、默认值、派生属性逐项对齐，保证同一份 .env 可直接复用。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Settings 全部配置项。字段默认值与 Python 版一致。
type Settings struct {
	Host string
	Port int

	// ASR
	ASRProtocol string // hy_stream / funasr_nano / funasr / qwen3_http
	FunASRServ  string
	FunASRWSURL string
	ASRMode     string

	HyASRWSURL string
	HyASRToken string
	HyASRModel string

	Qwen3Backend       string
	Qwen3Model         string
	ASRPartialInterval float64

	ASRChunkSize    string
	ASRLanguage     string
	HotwordsPath    string
	LocalVADSegment bool

	// 顺句改写
	RefineEnabled  bool
	RefineMinChars int

	// LLM
	LLMBase        string
	LLMKey         string
	LLMModel       string
	LLMTimeout     float64
	LLMMaxTokens   int
	LLMTemperature float64
	LLMChatPath    string

	// 音频
	SampleRate        int
	FrameMS           int
	VADAggressiveness int
	VADSilenceMS      int
	MaxSegmentS       int

	// 数据与开关
	DataDir      string
	KeepAudio    bool
	WakewordAuto bool
	LogLevel     string

	BaseDir string // 解析相对路径的基准目录（.env / sessions / hotwords 所在处）
}

// Load 载入配置：先定位基准目录，再读取 .env（不覆盖已存在的真实环境变量，
// 与 pydantic-settings 的优先级一致），最后按默认值补齐。
func Load() *Settings {
	base := resolveBaseDir()
	// godotenv.Load 不覆盖已有环境变量；.env 不存在时静默跳过
	_ = godotenv.Load(filepath.Join(base, ".env"))

	s := &Settings{
		Host: envStr("HOST", "127.0.0.1"),
		Port: envInt("PORT", 8080),

		ASRProtocol: envStr("ASR_PROTOCOL", "funasr_nano"),
		FunASRServ:  envStr("FUNASR_SERV", "http://127.0.0.1:8002"),
		FunASRWSURL: envStr("FUNASR_WS_URL", ""),
		ASRMode:     envStr("ASR_MODE", "2pass"),

		HyASRWSURL: envStr("HY_ASR_WS_URL", "wss://asr.example.com/tencent/asr/recognize/stream"),
		HyASRToken: envStr("HY_ASR_TOKEN", "xxxxx"),
		HyASRModel: envStr("HY_ASR_MODEL", "HY-ASR-3-Stream"),

		Qwen3Backend:       envStr("QWEN3_BACKEND", "https://asr.example.com/s2"),
		Qwen3Model:         envStr("QWEN3_MODEL", "qwen3asr17b"),
		ASRPartialInterval: envFloat("ASR_PARTIAL_INTERVAL", 1.0),

		ASRChunkSize:    envStr("ASR_CHUNK_SIZE", "5,10,5"),
		ASRLanguage:     envStr("ASR_LANGUAGE", "中文"),
		HotwordsPath:    envStr("HOTWORDS_PATH", "hotwords_dict.txt"),
		LocalVADSegment: envBool("LOCAL_VAD_SEGMENT", true),

		RefineEnabled:  envBool("REFINE_ENABLED", true),
		RefineMinChars: envInt("REFINE_MIN_CHARS", 12),

		LLMBase:        envStr("LLM_BASE", ""),
		LLMKey:         envStr("LLM_KEY", ""),
		LLMModel:       envStr("LLM_MODEL", ""),
		LLMTimeout:     envFloat("LLM_TIMEOUT", 30.0),
		LLMMaxTokens:   envInt("LLM_MAX_TOKENS", 4000),
		LLMTemperature: envFloat("LLM_TEMPERATURE", 0.3),
		LLMChatPath:    envStr("LLM_CHAT_PATH", ""),

		SampleRate:        envInt("SAMPLE_RATE", 16000),
		FrameMS:           envInt("FRAME_MS", 20),
		VADAggressiveness: envInt("VAD_AGGRESSIVENESS", 2),
		VADSilenceMS:      envInt("VAD_SILENCE_MS", 600),
		MaxSegmentS:       envInt("MAX_SEGMENT_S", 15),

		DataDir:      envStr("DATA_DIR", "sessions"),
		KeepAudio:    envBool("KEEP_AUDIO", false),
		WakewordAuto: envBool("WAKEWORD_AUTO", false),
		LogLevel:     envStr("LOG_LEVEL", "info"),

		BaseDir: base,
	}
	return s
}

// resolveBaseDir 决定相对路径基准：
//  1. 显式 BASE_DIR 环境变量优先；
//  2. 当前目录有 .env → 用当前目录（容器内 WORKDIR /app）；
//  3. 上级目录有 .env → 用上级（仓库内 `cd go && ./seat`，与 Python 版共用配置）；
//  4. 否则用当前目录。
func resolveBaseDir() string {
	if v := strings.TrimSpace(os.Getenv("BASE_DIR")); v != "" {
		return v
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if fileExists(filepath.Join(cwd, ".env")) {
		return cwd
	}
	parent := filepath.Dir(cwd)
	if fileExists(filepath.Join(parent, ".env")) {
		return parent
	}
	return cwd
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- 派生属性（对齐 Python 版 @property）----

// HotwordsFile 全局热词词典路径（相对路径按 BaseDir 解析）。
func (s *Settings) HotwordsFile() string { return s.abs(s.HotwordsPath) }

// DataDirPath 运行数据目录。
func (s *Settings) DataDirPath() string { return s.abs(s.DataDir) }

// ChunkSize funasr 协议的 chunk_size 列表。
func (s *Settings) ChunkSize() []int {
	var out []int
	for _, part := range strings.Split(s.ASRChunkSize, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// WSURL 流式 ASR 端点：显式配置优先；funasr_nano 走网关默认；funasr 由 FunASRServ 推导。
func (s *Settings) WSURL() string {
	if s.ASRProtocol == "funasr_nano" && s.FunASRWSURL == "" {
		return "wss://asr.example.com/s2/ws"
	}
	if s.FunASRWSURL != "" {
		return s.FunASRWSURL
	}
	url := s.FunASRServ
	switch {
	case strings.HasPrefix(url, "https://"):
		url = "wss://" + strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		url = "ws://" + strings.TrimPrefix(url, "http://")
	}
	return strings.TrimRight(url, "/") + "/"
}

// FrameSamples 每帧采样点数（20ms @16k = 320）。
func (s *Settings) FrameSamples() int { return s.SampleRate * s.FrameMS / 1000 }

// BlockBytes 每帧字节数（int16 单声道）。
func (s *Settings) BlockBytes() int { return s.FrameSamples() * 2 }

func (s *Settings) abs(p string) string {
	if p == "" {
		return s.BaseDir
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.BaseDir, p)
}

// ---- 环境变量读取helper ----

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}
