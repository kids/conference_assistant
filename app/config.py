"""应用配置（环境变量 / .env 注入）。"""
from __future__ import annotations

from pathlib import Path

from pydantic_settings import BaseSettings, SettingsConfigDict

BASE_DIR = Path(__file__).resolve().parent.parent


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_file=BASE_DIR / ".env", env_file_encoding="utf-8", extra="ignore"
    )

    # 服务
    host: str = "127.0.0.1"
    port: int = 8080

    # FunASR 流式推理
    asr_protocol: str = "funasr_nano"  # funasr_nano / funasr / hy_stream（HY-ASR-3-Stream 逐词增量）
    funasr_serv: str = "http://127.0.0.1:8002"
    funasr_ws_url: str = ""  # 留空则从 funasr_serv 推导（funasr 协议）；nano 协议默认走 asr-gateway
    asr_mode: str = "2pass"  # funasr 协议专用：2pass / online / offline

    # hy_stream 协议（asr-gateway /tencent/asr/recognize/stream）：逐词增量 + 语义 VAD 断句
    hy_asr_ws_url: str = "wss://asr.example.com/tencent/asr/recognize/stream"
    hy_asr_token: str = "xxxxx"
    hy_asr_model: str = "HY-ASR-3-Stream"

    # qwen3_http 协议（vLLM /v1/audio/transcriptions）：客户端拼装流式（VAD 切句 + 周期重推草稿）
    qwen3_backend: str = "https://asr.example.com/s2"
    qwen3_model: str = "qwen3asr17b"
    asr_partial_interval: float = 1.0  # 草稿重推周期（秒音频），调小更跟手、请求更频
    # 单次推理超时（秒）。与 llm_timeout 独立 —— 服务端排队时同一段音频延迟在 0.8s~60s
    # 波动，超时过紧会整段丢字（表现：字幕时有时无）。超时/网络错误自动重试一次，4xx 不重试。
    asr_infer_timeout: float = 60.0
    asr_chunk_size: str = "5,10,5"
    asr_language: str = "中文"  # funasr_nano 协议语种
    hotwords_path: str = "hotwords_dict.txt"
    # funasr_nano：用本地 VAD 主动切句（句尾静音即 STOP flush），出字更快、粒度更细。
    # 关闭则退回服务端 VAD（实测 10~20s 才出一句）。
    local_vad_segment: bool = True

    # 转写终稿的 LLM 顺句改写（纠错别字/补标点/去赘词，异步更新显示）
    refine_enabled: bool = True
    refine_min_chars: int = 12

    # LLM（OpenAI 兼容 chat.completions）
    llm_base: str = ""
    llm_key: str = ""
    llm_model: str = ""
    llm_timeout: float = 30.0
    # 注意：taiji hy3 是思考模型，reasoning_content 与正文共享 max_tokens 预算，
    # 思考通常占 500~3000 字。预算过小会导致正文为空（校验报"过短(0<60)"）。
    llm_max_tokens: int = 4000
    llm_temperature: float = 0.3
    # chat 端点后缀：默认空 = llm_base 即完整端点（taiji）；标准 OpenAI 可填 "/v1/chat/completions"
    llm_chat_path: str = ""

    # 讲稿文档解析（控制台「解析讲稿」上传演示稿 → 抽热词 + 生成上下文摘要）
    # 默认值与 app/context/document.py 的常量一致
    doc_parse_url: str = "https://ml-serv.ssv.qq.com/parse_doc"
    # 摘要目标字数上限 2400：上下文组装时 STATIC 块会被截到 2500 字
    doc_digest_chars: int = 800
    # 上传体积预检上限。file 服务的 DefaultBodyLimit 已调到 256MB，
    # 这里取同一量级；该服务会把整个文件读进内存，完全放开有 OOM 风险。
    doc_max_bytes: int = 256 << 20

    # 音频
    sample_rate: int = 16000
    frame_ms: int = 20
    vad_aggressiveness: int = 2
    vad_silence_ms: int = 600
    max_segment_s: int = 15

    # 数据与开关
    data_dir: str = "sessions"
    keep_audio: bool = False
    wakeword_auto: bool = False
    log_level: str = "info"

    # ---- 派生属性 ----
    @property
    def hotwords_file(self) -> Path:
        p = Path(self.hotwords_path)
        return p if p.is_absolute() else BASE_DIR / p

    @property
    def data_dir_path(self) -> Path:
        p = Path(self.data_dir)
        return p if p.is_absolute() else BASE_DIR / p

    @property
    def chunk_size(self) -> list[int]:
        return [int(x) for x in self.asr_chunk_size.split(",") if x.strip()]

    @property
    def ws_url(self) -> str:
        # funasr_nano 协议默认端点（Kong 网关 /s2/ → 内部 /ws）
        if self.asr_protocol == "funasr_nano" and not self.funasr_ws_url:
            return "wss://asr.example.com/s2/ws"
        if self.funasr_ws_url:
            return self.funasr_ws_url
        url = self.funasr_serv
        if url.startswith("https://"):
            url = "wss://" + url[len("https://"):]
        elif url.startswith("http://"):
            url = "ws://" + url[len("http://"):]
        return url.rstrip("/") + "/"

    @property
    def frame_samples(self) -> int:
        """每帧采样点数（20ms @16k = 320 samples）。"""
        return self.sample_rate * self.frame_ms // 1000


_settings: Settings | None = None


def get_settings() -> Settings:
    global _settings
    if _settings is None:
        _settings = Settings()
    return _settings
