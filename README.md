# AI 跨学科实时翻译席

本地执行的 Web 应用：会议现场投屏。实时听取报告与问答（流式 ASR），主持人口令后由大模型生成跨学科短翻译/桥接问题，投屏展示（本版无 TTS）。

对应需求文档：《Workshop_AI跨学科实时翻译席技术方案》；架构设计见 `../架构设计_AI跨学科实时翻译席.md`。

## 两种实现

本仓库有两套**功能等价、接口兼容**的实现，可随时切换：

|  | **Go 版**（`go/`） | Python 版（`app/`） |
|---|---|---|
| 定位 | **默认部署版本**（`Dockerfile` 构建它） | 保留可用，作为回退与对照 |
| 前端页面 | 同一套页面，`go:embed` 进二进制（`go/web/` 与 `app/web/` 逐字一致） | 从磁盘读取 |
| HTTP / WebSocket 接口 | 与 Python 版完全一致 | — |
| SQLite 表结构 | 与 Python 版完全一致，可共用同一份 `sessions/transcript.sqlite` | 同 |
| 并发模型 | goroutine + channel + context；ASR 重连为异步，不阻塞音频链路 | 线程 + asyncio + 跨线程事件投递 |
| 运行镜像 | 116 MB | 314 MB |

因为接口完全一致，**前端页面与 `使用说明.md` 的操作流程对两者通用**。

## 快速开始（Go 版，推荐）

```bash
cd go
CGO_ENABLED=1 go build -o seat .

cp ../.env.example ../.env      # 首次：填写 LLM_BASE / LLM_KEY / LLM_MODEL
./seat
```

- 操作员控制台：<http://127.0.0.1:8080/console>
- 会场投屏大屏：<http://127.0.0.1:8080/screen>

配置查找顺序：`BASE_DIR` 环境变量 → 当前目录 `.env` → 上级目录 `.env`（即仓库根的 `.env`，与 Python 版共用）。环境变量优先于 `.env`：

```bash
HOST=0.0.0.0 PORT=8081 ./seat                    # 换绑定地址/端口（容器部署常用）
BASE_DIR=/path/to/conf ./seat                    # 指定配置与数据目录
JIEBA_DICT_DIR=/path/to/jieba_dict ./seat        # 指定 jieba 词典目录，见下
```

### 构建依赖（cgo）

Go 版有 4 个 cgo 依赖，需要 `gcc` / `g++`：

| 依赖 | 用途 |
|---|---|
| `maxhawkins/go-webrtcvad` | VAD 断句。与 Python 版使用**同一个 webrtcvad C 库**，阈值语义一致 |
| `yanyiwu/gojieba` | 中文分词 + 词性标注（C++） |
| `mattn/go-sqlite3` | SQLite 驱动 |
| `gen2brain/malgo` | 本机声卡采集（miniaudio，运行时通过 dlopen 加载 ALSA，故 `libasound2` 非必需） |

**`JIEBA_DICT_DIR` 说明**：gojieba 默认用 `runtime.Caller` 从**编译期源码路径**推导词典目录，该路径指向构建时的模块缓存。若运行时该路径不存在（典型场景是容器/分发二进制），gojieba 会直接 panic。因此：

- 本地开发（模块缓存完整）通常**无需设置**；
- 容器或分发场景，请把词典目录（含 `pos_dict/` 子目录）与二进制一起带上，并用 `JIEBA_DICT_DIR` 指向它；
- 词典缺失时程序**降级为按标点切分**（对齐 Python 版 jieba 不可用时的行为），不会中断服务。

### 与 Python 版的已知行为差异

均为有意设计，非缺陷：

1. **重连期间丢帧**，而非阻塞排队后补发。Python 版在退避重连时持锁休眠（最坏约 25s）并让音频链路停摆，Go 版改为异步重连 + 有界丢帧，实时性优先。
2. **流水线启动时预热 ASR 连接**，避免开头几百毫秒音频因"首次发送才建连"被丢弃。
3. **读空闲超时 3 分钟**。Python 版底层 `websocket-client` 的 socket 超时为 10s，静默连接会被误判断开。
4. **中文分词边界个别句子不同**：gojieba 与 Python jieba 同源但非逐字一致，实测 10 句样本中 6 句完全相同；差异处各有得失（`带隙`/`隧穿` 被合并成词反而多识别出术语，`X射线` 被拆开少识别一个），净效果基本中性。
5. `GET /api/devices` 改用 malgo 枚举，返回字段与 Python 版的 sounddevice 不同（仅调试接口）。

## 快速开始（Python 版）

```bash
python -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
# 编辑 .env：填写 LLM_BASE / LLM_KEY / LLM_MODEL；按需调整 FUNASR_SERV / FUNASR_WS_URL

./run.sh          # 或 uvicorn app.main:app --host 127.0.0.1 --port 8080
```

`run.sh` 默认绑定 `0.0.0.0:8081`（避开常被 IDE 占用的 8080），并带 `--reload`。

## Docker 部署

```bash
# Go 版（默认）
docker build -t translation-seat:latest .
# Python 版
docker build -f Dockerfile.python -t translation-seat:py .

docker run -d --name translation-seat -p 8081:8081 \
  -v "$PWD/.env:/app/.env:ro" \
  -v "$PWD/sessions:/app/sessions" \
  translation-seat:latest
```

注意：

- 镜像内已固定 `HOST=0.0.0.0` / `PORT=8081`。godotenv **不覆盖已有环境变量**，所以挂载进来的 `.env` 里若写了 `HOST`/`PORT` 不会生效——改端口请用 `-e PORT=9000`。
- 前端页面已编进二进制，运行镜像不含静态文件目录。
- HEALTHCHECK 使用二进制自带的探活参数 `/app/seat -healthcheck`（运行镜像内没有 curl/python）。
- 容器内一般无音频设备，程序**自动降级为「浏览器收音」**。需容器直连声卡时加 `--device /dev/snd`。
- 镜像内附 `mockasr`（本地假 ASR 服务），可在无外网环境离线验证：`docker exec -it <容器> /app/mockasr -addr 127.0.0.1:18900`。

## 离线验证（无需真实 ASR 网关）

`go/tools/mockasr` 是一个实现了 `funasr_nano` 协议的本地假 ASR 服务，用于在网关不可达时验证
「采集 → VAD → ASR 客户端 → 落库 → 事件推送 → 页面渲染」整条链路：

```bash
# 终端 1：假 ASR 服务
cd go && go run ./tools/mockasr -addr 127.0.0.1:18900

# 终端 2：服务指向它（无需麦克风）
ASR_PROTOCOL=funasr_nano FUNASR_WS_URL=ws://127.0.0.1:18900 ./seat

# 终端 3：触发回放会话（WAV 需为 16k/mono/int16，或裸 PCM）
curl -X POST http://127.0.0.1:8080/api/session \
  -H 'Content-Type: application/json' \
  -d '{"title":"离线验证","capture":"browser","replay_path":"/path/to/16k-mono.wav"}'
```

`mockasr` 按累积数组返回 `sentences`（与真实 FunASR-Nano 服务端行为一致），因此可用于回归
「新句 / 服务端回退修正」这两条分支。

## 远端部署与浏览器收音

部署在服务器（无本地麦克风）时，可用**浏览器收音**：任意机器上的浏览器采集 16k PCM，经 WebSocket 推给服务进入 ASR 流水线。

1. 操作员打开 `http://<服务器IP>:<端口>/console`，勾选**「浏览器收音」**，点「开始 Session」
2. 点**「开始收音」**并授权麦克风——底部状态条显示"采集中（浏览器）"
3. 服务器无音频设备时，本地麦克风采集失败会**自动降级**为浏览器收音（无需手动设置）

> 浏览器麦克风权限要求**安全上下文**：`localhost`/`127.0.0.1` 或 HTTPS 可直接使用；远端 HTTP 访问需
> ① 用 SSH 端口转发走 localhost（推荐，`ssh -N -L <端口>:127.0.0.1:<端口> <user>@<服务器>`），或
> ② 在浏览器开启"将不安全来源视为安全"（Chrome：`chrome://flags/#unsafely-treat-insecure-origin-as-secure`）。

## 环境检查

```bash
python tools/check_env.py        # Python 版
```

## 关键配置（.env）

两套实现共用同一份 `.env`（字段名与默认值一致）。

| 变量 | 说明 |
|---|---|
| `ASR_PROTOCOL` | `qwen3_http`（Qwen3-ASR HTTP，客户端拼装流式）/ `hy_stream`（HY-ASR-3-Stream，逐词增量+语义 VAD）/ `funasr_nano`（Fun-ASR-Nano vLLM）/ `funasr`（标准 FunASR 2pass） |
| `QWEN3_BACKEND` | `qwen3_http` 的 HTTP 基址（如 `https://ml-serv.ssv.qq.com/s2`） |
| `QWEN3_MODEL` | `qwen3_http` 模型名 |
| `ASR_PARTIAL_INTERVAL` | `qwen3_http` 草稿重推周期（秒音频），调小更跟手、请求更频 |
| `HY_ASR_WS_URL` | `hy_stream` 端点 |
| `HY_ASR_TOKEN` / `HY_ASR_MODEL` | `hy_stream` 鉴权与模型 |
| `FUNASR_WS_URL` | 流式 WebSocket 端点；留空则按协议推导（`funasr_nano` 默认走 asr-gateway） |
| `FUNASR_SERV` | `funasr` 协议的 HTTP 基址 |
| `ASR_LANGUAGE` | `funasr_nano` / `qwen3_http` 语种，默认 `中文` |
| `LOCAL_VAD_SEGMENT` | `funasr_nano` 是否用本地 VAD 主动切句（句尾静音即发 STOP，出字更快）。关闭则退回服务端 VAD（实测 10~20s 才出一句） |
| `LLM_BASE` | taiji LLM 端点（即完整端点，不再拼接 `/v1/chat/completions`） |
| `LLM_KEY` | API Key |
| `LLM_MODEL` | 模型名，默认 `hy3` |
| `LLM_TIMEOUT` | 单次调用超时。**注意**：`hy3` 是思考模型，长 prompt（如热词生成）思考需 28~37s，该值偏小会导致随机超时；热词生成已在代码内单独给 90s |
| `LLM_MAX_TOKENS` | 生成预算。`hy3` 的 `reasoning_content` 与正文**共享**该预算，过小会导致正文为空 |
| `LLM_CHAT_PATH` | 端点后缀，taiji 留空；标准 OpenAI 服务填 `/v1/chat/completions` |
| `HOTWORDS_PATH` | 全局热词兜底（可留空，会前按科学家自动生成 session 专属热词） |

### 科学家热词自动生成

在控制台填入**科学家姓名 + 机构**后点「开始 Session」，系统会用 LLM 检索该科学家研究背景，自动生成：

- **专属热词**（研究领域核心术语/方法/缩写，10~25 个）→ 用于提升 ASR 识别准确率；
- **研究背景简介** → 写入 session 资料，作为大模型翻译时的上下文。

生成结果写入 `sessions/<id>/hotwords.txt` 和 `sessions/<id>/materials/speaker_profile.md`。
也可手动触发：`POST /api/hotwords/generate`（`{"name","institution","discipline"}`）。

> 该调用对 `max_tokens` 与超时都很敏感：预算 2000 会被思考吃光导致正文为空（0 个热词），
> 超时 30s 会随机掐断。代码内已按调用给到 `max_tokens=6000` / `timeout=90s`。

### ASR 协议说明

- **`qwen3_http`**：vLLM OpenAI 兼容转写 `POST /v1/audio/transcriptions`（整段 WAV → 文本，单段 ≤30s）。
  WS 流式体验由客户端拼装：每 `ASR_PARTIAL_INTERVAL` 秒把累积缓冲整段重推一次出草稿，
  webrtcvad 检测句尾静音后推理确认句；单段达 `MAX_SEGMENT_S` 强制切句；纯静音段不推理。
- **`hy_stream`**：`asr-gateway /tencent/asr/recognize/stream`，逐词增量 + 语义 VAD 自动断句，支持服务端回退修正。
- **`funasr_nano`**：Fun-ASR-Nano vLLM 服务，协议为 `START`/`LANGUAGE:中文`/`HOTWORDS:词1,词2`/`STOP` 文本命令 + 二进制 PCM（16k/mono/int16）。
  服务端自带动态 VAD 断句，客户端持续透传即可；本项目默认改用本地 VAD 主动切句以降低出字延迟。
- **`funasr`**：标准 FunASR WebSocket 2pass，协议为配置 JSON（`mode`/`chunk_size`/`is_speaking`）+ PCM 二进制帧，本地 VAD 控制断句。

## 快捷键（控制台）

`1..5` 触发 5 个任务 · `Enter` 投屏展示 · `Backspace` 丢弃 · `R` 重生成 · `Esc` 急停

## 目录

```
app/            Python 版（FastAPI：audio 采集/断句、asr 流式、context、agent、providers）
app/web/        console.html / screen.html（零框架原生前端）
go/             Go 版（internal/{config,events,state,store,llm,asr,audio,contextx,agent,server}）
go/web/         与 app/web/ 逐字一致，编译时 embed 进二进制
go/tools/mockasr  本地假 ASR 服务（离线验证整条流水线）
Dockerfile      Go 版镜像（默认）
Dockerfile.python  Python 版镜像
sessions/       运行数据（SQLite 转写库、session 资料、复盘导出）
tools/          check_env.py 环境自检（Python 版）
```
