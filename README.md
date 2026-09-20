# AI 跨学科实时翻译席

本地执行的 Web 应用：会议现场投屏。实时听取报告与问答（流式 ASR），主持人口令后由大模型生成跨学科短翻译/桥接问题，投屏展示（本版无 TTS）。

对应需求文档：《Workshop_AI跨学科实时翻译席技术方案》；架构设计见 `../架构设计_AI跨学科实时翻译席.md`。

除 5 个投屏翻译任务外，同一套页面还提供两项现场能力：

- **说话人区分**：CAM++ sidecar 逐句判定说话人，控制台/大屏在句子上方标注「说话人 N」，换人处画分隔线（见下文「说话人区分」）；
- **发言**：中栏填「立场」+ 选语言/长度 → 结合会议上下文生成发言稿，展示在右下角（见下文「发言」）。

## 实现

**单一实现：Go 版（`go/`）** —— 即 `Dockerfile` 构建的默认部署版本；前端页面 `go:embed` 进二进制，运行镜像无需静态文件。

> 早期的 Python 版（`app/`）已于 2026-09 随 Go 版稳定后**整体移除**（历史可查 git 提交），
> 以免两套实现重复维护。下文仍出现「Python 版」字样的段落，均为保留的设计对照记录，非现役代码。

## 快速开始

```bash
cd go
CGO_ENABLED=1 go build -o seat .

cp ../.env.example ../.env      # 首次：填写 LLM_BASE / LLM_KEY / LLM_MODEL
./seat
```

- 操作员控制台：<http://127.0.0.1:8080/console>
- 会场投屏大屏：<http://127.0.0.1:8080/screen>

配置查找顺序：`BASE_DIR` 环境变量 → 当前目录 `.env` → 上级目录 `.env`（即仓库根的 `.env`）。环境变量优先于 `.env`：

```bash
HOST=0.0.0.0 PORT=8081 ./seat                    # 换绑定地址/端口（容器部署常用）
BASE_DIR=/path/to/conf ./seat                    # 指定配置与数据目录
JIEBA_DICT_DIR=/path/to/jieba_dict ./seat        # 指定 jieba 词典目录，见下
```

### 构建依赖（cgo）

Go 版有 4 个 cgo 依赖，需要 `gcc` / `g++`：

| 依赖 | 用途 |
|---|---|
| `maxhawkins/go-webrtcvad` | VAD 断句（webrtcvad C 库） |
| `yanyiwu/gojieba` | 中文分词 + 词性标注（C++） |
| `mattn/go-sqlite3` | SQLite 驱动 |
| `gen2brain/malgo` | 本机声卡采集（miniaudio，运行时通过 dlopen 加载 ALSA，故 `libasound2` 非必需） |

**`JIEBA_DICT_DIR` 说明**：gojieba 默认用 `runtime.Caller` 从**编译期源码路径**推导词典目录，该路径指向构建时的模块缓存。若运行时该路径不存在（典型场景是容器/分发二进制），gojieba 会直接 panic。因此：

- 本地开发（模块缓存完整）通常**无需设置**；
- 容器或分发场景，请把词典目录（含 `pos_dict/` 子目录）与二进制一起带上，并用 `JIEBA_DICT_DIR` 指向它；
- 词典缺失时程序**降级为按标点切分**，不会中断服务。

### 设计取舍记录（对照已移除的 Python 版）

以下均为有意设计，非缺陷：

1. **重连期间丢帧**，而非阻塞排队后补发。Python 版在退避重连时持锁休眠（最坏约 25s）并让音频链路停摆，Go 版改为异步重连 + 有界丢帧，实时性优先。
2. **流水线启动时预热 ASR 连接**，避免开头几百毫秒音频因"首次发送才建连"被丢弃。
3. **读空闲超时 3 分钟**。Python 版底层 `websocket-client` 的 socket 超时为 10s，静默连接会被误判断开。
4. **中文分词边界个别句子不同**：gojieba 与 Python jieba 同源但非逐字一致，实测 10 句样本中 6 句完全相同；差异处各有得失（`带隙`/`隧穿` 被合并成词反而多识别出术语，`X射线` 被拆开少识别一个），净效果基本中性。
5. `GET /api/devices` 改用 malgo 枚举，返回字段与 Python 版的 sounddevice 不同（仅调试接口）。
6. **前端页面在 Python 版封板后继续演进**：现役 `go/web/` 比当年的 `app/web/` 多出「说话人标记」与「发言/发言稿」两块。
   DOM 引用（JS 引用的 id 必须真实存在）由 `go/web_assets_test.go` 钉住。

## 说话人区分（CAM++ sidecar）

一句话里"换人了"在字幕上是看不出来的。本功能给每句转写补一个说话人标记：
同一 session 内编号稳定（S1/S2/…），控制台左栏与大屏字幕在该句上方显示「说话人 N」，
换人处画一条分隔线（控制台虚线 + 缩进，大屏左侧竖线）。

- **怎么做**：主程序（Go）把「一次连续说话」的音频（本地 VAD 切段，带起止时间）丢给 sidecar；
  sidecar 用 CAM++ 提 192 维声纹，与本场已有说话人中心做余弦比对（阈值 `DIARIZE_THRESHOLD`），
  低于阈值就新建一位说话人，高于阈值则并入并把该段音频按长度加权进中心。
  判定结果回来后再与已落库的句子按时间对齐，回填 `segment.speaker_hint` 并推 `SPEAKER_ASSIGNED`。
- **为什么是 sidecar**：CAM++ 要 PyTorch，而主程序是 Go（cgo 只带了 VAD/分词/SQLite）；
  依赖体积也不适合进主镜像，因此交给 `tools/diarize/` 在**运行时**装依赖、下模型。
- **异步、可缺失**：判定与转写解耦（有界队列 + 丢弃），sidecar 慢/挂都不会阻塞音频链路；
  拿不准的句子宁可不标（超时未配对的条目 120s 后丢弃），也不会标错人。
- **同一 session 内对齐**：说话人表持久化在 `sessions/<sid>/speakers.json`，
  sidecar 重启后重连同一场次，编号仍从 S1 续上；新开 session 自动重置。

启动 sidecar（首次会自动建 venv、装依赖、下模型，约 200MB + 28MB，之后启动 2~4s）：

```bash
# 方式一：由主程序自动拉起（.env 里 DIARIZE_ENABLED=true，URL 指向本机时默认开）
# 方式二：手动（推荐首次先手动跑一遍，能直接看到安装与加载日志）
bash tools/diarize/run.sh --port 18901

# 没有 Python 环境 / 只想验证链路是否通：mock 模式（不做真实声纹，仅按固定规则分组）
python3 tools/diarize/server.py --mock --port 18901
```

`.env` 配置：

```bash
DIARIZE_ENABLED=false      # 打开开关
DIARIZE_URL=http://127.0.0.1:18901
DIARIZE_THRESHOLD=0.5      # 同人判定阈值：同一个人被拆成多个编号→调小；不同人并成一个→调大
DIARIZE_MIN_MS=600         # 过短语音段不判定
DIARIZE_MAX_SEG_S=6        # 音频段最长秒数：多人接话时 15s 段会混入多人的声音，不同人容易被并成一个编号
DIARIZE_TIMEOUT=20
DIARIZE_AUTOSTART=true     # 不可达时自动执行 run.sh（仅本机 URL）
```

页脚 `说话人:` 会显示三种状态：`未启用`（配置关闭）/ `区分中（已标 N 句）` / `sidecar 不可达`。
sidecar 侧日志在主程序数据目录下（`sessions/diarize.log`，由自动拉起时重定向），每次判定一行（含相似度）；
排查「编号乱跳 / 句子没标记」时再打开 `DIARIZE_DEBUG=true`，会打印每段语音的判定结果与句子配对决策。

**对齐规则**（`internal/server/diarize.go`，实测踩过的坑都写在常量注释里）：

- 语音段与句子按时间配对：**有重叠优先**（本地 VAD 模式即此情形），无重叠时允许「接近」（服务端断句会晚几秒）；
- 一句话如果与某段音频只有"擦边"或"接近"的关系，会先**等一会儿**（`settleAfter`）：真正对应的那段音频的
  判定结果往往还差一两秒，急着配会把它配给上一段并顺着继承污染后面的句子；
- 同一段连续说话被 ASR 拆成多句时**继承**该段说话人（要求实打实的重叠，且重叠占该句估计时长的
  `inheritMinContain` 以上），因此"一段话里三句只有第一句有标记"这种情况不会出现；
- 超时未配对的条目 120s 后丢弃：宁可不标，也不标错。

## 发言（按立场生成发言稿）

给与会者自己用的能力：**填立场 → 结合现场上下文生成一段可朗读的发言稿**，
展示在控制台**右下角**（与右上「AI 输出卡」完全独立，生成过程不会清掉大屏上的卡片）。

- 中栏底部填写：**立场**（必填）、**语言**（中文/English/中英双语）、**长度**（约 30 秒 / 1 分钟 / 2 分钟）、
  补充要求（可选，如「面向政府听众」）；点「发言」（或在立场框按 `Ctrl/Cmd+Enter`）。
- 后端走 `task=SPEECH` 的独立提示词（`agent.SpeechSystemPrompt`）：允许表达立场与分段，
  但同样禁止编造会议中未出现的数据/结论；`LLM_MAX_TOKENS` 不够用，单独用 `SPEECH_MAX_TOKENS`（默认 6000）。
- 校验只卡两件事：字数窗口与口播时长（窗口与语言/长度档位一一对应，由 `GET /api/speech/options` 下发到页面，
  避免"提示词按 2 分钟、校验按 30 秒"的口径漂移）；不再强制"必须请求确认"「禁评价词」那套短任务铁律。
- 右下角的稿子可直接编辑（失焦保存）、**重生成**、**复制**、**投屏**。
  投到大屏时角标会变成「AI 起草发言稿 · 供发言人修改使用」，留屏 2 分钟（翻译任务是 30 秒）。

## Docker 部署

```bash
docker build -t translation-seat:latest .

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

若同时要离线验证**说话人区分**链路，再起一个 mock sidecar（不加载模型，按音频统计量分组）：

```bash
# 终端 3：假说话人 sidecar
python3 tools/diarize/server.py --mock --port 18901

# 启动主程序时打开开关（AUTOSTART 关掉，避免它去装真依赖）
DIARIZE_ENABLED=true DIARIZE_AUTOSTART=false ./seat
```

## 远端部署与浏览器收音

部署在服务器（无本地麦克风）时，可用**浏览器收音**：任意机器上的浏览器采集 16k PCM，经 WebSocket 推给服务进入 ASR 流水线。

音源由顶栏的**「浏览器收音」开关**决定（**默认勾选**），改动在下次点「开始 Session」时生效：

1. 操作员打开 `http://<服务器IP>:<端口>/console`，保持**「浏览器收音」勾选**，点「开始 Session」
2. 浏览器会**自动请求麦克风权限**并开始推流——底部状态条显示"采集中（浏览器）"，**无需再点其他按钮**
3. 取消勾选则改用**服务器本机声卡**；若本机声卡打开失败，服务端会**自动降级**为浏览器收音（此时前端也会自动开麦，无需手动切换）
4. 采集期间顶栏会出现**「停止收音」**按钮，可随时关闭浏览器麦克风（服务端流水线继续运行，重新点「开始 Session」可再开）

> 浏览器麦克风权限要求**安全上下文**：`localhost`/`127.0.0.1` 或 HTTPS 可直接使用；远端 HTTP 访问需
> ① 用 SSH 端口转发走 localhost（推荐，`ssh -N -L <端口>:127.0.0.1:<端口> <user>@<服务器>`），或
> ② 在浏览器开启"将不安全来源视为安全"（Chrome：`chrome://flags/#unsafely-treat-insecure-origin-as-secure`）。

## 环境检查

服务启动后，用健康接口查看 ASR / LLM / 收音 / 说话人状态（页面页脚也实时显示）：

```bash
curl -s http://127.0.0.1:8081/api/health
```

## 关键配置（.env）

字段名与默认值见 `.env.example`。

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
| `LLM_MODEL` | 模型名，当前 `hy4-preview` |
| `LLM_REASONING_EFFORT` | 思考开关：`no_think` 关闭思考（hy 系列）。思考阶段只流 `reasoning_content`（客户端丢弃），首字要等 10~30s；关掉后正文直接开始流。留空=模型默认（思考） |
| `LLM_TIMEOUT` | 单次调用超时。**注意**：思考模式下长 prompt（如热词生成）思考需 28~37s，该值偏小会导致随机超时；热词生成已在代码内单独给 90s |
| `LLM_MAX_TOKENS` | 生成预算。思考模式下模型 `reasoning_content` 与正文**共享**该预算，过小会导致正文为空；`no_think` 后不再共享 |
| `LLM_CHAT_PATH` | 端点后缀，taiji 留空；标准 OpenAI 服务填 `/v1/chat/completions` |
| `SPEECH_MAX_TOKENS` | 「发言」的单次生成预算（默认 6000）。发言稿最长 2 分钟，比短翻译长得多 |
| `DIARIZE_ENABLED` | 是否开启说话人区分（默认 false）。开启需要 Python 环境跑 CAM++ sidecar |
| `DIARIZE_URL` | sidecar 地址，默认 `http://127.0.0.1:18901` |
| `DIARIZE_THRESHOLD` | 同一说话人判定阈值（余弦，默认 0.5）：同人被拆开→调小，不同人并一起→调大 |
| `DIARIZE_MIN_MS` | 过短的语音段不做判定（默认 600ms） |
| `DIARIZE_MAX_SEG_S` | 说话人区分的音频段最长秒数（默认 6，与 ASR 的 `MAX_SEGMENT_S` 分开）：段越短、段内越可能只含一人；多人接话时 15s 段会把不同人并成一个编号 |
| `DIARIZE_TIMEOUT` | 单段声纹判定超时（秒，默认 20） |
| `DIARIZE_AUTOSTART` | sidecar 不可达时自动拉起（优先 Go 版 `tools/diarize-go/seat-diarize`，其次 Python 版 `tools/diarize/run.sh`；默认 true，仅本机 URL） |
| `DIARIZE_DEBUG` | 打印每段语音的判定与配对决策（默认 false，排查编号乱跳时打开） |
| `REC_ENABLED` | 会话录音留存（默认 false）：原始音频连续写成 WAV 分片 `sessions/<sid>/rec/`，供事后排查；录音含会议内容，开启前确认现场同意 |
| `REC_SEGMENT_SEC` | 录音分片时长（秒，默认 300） |
| `REC_KEEP_DAYS` | 录音保留天数（默认 7）：超期自动清理（服务启动时 + 新建会话时）；`0`=不清理。占用约 115MB/小时 |
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
go/             Go 版主程序（internal/{config,events,state,store,llm,asr,audio,contextx,agent,diarize,server}）
go/web/         console.html / screen.html（零框架原生前端，编译时 embed 进二进制）
go/tools/mockasr  本地假 ASR 服务（离线验证整条流水线）
Dockerfile      Go 版镜像
sessions/       运行数据（SQLite 转写库、session 资料、复盘导出、
                <sid>/speakers.json 说话人表、diarize.log sidecar 日志）
tools/diarize/  CAM++ 说话人区分 sidecar（run.sh 运行时装依赖+下模型；server.py 可 --mock 联调）
```
