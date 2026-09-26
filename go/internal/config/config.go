// Package config 读取 .env / 环境变量。
// 与 Python 版 app/config.py 的字段、默认值、派生属性逐项对齐，保证同一份 .env 可直接复用。
package config

import (
	"log"
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

	Qwen3Backend string
	Qwen3Model   string
	// Qwen3Token qwen3asr 服务端鉴权密钥（Authorization: Bearer）。服务端未开 --api-key
	// 时留空即可（客户端不发该头）。.env.example 里一直有这项，但此前 Go 版未接通 ——
	// 属于「配置写了却不生效」，与「写死」同样是配置失真，现已真正生效。
	Qwen3Token string
	// Qwen3WarmupMS qwen3_http 启动冷静期（毫秒）：丢弃采集启动瞬态的「类语音」环境声
	// （浏览器 AGC 冲激等，实测会诱发百科式幻觉，如"《小王子》是…"；与电平无关）。
	// 检出真实说话起始会自动提前结束；0=关闭。
	Qwen3WarmupMS      int
	ASRPartialInterval float64
	// ASRInferTimeout qwen3_http 单次推理超时（秒）。
	// 与 LLMTimeout 分开：LLM 生成与 ASR 转写的合理超时不同，且服务端偶发排队时
	// ASR 需要更宽的容忍度（实测同一段音频延迟在 0.8s~60s 间波动）。
	ASRInferTimeout float64

	ASRChunkSize string
	// ASRLanguage ASR 语种（qwen3_http / funasr_nano）：auto/自动 = 不指定语种，
	// 由模型逐句自动检测（默认）；也可强制 中文/英文/日语/韩语 或 ISO 码。
	// 默认 auto 的原因：LLM 式 ASR 被强指定语种时，另一语种的报告会被硬解码成
	// 该语种 —— 英文报告「空耳」成中文、个别句子被翻译成中文；中文语音在 auto 下
	// 同样能正确识别。运行期可在控制台顶部「语种」下拉框切换（见 Runtime.SetASRLanguage）。
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
	// LLMReasoningEffort 思考开关（OpenAI 兼容字段 reasoning_effort）：空=不发送、
	// 用模型默认行为（思考模型会先思考再出正文）；"no_think"=关闭思考（hy 系列）。
	// 思考阶段只流 reasoning_content 且客户端会丢弃，首字要等 10~30s；关掉后
	// 正文 delta 直接开始流。
	LLMReasoningEffort string

	// 讲稿文档解析（上传演示稿 → 抽热词 + 生成上下文摘要）
	DocParseURL    string // /parse_doc 服务地址
	DocDigestChars int    // 浓缩摘要目标字数（进 AI 上下文的版本）
	DocMaxBytes    int    // 上传体积预检上限（受网关 client_max_body_size 制约）
	// DocParseTimeout 单次解析的超时（秒）。解析耗时随体积线性增长：实测 17MB→17s、
	// 22MB→31s，120s 余量偏紧（网络慢或服务端排队时会误报「不可达」）。
	DocParseTimeout float64

	// 说话人区分（CAM++ sidecar）
	DiarizeEnabled   bool
	DiarizeURL       string
	DiarizeTimeout   float64
	DiarizeThreshold float64
	DiarizeMinMS     int
	// DiarizeMaxSegS 说话人区分的音频段最长秒数（与 ASR 的 MAX_SEGMENT_S 分开）。
	// 段内混入多人声音时声纹 embedding 变成混合体，聚类会把不同人并成一个编号；
	// 多人接话的会议里 15s 强切段几乎必然混合，5~8s 是较好的折中。
	DiarizeMaxSegS int
	// DiarizeAutostart 启动时自动拉起 sidecar（tools/diarize/run.sh，首次运行会装依赖+下模型）。
	// 仅在 DiarizeURL 指向本机、且脚本存在时生效；起不来只记日志，不影响转写。
	DiarizeAutostart bool
	// DiarizeDebug 打印「每段语音的判定结果」与「句子-语音段的配对决策」。
	// 现场排查「编号乱跳 / 标记缺失」时打开：能直接看到某句话被配给了哪一段音频、为什么。
	DiarizeDebug bool
	// DiarizeSaveSegmentAudio 把「说话人判定用的语音段」及其判定结果（index.jsonl，含
	// embedding）落盘到 sessions/<sid>/audio/（调试用：离线重算相似度矩阵、定量验证阈值）。
	// 与 RecSessionEnabled 的区别：那是连续完整录音（含静音），这里是按 VAD/段长强切出的段。
	// 默认关闭。此前它只存在于 sidecar 自己的环境变量里、主程序从不透传（.env 里写了无效），
	// 现改为真正的配置项并由主程序传给 sidecar。
	DiarizeSaveSegmentAudio bool

	// 会话录音留存（排查用）：把每场会议的原始音频**连续**写成 WAV 分片
	// （sessions/<sid>/rec/），含静音上下文，用于事后回放「当时收音到底什么样」。
	// 与 DiarizeSaveSegmentAudio（只存「判定用的语音段」）是两种不同材料，命名上
	// 用 SESSION / SEGMENT 区分。录音含会议内容，默认关闭；超期自动清理（RecKeepDays）。
	RecSessionEnabled bool
	RecChunkSec       int // 分片时长（秒）—— 文件分片，不是说话人判定段
	RecKeepDays       int // 保留天数；0=不自动清理

	// 发言（按立场生成发言稿）
	// SpeechMaxTokens 「发言」单次生成预算。hy3 的思考与正文共享该预算，
	// 2 分钟发言稿正文可达 600 字，预算过小会出现「正文为空」。
	SpeechMaxTokens int

	// 音频
	SampleRate        int
	FrameMS           int
	VADAggressiveness int
	VADSilenceMS      int
	MaxSegmentS       int

	// 数据与开关
	DataDir string
	// WakewordAuto 预留：唤醒词自动触发（当前无实现）
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
		Qwen3Token:         envStr("QWEN3_TOKEN", ""),
		Qwen3WarmupMS:      envInt("QWEN3_WARMUP_MS", 3000),
		ASRPartialInterval: envFloat("ASR_PARTIAL_INTERVAL", 1.0),
		ASRInferTimeout:    envFloat("ASR_INFER_TIMEOUT", 60.0),

		ASRChunkSize:    envStr("ASR_CHUNK_SIZE", "5,10,5"),
		ASRLanguage:     envStr("ASR_LANGUAGE", "auto"),
		HotwordsPath:    envStr("HOTWORDS_PATH", "hotwords_dict.txt"),
		LocalVADSegment: envBool("LOCAL_VAD_SEGMENT", true),

		RefineEnabled:  envBool("REFINE_ENABLED", true),
		RefineMinChars: envInt("REFINE_MIN_CHARS", 12),

		LLMBase:            envStr("LLM_BASE", ""),
		LLMKey:             envStr("LLM_KEY", ""),
		LLMModel:           envStr("LLM_MODEL", ""),
		LLMTimeout:         envFloat("LLM_TIMEOUT", 30.0),
		LLMMaxTokens:       envInt("LLM_MAX_TOKENS", 4000),
		LLMTemperature:     envFloat("LLM_TEMPERATURE", 0.3),
		LLMChatPath:        envStr("LLM_CHAT_PATH", ""),
		LLMReasoningEffort: envStr("LLM_REASONING_EFFORT", ""),

		// 默认值与 contextx 包内的常量一致（此处写字面量以保持 config 不反向依赖业务包）
		DocParseURL:    envStr("DOC_PARSE_URL", "https://ml-serv.ssv.qq.com/parse_doc"),
		DocDigestChars: envInt("DOC_DIGEST_CHARS", 800),
		// file 服务的 DefaultBodyLimit 已调到 256MB，这里取同一量级
		DocMaxBytes: envInt("DOC_MAX_BYTES", 256<<20),
		// 解析超时：给大文件与慢链路留余量（原硬编码 120s，22MB 已耗 31s，余量偏紧）
		DocParseTimeout: envFloat("DOC_PARSE_TIMEOUT", 180.0),

		// 说话人区分：默认关闭（需要 Python 环境跑 CAM++ sidecar），见 .env.example 说明
		DiarizeEnabled:   envBool("DIARIZE_ENABLED", false),
		DiarizeURL:       envStr("DIARIZE_URL", "http://127.0.0.1:18901"),
		DiarizeTimeout:   envFloat("DIARIZE_TIMEOUT", 20.0),
		DiarizeThreshold: envFloat("DIARIZE_THRESHOLD", 0.5),
		DiarizeMinMS:     envInt("DIARIZE_MIN_MS", 600),
		DiarizeMaxSegS:   envInt("DIARIZE_MAX_SEG_S", 6),
		DiarizeAutostart: envBool("DIARIZE_AUTOSTART", true),
		DiarizeDebug:     envBool("DIARIZE_DEBUG", false),
		// 旧名 DIARIZE_SAVE_AUDIO 兼容（此前主程序并不读它，只有 sidecar 自己看环境变量）
		DiarizeSaveSegmentAudio: envBoolRenamed("DIARIZE_SAVE_SEGMENT_AUDIO", "DIARIZE_SAVE_AUDIO", false),

		RecSessionEnabled: envBoolRenamed("REC_SESSION_ENABLED", "REC_ENABLED", false),
		RecChunkSec:       envIntRenamed("REC_CHUNK_SEC", "REC_SEGMENT_SEC", 300),
		RecKeepDays:       envInt("REC_KEEP_DAYS", 7),

		SpeechMaxTokens: envInt("SPEECH_MAX_TOKENS", 6000),

		SampleRate:        envInt("SAMPLE_RATE", 16000),
		FrameMS:           envInt("FRAME_MS", 20),
		VADAggressiveness: envInt("VAD_AGGRESSIVENESS", 2),
		VADSilenceMS:      envInt("VAD_SILENCE_MS", 600),
		MaxSegmentS:       envInt("MAX_SEGMENT_S", 15),

		DataDir:      envStr("DATA_DIR", "sessions"),
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
		return parseBool(v, def)
	}
	return def
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// ---- 改名兼容层 ----
// env*Renamed 优先读新键名，旧键名仍兼容（命中旧名时提示改名）。
// 存在的意义：环境变量改名后，旧的 .env / 容器 ENV 不会报错，只会静默回落到默认值 ——
// 「功能突然不生效了」最难查。宁可打一行日志让人看见，也不要悄悄降级。

func envBoolRenamed(newKey, oldKey string, def bool) bool {
	if v, ok := os.LookupEnv(newKey); ok && strings.TrimSpace(v) != "" {
		return parseBool(v, def)
	}
	if v, ok := os.LookupEnv(oldKey); ok && strings.TrimSpace(v) != "" {
		log.Printf("[config] %s 已更名为 %s（当前仍在用旧名，请更新 .env）", oldKey, newKey)
		return parseBool(v, def)
	}
	return def
}

func envIntRenamed(newKey, oldKey string, def int) int {
	if v, ok := os.LookupEnv(newKey); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	if v, ok := os.LookupEnv(oldKey); ok && strings.TrimSpace(v) != "" {
		log.Printf("[config] %s 已更名为 %s（当前仍在用旧名，请更新 .env）", oldKey, newKey)
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}
