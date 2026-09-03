# AI 跨学科实时翻译席

本地执行的 Web 应用：会议现场投屏。实时听取报告与问答（FunASR 流式 2pass），主持人口令后由大模型生成跨学科短翻译/桥接问题，投屏展示（本版无 TTS）。

对应需求文档：《Workshop_AI跨学科实时翻译席技术方案》；架构设计见 `../架构设计_AI跨学科实时翻译席.md`。

## 快速开始

```bash
cd translation_seat
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
# 编辑 .env：填写 LLM_BASE / LLM_KEY / LLM_MODEL；按需调整 FUNASR_SERV / FUNASR_WS_URL

./run.sh          # 或 uvicorn app.main:app --host 127.0.0.1 --port 8080
```

- 操作员控制台：<http://127.0.0.1:8080/console>
- 会场投屏大屏：<http://127.0.0.1:8080/screen>（在扩展屏全屏打开）

## 远端部署与浏览器收音

部署在服务器（无本地麦克风）时，可用**浏览器收音**：任意机器上的浏览器采集 16k PCM，经 WebSocket 推给服务进入 ASR 流水线。`run.sh` 默认绑定所有网卡（`HOST=0.0.0.0`），远端浏览器可直接访问；仅本机使用：`HOST=127.0.0.1 ./run.sh`。

1. 操作员在任意机器打开 `http://<服务器IP>:8081/console`，勾选**「浏览器收音」**，点「开始 Session」
2. 点**「开始收音」**并授权麦克风——底部状态条显示“采集中（浏览器）”
3. 服务器无音频设备时，本地麦克风采集失败会自动降级为浏览器收音（无需手动设置）

> 浏览器麦克风权限要求**安全上下文**：`localhost`/`127.0.0.1` 或 HTTPS 可直接使用；远端 HTTP 访问需在浏览器开启“将不安全来源视为安全”
> （Chrome：`chrome://flags/#unsafely-treat-insecure-origin-as-secure` 中添加地址并重启）。

## 环境检查

```bash
python tools/check_env.py
```

## 关键配置（.env）

| 变量 | 说明 |
|---|---|
| `ASR_PROTOCOL` | `funasr_nano`（默认，Fun-ASR-Nano vLLM 实时流式）/ `funasr`（标准 FunASR 2pass） |
| `FUNASR_WS_URL` | 流式 WebSocket 端点；`funasr_nano` 默认 `wss://asr.example.com/s2/ws` |
| `FUNASR_SERV` | `funasr` 协议的 HTTP 基址（参考 tan-asr/batch_asr.py） |
| `ASR_LANGUAGE` | `funasr_nano` 协议语种，默认 `中文` |
| `LLM_BASE` | taiji LLM 端点，默认 `https://asr.example.com/tencent/llm/taiji`（即完整端点） |
| `LLM_KEY` | API Key，默认 `xxxxx` |
| `LLM_MODEL` | 模型名，默认 `hy3` |
| `LLM_CHAT_PATH` | 端点后缀，taiji 留空；标准 OpenAI 服务填 `/v1/chat/completions` |
| `HOTWORDS_PATH` | 全局热词兜底（可留空，会前按科学家自动生成 session 专属热词） |

### 科学家热词自动生成

在控制台填入**科学家姓名 + 机构**后点「开始 Session」，系统会用 LLM 检索该科学家研究背景，自动生成：

- **专属热词**（研究领域核心术语/方法/缩写，10~25 个）→ 用于提升 ASR 识别准确率；
- **研究背景简介** → 写入 session 资料，作为大模型翻译时的上下文。

生成结果写入 `sessions/<id>/hotwords.txt` 和 `sessions/<id>/materials/speaker_profile.md`。
也可手动触发：`POST /api/hotwords/generate`（`{"name","institution","discipline"}`）。

### ASR 协议说明

- **`funasr_nano`**（默认）：Fun-ASR-Nano vLLM 服务，协议为 `START`/`LANGUAGE:中文`/`HOTWORDS:词1,词2`/`STOP` 文本命令 + 二进制 PCM（16k/mono/int16）。**服务端自带动态 VAD 断句**，客户端持续透传音频即可，无需本地断句。
- **`funasr`**：标准 FunASR WebSocket 2pass，协议为配置 JSON（`mode`/`chunk_size`/`is_speaking`）+ PCM 二进制帧，本地 VAD 控制断句。

## 快捷键（控制台）

`1..5` 触发 5 个任务 · `Enter` 投屏展示 · `Backspace` 丢弃 · `R` 重生成 · `Esc` 急停

## 目录

```
app/            FastAPI 应用（audio 采集/断句、asr 流式、context、agent、providers）
app/web/        console.html / screen.html（零框架原生前端）
sessions/       运行数据（SQLite 转写库、复盘导出）
tools/          check_env.py 环境自检
```
