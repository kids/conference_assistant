# AI 跨学科实时翻译席 — 项目架构设计文档

> 对应需求：《新基石研究员会议_AI跨学科实时翻译席技术方案》（讨论稿 v1.0）
> 本文档定义 MVP 的技术架构：**一个本地执行的 Web 应用**（单机运行，会议现场投屏）。
> ASR 采用 **Fun-ASR-Nano vLLM 实时流式协议（WebSocket）**，在本项目独立实现；服务地址 `ml-serv.ssv.qq.com/s2/ws`（HTTP 整段接口 `/s2/asr` 参考 `tan-asr/batch_asr.py`）。
> 版本：架构设计 v1.2（Fun-ASR-Nano vLLM 流式 ASR + 纯投屏显示，暂不含 TTS）
>
> **实现状态（2026-09）**：本文写于 Python 版（`app/`）时期，该版本已移除（历史可查 git）；
> 现役实现为 **Go 版**（`go/`），与本架构逐项对齐，构建与部署见 `README.md`。
> 本文的架构设计、协议与规则仍是现役实现的依据。

---

## 1. 设计目标与约束

### 1.1 必须满足（来自需求文档）

| 编号 | 约束 | 架构落点 |
|---|---|---|
| C1 | 实时听：持续接收现场音频并转写 | 本地音频采集 + Fun-ASR-Nano 流式（服务端 VAD 断句） |
| C2 | 受控说：只有主持人唤醒才发声 | 状态机默认 `LISTENING`，展示必须经操作员确认 |
| C3 | 短发声：单次 20–30 秒（对应字数窗口） | Prompt 硬约束字数 + 服务端字数裁剪 + 预估口播时长拦截 |
| C4 | 任务窄：每次只做一件事 | 5 个独立 Task 类型，各自独立 prompt，不做通用对话 |
| C5 | 专家确认：输出须由报告人确认 | 输出模板强制以确认句结尾 + 大屏显著标注 |
| C6 | 不评价 | 系统提示词负向约束 + 输出后置敏感词过滤（评价类词表） |
| C7 | 可打断：随时停止 | 全局 Kill Switch（快捷键/按钮），立即中止生成 |
| C8 | 端到端延迟 ≤ 8s | 延迟预算见 §7，各环节设硬超时（本版无 TTS，预算更宽裕） |
| C9 | 原始音频默认不留存 | 内存环形缓冲，落盘需显式开关，退出即清理 |
| C10 | 数据隔离优先 | ASR/LLM 全部走可配置 endpoint，支持全内网/本地模型 |

### 1.2 部署约束

- **单机运行**：一台笔记本（macOS/Linux）即可跑全部服务，不依赖会场网络的公网出口（除模型服务地址可配）。
- **投屏**：会场大屏通过 HDMI 扩展显示器打开浏览器全屏页面；操作员在本机主屏用控制台页面。
- **零安装依赖终端**：大屏只需一个现代浏览器（Chrome/Edge/Safari）。
- **可离线彩排**：支持喂入本地录音文件回放，复现现场链路（对应需求"阶段 0 离线模拟"）。

### 1.3 明确不做（MVP 边界）

- 不做多会场/多路会议并发（单 session 单实例）。
- 不做说话人聚类（Speaker Diarization），只做"发言轨道"粗分（主持人麦 / 报告人麦分通道，可选）。
- 不做复杂向量知识库，会前资料以本地文件夹 + 全量塞入上下文为主。
- 不做用户体系与权限系统（本地单机，物理隔离即权限）。
- **本版不做 TTS**：AI 输出仅投屏显示（大字卡），由主持人现场口播或听众自读。

---

## 2. 系统总体架构

### 2.1 分层视图

```
┌──────────────────────────────────────────────────────────────────────┐
│  展示层（浏览器）                                                       │
│  ┌────────────────────────┐        ┌──────────────────────────────┐  │
│  │ /console  操作员控制台   │        │ /screen  会场投屏大屏          │  │
│  │ 转写流 / 候选术语 /      │        │ 实时字幕 / AI 大字卡 /         │  │
│  │ 生成·预览·展示·丢弃      │        │ 状态灯 / "AI 翻译尝试"水印      │  │
│  └───────────┬────────────┘        └──────────────┬───────────────┘  │
└──────────────│──────────────────────────────────── │──────────────────┘
               │  WebSocket /ws/console              │ WebSocket /ws/screen
┌──────────────▼─────────────────────────────────────▼──────────────────┐
│  应用层（本地 FastAPI，单进程 + 线程池）                                  │
│  ┌──────────┐ ┌───────────┐ ┌────────────┐ ┌──────────┐ ┌──────────┐ │
│  │ 会话编排  │ │ 上下文管理 │ │ 智能体编排  │ │ 展示控制 │ │ 事件总线  │ │
│  │ Session  │ │ Context   │ │ AgentRunner│ │ Display  │ │ EventBus │ │
│  └──────────┘ └───────────┘ └────────────┘ └──────────┘ └──────────┘ │
├───────────────────────────────────────────────────────────────────────┤
│  流水线层                                                              │
│  AudioCapture → FunAsrNanoStreamClient(服务端 VAD) → Transcript        │
│                                                   ↘ Wakeword           │
├───────────────────────────────────────────────────────────────────────┤
│  适配层（可替换 Provider）                                              │
│      AsrProvider(FunASR-WS 流式)        │        LlmProvider           │
└───────────────────────────────────────────────────────────────────────┘
        │  wss://ml-serv.ssv.qq.com/s2/ws       │  https://ml-serv.ssv.qq.com/tencent/llm/taiji
     Fun-ASR-Nano vLLM 流式推理服务         LLM（taiji hy3，思考模型）
```

### 2.2 关键架构决策（ADR 摘要）

| # | 决策 | 理由 | 被否方案 |
|---|---|---|---|
| A1 | 音频在 **Python 后端**采集（sounddevice/PortAudio），不用浏览器 getUserMedia | 大屏页面可能在扩展屏/另一台机器；需要选择调音台声卡设备、双通道；浏览器权限与自动播放策略不可控 | 前端采集上传 |
| A2 | **流式 ASR**：Fun-ASR-Nano vLLM 实时 WebSocket，`START`/`STOP` 命令 + PCM 帧，**服务端自带动态 VAD 断句** | 实测 `ml-serv.ssv.qq.com/s2/ws` 可用；服务端 VAD 天然给出句子边界与 confirmed 句子；客户端只需持续透传音频 | 整段 HTTP（`/s2/asr`）/ 标准 FunASR 2pass 协议 |
| A3 | AI 输出**纯投屏显示**（本版无 TTS） | 需求决策：先验证"翻译质量与现场接受度"，避免 TTS 选型与音响链路阻塞 MVP；播报由主持人完成 | TTS 合成播放 |
| A4 | 前后端通信用 **WebSocket 广播 + 只读大屏** | 大屏零交互、断线自动重连即可恢复；控制台是唯一写入端 | 轮询 REST |
| A5 | 状态机集中在后端，前端无状态 | 断线/刷新不影响会议；Kill Switch 必须在唯一权威处生效 | 前端管状态 |
| A6 | 数据存 **SQLite（WAL）+ 内存环形缓冲** | 单机零运维；转写文本可落盘复盘，音频默认只在内存 | Postgres / 全内存（丢复盘） |
| A7 | ASR 流式协议独立实现；热词由**科学家背景自动生成**（不再依赖固定词表） | 流式协议按 Fun-ASR-Nano vLLM 实时协议实现（`START`/`STOP` + PCM）；热词由 LLM 根据科学家姓名+机构生成 session 专属词表 | 复用 HTTP 整段接口 / 复用固定 `hotwords_dict.txt` |

---

## 3. 模块设计

### 3.1 AudioCapture（音频采集）

- 输入源可选：
  - `device`：本机声卡/调音台/USB 麦（`sounddevice.InputStream`），16 kHz、mono、int16；
  - `file`：本地录音回放（离线彩排/阶段 0），按真实时间轴节流喂入；
  - 可选双通道模式：`ch0 = 主持人麦`，`ch1 = 报告人/听众麦`，分轨转写以提升唤醒词识别与角色标注。
- 输出：20 ms PCM 帧（640 bytes @16k/int16）→ 内部 `queue.Queue`。
- **RingBuffer**：内存保留最近 **120 秒** 原始 PCM（约 3.8 MB/通道），供"刚才那个术语"回溯重识别使用。超出即覆盖，不落盘。
- 采集异常（设备掉线）→ 事件总线告警 → 控制台红条提示 + 自动重试重开流。

### 3.2 VadSegmenter（断句）

- 一级：能量 + `webrtcvad`（aggressiveness=2）判定语音/静音。
- 二级：句子边界规则
  - `speech_start`：连续 ≥ 3 帧语音；
  - `speech_end`：连续静音 ≥ **600 ms**；
  - 强制切分：单段 > **15 s** 时在最近静音点强切（防长句拖延迟）；
  - 丢弃：段长 < 300 ms 或平均能量低于噪声门（防空段打 ASR）。
- 输出事件：
  - `SPEECH_START`：开始一段语音；
  - `SPEECH_END`：句子结束（触发 `is_speaking=false`，产出终稿）。

### 3.3 FunAsrNanoStreamClient（流式识别，本项目独立实现）

- **协议**（对齐 Fun-ASR-Nano vLLM 实时服务 `serve_realtime_ws.py`，端点 `ml-serv.ssv.qq.com/s2/ws`）：

  1. 建立 WebSocket 连接：`wss://ml-serv.ssv.qq.com/s2/ws`（Kong 网关 `/s2/` 前缀 → 内部 `/ws`）；
  2. 发送**文本命令**初始化：

     ```
     START
     LANGUAGE:中文
     HOTWORDS:节水抗旱稻,耦合簇方法
     ```

     - `START` 初始化会话；`LANGUAGE:中文` 设置语种；`HOTWORDS:词1,词2` 设置热词（逗号分隔，词来自 `hotwords_dict.txt` 第一列）；
  3. 持续发送 **PCM 二进制帧**（16 kHz / mono / int16 little-endian，无 WAV 头），**含静音帧**——服务端动态 VAD 依赖静音检测句子端点；
  4. 会话结束发 `STOP` 触发 flush 与最终结果。

- **服务端返回**：
  - `{"event":"started"}` / `{"event":"language_set",...}` / `{"event":"hotwords_set",...}` / `{"event":"stopped"}` —— 控制事件；
  - `{"sentences":[{text,start,end,spk}...],"partial":"...","is_final":false}` —— 中间结果；
    - `sentences`：**服务端已确认的句子**（累积数组），新句子经去重后写入 TranscriptStore 进 LLM 上下文；
    - `partial`：当前未确定的临时文本 → 上屏为滚动字幕（草稿）；
  - `{"sentences":[...],"partial":"","is_final":true}` —— 最终结果。
- 实现要点：
  - **服务端自带动态 VAD 断句**（静音阈值随句长自适应 2.0s→0.1s），客户端无需本地断句，持续透传所有帧即可；
  - 发送用主流水线线程，接收用独立线程 `recv()` 循环解析 JSON；
  - 自动重连：连接断开 → 指数退避重连，重连后重新发 `START`/`LANGUAGE`/`HOTWORDS`，句子计数归零；
  - 热词复用 `batch_asr.py` 的 `_load_hotwords()` 语义（`词<TAB>权重`，`#` 注释跳过），提取词列表转为逗号分隔；
  - 兼容降级：`ASR_PROTOCOL=funasr` 可切标准 FunASR 2pass 协议（`app/asr/funasr_ws.py`，本地 VAD 断句）。

### 3.4 TranscriptStore + ContextManager（上下文）

- `TranscriptStore`：按 session 顺序存 `Segment{ id, t_start, t_end, track, speaker_hint, text, revised_text, is_final }`。
  - 操作员可在控制台**行内修正**术语/人名错识 → 写 `revised_text`，LLM 只读修正后文本（对应需求"ASR 识别错误"控制措施）。
- `ContextManager` 组装 LLM 输入，分四块并做预算控制（总 ≤ 6k tokens）：

| 块 | 内容 | 来源 | 预算 |
|---|---|---|---|
| STATIC | 报告摘要、PPT 文本、报告人简介、session 主题、术语表 | 会前 `sessions/<id>/materials/` 目录（.md/.txt/.pdf→文本/.pptx→文本） | ≤ 2.5k |
| RECENT | 最近 **30 分钟**转写（超出按段落做压缩摘要，滚动淘汰） | TranscriptStore | ≤ 2.5k |
| FOCUS | 最近 **60 秒**原文逐字 + 操作员用鼠标框选的目标句 | TranscriptStore / UI 选区 | ≤ 0.6k |
| HISTORY | 本场已展示过的 AI 输出（防重复解释同一术语） | InvocationStore | ≤ 0.4k |

- **术语候选提取**（给操作员减负）：final 段落到达后，用轻量规则实时打分并推给控制台"候选术语区"：
  - 命中会前术语表/session 专属热词 → 高分；
  - 大写缩写 / 英文词 / 中英混排 / 低频专业名词（对比通用词频表）→ 中分；
  - 已解释过 → 降权。
  - 该步不调用 LLM（0 成本、0 延迟）。

#### 3.4.1 HotwordBuilder（科学家热词自动生成）

会前输入**科学家姓名 + 机构**，用 LLM 检索其研究背景，自动生成两样东西：

| 产出 | 用途 | 落盘 |
|---|---|---|
| 专属热词（核心术语/方法/缩写，10~25 个，带权重） | 提升 ASR 对专业术语的识别准确率；兼作术语候选打分词表 | `sessions/<id>/hotwords.txt` |
| 研究背景简介（150 字） | 注入 STATIC 上下文，提升翻译智能体对该领域背景的理解 | `sessions/<id>/materials/speaker_profile.md` |

- 实现：`app/context/hotword_builder.py`，向 LLM 发结构化 prompt（要求输出 JSON：`profile`/`fields`/`hotwords[{word,weight}]`），容错解析（剥 ` ```json ` 包裹 / 提取首个 `{...}`）。
- 触发：session 创建时若填了科学家姓名则同步生成；亦可手动 `POST /api/hotwords/generate` 重新生成（生成后自动重启 ASR 流水线使新热词生效）。
- 降级：LLM 失败/未配置 → 退回全局兜底热词，不影响开会。

### 3.5 AgentRunner（大模型翻译智能体）

- 5 个 Task（与需求附录口令一一对应）：

| Task | 口令关键词 | 输入焦点 | 输出模板 | 目标字数 |
|---|---|---|---|---|
| `TERM` 术语翻译 | "解释…这个术语" | 目标术语 + FOCUS | "这个术语可以理解为……它在本报告中重要，是因为……这个解释请报告人确认。" | 80–120 字 |
| `Q_TRANSLATE` 问题转译 | "把刚才这个问题转译" | 最近提问段 | "刚才的问题更一般地说是在问……它可能与……领域也有关。这个转译请提问者和报告人确认。" | 80–120 字 |
| `A_TRANSLATE` 回答转译 | "把报告人刚才的回答转述" | 最近回答段 | 通俗版复述 + 确认句 | 80–120 字 |
| `BRIDGE` 桥接提问 | "跨学科桥接问题" | RECENT + STATIC | "一个可以追问的问题是……这个问题可能连接……和……。这个连接是否成立，请报告人判断。" | 70–110 字 |
| `SUMMARY` 报告总结 | "通俗易懂的总结" | 全场 RECENT | 通俗总结 + 确认句 | 110–150 字 |

- Prompt 结构：`system(角色边界 + 禁令) + context(四块) + task_instruction(模板与字数) + guardrail(自检)`。
  - 禁令固化：不评价研究质量/不比较学术贡献/不下结论/不编造数据/不确定就说"这一点需要报告人确认"。
  - 输出要求**纯口播文本**（无 Markdown、无列表、无括注），便于主持人直接朗读。
- **后置校验流水线**（失败即拒绝并给操作员显示原因）：
  1. 字数窗口校验（超出 → 一次自动重生成，仍超 → 截断到最后一个完整句）；
  2. 评价类词表拦截（"很有价值/水平很高/突破性/不如/领先于"等）；
  3. 必含确认句校验；
  4. 禁止 Markdown/换行符；
  5. 口播时长预估（中文 ~4.5 字/秒）> 30 s → 拒绝。
- 流式生成（SSE/增量）推给控制台，让操作员边生成边读。
- Provider：OpenAI 兼容 `POST {LLM_BASE}/v1/chat/completions`，`temperature=0.3`，`max_tokens=300`，硬超时 6 s（超时→"生成超时，请重试"）。

### 3.6 DisplayController（展示控制，本版替代 TTS 播报）

- 状态机：

```
IDLE ──generate──▶ GENERATING ──ok──▶ PENDING_APPROVAL ──show──▶ SHOWING ──▶ IDLE
                        │ fail              │ discard              │ KILL
                        ▼                  ▼                      ▼
                      IDLE               IDLE                  IDLE(立即下屏)
```

- 关键行为：
  - **投屏即展示**：进入 `SHOWING` 后，AI 输出以大字卡固定展示在 `/screen` 上部 70% 区域，直到主持人"结束展示"或下一次调用；
  - **Kill Switch**：`Esc`（控制台）/ 全局热键 / REST `POST /api/kill`，立即中止生成并清空大屏 AI 卡（≤ 100 ms）；
  - 无 TTS，故不存在"音响回采"问题，ASR 无需播报门控（保留扩展点，后续接入 TTS 时启用）。

### 3.7 WakewordMatcher（阶段 2 自动触发）

- 输入：终稿文本（优先主持人通道）。
- 匹配：口令模板集合做**归一化模糊匹配**（去标点、数字/中文数字归一、拼音首字母兜底、编辑距离阈值），命中即产出 `Intent{task, target_hint}`。
- **默认关闭**（`WAKEWORD_AUTO=false`）：MVP 阶段 1 由操作员点按钮；开启后也只自动**生成候选**，仍需操作员确认才展示（保持 C2、C7）。
- "AI，暂停"类纠偏口令例外：命中即执行 Kill，无需确认。

### 3.8 前端两个页面

**`/console` 操作员控制台**（1 屏 3 列）

- 左：实时转写流（草稿灰、终稿黑；可点击行内编辑修正；鼠标框选设定 FOCUS）；
- 中：候选术语区（自动打分排序，点击即以该术语发起 `TERM`）；5 个任务大按钮 + 快捷键 `1..5`；
- 右：AI 输出卡（流式显示、字数/预估时长实时校验、`Enter` 展示 / `Backspace` 丢弃 / `R` 重生成 / `Esc` 急停）；
- 底部状态条：ASR/LLM 健康灯 + 各环节延迟（p50/p95）+ 本场调用次数与累计展示时长（超 4 次提醒，对应需求"每场 3–4 次"）。

**`/screen` 会场投屏大屏**（只读、深色、远距可读）

- 上部 70%：AI 卡片区——大字号显示当前 AI 输出，附**任务类型徽标**与固定角标"AI 跨学科翻译尝试 · 待报告人确认"；
- 下部 25%：实时字幕（滚动 2–3 行，草稿轨半透明）；
- 顶栏：状态灯（🟢静默听会 / 🟡生成中 / 🔵展示中）+ session 标题 + 计时；
- 无 AI 输出时（静默听会）默认仅显示字幕与极简会议信息，避免干扰报告。
- 断线自动重连（指数退避），重连后由后端补发当前快照（`SNAPSHOT` 消息）。

---

## 4. 接口设计

### 4.1 REST（控制台专用，本机 127.0.0.1 绑定）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/session` | 创建 session（标题、报告人、机构、学科、材料目录、是否启用 AI）；填了报告人则自动生成专属热词与背景 |
| POST | `/api/hotwords/generate` | `{name, institution?, discipline?}` → 手动（重新）生成科学家热词与背景，并重启 ASR 使热词生效 |
| POST | `/api/session/{id}/start` \| `/stop` | 开始/结束采集 |
| GET | `/api/devices` | 枚举音频输入/输出设备 |
| POST | `/api/invoke` | `{task, target?, focus_range?}` → 返回 `invocation_id`，异步生成 |
| POST | `/api/invocation/{iid}/show` \| `/discard` \| `/regenerate` | 展示/丢弃/重生成 |
| POST | `/api/kill` | 全局急停（幂等，任何状态可调） |
| PATCH | `/api/segment/{sid}` | 修正转写文本 |
| GET | `/api/health` | ASR/LLM 连通性 + 延迟探测 |
| GET | `/api/session/{id}/export` | 导出复盘包（转写 + 调用记录 + 确认状态，见 §6.3） |

### 4.2 WebSocket 消息协议

服务端 → 客户端（JSON，`type` 区分；`/ws/screen` 只收下表子集）：

```json
{"type":"TRANSCRIPT_PARTIAL","seg_id":"s12","text":"我们用的是…","t":128.4}
{"type":"TRANSCRIPT_FINAL","seg_id":"s12","text":"我们用的是耦合簇方法。","speaker":"speaker","t_start":124.1,"t_end":128.9}
{"type":"TERM_CANDIDATES","items":[{"term":"耦合簇方法","score":0.86,"seg_id":"s12"}]}
{"type":"AI_STATE","state":"GENERATING","task":"TERM","invocation_id":"iv3"}
{"type":"AI_DELTA","invocation_id":"iv3","delta":"这个术语可以理解为"}
{"type":"AI_READY","invocation_id":"iv3","text":"…请报告人确认。","chars":96,"est_sec":21.3,"checks":{"len":"ok","no_eval":"ok","confirm":"ok"}}
{"type":"AI_SHOWING","invocation_id":"iv3"}
{"type":"AI_DONE","invocation_id":"iv3","shown_sec":21.0}
{"type":"AI_KILLED","invocation_id":"iv3"}
{"type":"HEALTH","asr":"ok","llm":"ok","lat":{"asr_p95":760,"llm_p95":2400}}
{"type":"SNAPSHOT","state":"...","recent":[...]}
```

客户端 → 服务端：仅 `PING` 与控制台的 `SET_FOCUS`（其余走 REST，便于审计与幂等）。

### 4.3 Provider 抽象接口

```python
class AsrProvider(Protocol):
    def stream(self, on_online, on_offline) -> "StreamSession": ...   # 长连接会话
    def health(self) -> bool: ...

class StreamSession(Protocol):
    def send_audio(self, pcm: bytes) -> None: ...
    def mark_end(self) -> None: ...    # 发 {"is_speaking": false}
    def close(self) -> None: ...

class LlmProvider(Protocol):
    def stream_chat(self, messages, *, max_tokens, temperature) -> Iterator[str]: ...
```

---

## 5. 目录结构与技术栈

```
translation_seat/
├── README.md
├── requirements.txt
├── .env.example                  # ASR_PROTOCOL / FUNASR_WS_URL / ASR_LANGUAGE / LLM_* / 设备与开关
├── run.sh                        # 一键启动：uvicorn app.main:app --host 127.0.0.1 --port 8081
├── hotwords_dict.txt             # 全局热词兜底（可留空，会前按科学家自动生成 session 热词）
├── app/
│   ├── main.py                   # FastAPI 装配、静态资源、WS 路由
│   ├── config.py                 # pydantic-settings 配置
│   ├── events.py                 # EventBus（线程安全广播）
│   ├── state.py                  # AI 状态机 + Kill Switch
│   ├── audio/
│   │   ├── capture.py            # sounddevice 采集 / 文件回放
│   │   ├── ringbuffer.py         # 120s PCM 环形缓冲
│   │   ├── vad.py                # webrtcvad 断句
│   │   └── pipeline.py           # 采集→VAD→ASR 主流水线线程
│   ├── asr/
│   │   ├── funasr_nano_ws.py     # Fun-ASR-Nano vLLM 流式客户端（START/STOP + PCM）
│   │   ├── funasr_ws.py          # 标准 FunASR 2pass 流式客户端（备选）
│   │   └── hotwords.py           # 热词解析（词<TAB>权重）与落盘
│   ├── context/
│   │   ├── store.py              # TranscriptStore（SQLite）
│   │   ├── manager.py            # 四块上下文组装与预算
│   │   ├── materials.py          # 会前资料加载（md/txt/pdf/pptx→文本）
│   │   ├── terms.py              # 术语候选打分
│   │   └── hotword_builder.py    # 科学家背景 → 热词 + 简介（LLM）
│   ├── agent/
│   │   ├── tasks.py              # 5 个 Task 定义
│   │   ├── prompts/*.md          # 提示词（学术校验人可直接审阅/修改）
│   │   ├── runner.py             # 生成 + 流式 + 重试
│   │   └── guard.py              # 后置校验（字数/评价词/确认句/时长）
│   ├── providers/
│   │   └── llm_openai.py         # taiji / OpenAI 兼容 LLM（含流式与非流式）
│   └── web/
│       ├── console.html/js/css
│       └── screen.html/js/css
├── sessions/<session_id>/
│   ├── meta.json                 # 报告人授权、是否启用 AI、术语表版本
│   ├── hotwords.txt              # session 专属热词（自动生成）
│   ├── materials/                # 摘要 / PPT 文本 / 简介(speaker_profile.md) / 术语表
│   ├── transcript.sqlite
│   └── export/                   # 复盘包
└── tools/
    ├── offline_replay.py         # 阶段 0：录音/文字稿灌入，批量测 5 个任务
    └── check_env.py              # 设备、Provider、延迟自检
```

**技术栈**：Python 3.11 + FastAPI + uvicorn + sounddevice + webrtcvad + websocket-client + httpx + SQLite；前端零框架（原生 ES Module + CSS，避免构建链，便于现场改）。

---

## 6. 数据设计与安全

### 6.1 数据模型（SQLite）

```
session(id, title, speaker_name, discipline, ai_enabled, consent_at, started_at, ended_at)
segment(id, session_id, t_start, t_end, track, speaker_hint, text, revised_text, is_final)
invocation(id, session_id, task, target, prompt_hash, output_text, checks_json,
           status /*generated|shown|discarded|killed*/, gen_ms, shown_sec, created_at)
confirmation(invocation_id, confirmed_by /*speaker|expert*/, verdict /*ok|corrected|rejected*/, note)
metric(session_id, k, v, at)          -- 延迟、失败率等
```

- `confirmation` 是需求 C5 的落地：**只有 verdict=ok/corrected 的 invocation 才允许进入复盘沉淀**。

### 6.2 安全与合规实现

| 需求条款 | 实现 |
|---|---|
| 会前告知与可关闭 | `session.ai_enabled=false` 时系统只做转写不生成不展示；大屏显示"本场未启用 AI" |
| 原始音频不长期保存 | 默认仅内存 RingBuffer；`KEEP_AUDIO=true` 才落盘到 `sessions/<id>/audio/`，并写入保留期限，`tools/purge.py` 到期删除 |
| 输出不自动成纪要 | 导出包默认只含 `confirmation.verdict != null` 的条目；未确认条目单独标注"未确认，不得引用" |
| 未发表数据处理 | 全部 Provider endpoint 可配为内网/本地；启动时打印"当前数据出网范围"清单并要求确认 |
| 公开展示标注 | 大屏固定角标 + AI 输出模板强制确认句 |
| 日志最小化 | 日志只记 `seg_id/耗时/状态码/字数`，**不记转写正文与模型输入**（`LOG_LEVEL=debug` 才记，且仅写本地） |
| 本机绑定 | 服务默认 `127.0.0.1`；大屏在同机扩展屏打开。若必须跨机投屏，走局域网 + Token + 只读 `/screen` |

### 6.3 复盘导出包

`export/` 含：`transcript.md`（含修正标记）、`invocations.csv`（任务/输出/状态/延迟/确认结论）、`metrics.json`（延迟 p50/p95、调用次数、展示时长）、`connections.md`（经确认的跨学科连接点清单）。直接对应需求"十一、试点评估指标"的采集需要。

---

## 7. 延迟预算与性能

以"主持人口令 → 大屏显示 AI 输出"为端到端目标（需求要求 ≤ 8 s；本版无 TTS，更宽裕）：

| 环节 | 预算(p50) | 预算(p95) | 说明 |
|---|---|---|---|
| 操作员点击（口令已说完） | 0.5 s | 1.0 s | 快捷键 `1..5` |
| 上下文组装 | 30 ms | 80 ms | 全内存 |
| LLM 首 token | 0.6 s | 1.2 s | 流式，操作员可提前读 |
| LLM 完整输出 | 2.0 s | 3.5 s | ≤ 300 tokens |
| 后置校验 | 20 ms | 50 ms | 纯本地规则 |
| 操作员确认 | 1.0 s | 2.0 s | 边生成边读 |
| 投屏推送 + 渲染 | 50 ms | 100 ms | WS 广播 |
| **合计** | **≈4.2 s** | **≈7.9 s** | 满足 8s |

p95 超标应对：① `BRIDGE`/`SUMMARY` 支持**预生成**（问答 8 分钟时后台先生成候选，12 分钟按需直接展示，端到端降至 ~2 s）；② LLM 6 s 硬超时并提示重试。

另需保障的实时性指标：

- 字幕上屏延迟（partial 结果）：≤ 1.5 s（服务端 `decode-interval` 0.48s + 网络 + 模型推理）；
- 终稿延迟（confirmed 句子）：句尾静音（动态 VAD 约 2s）后 ≤ 1.0 s 返回。

---

## 8. 可靠性与降级

| 故障 | 检测 | 降级动作 | 会议影响 |
|---|---|---|---|
| ASR 服务不可用 | `/health` 探测 + WS 连接失败 | 大屏字幕区显示"转写暂停"，AI 任务按钮置灰（无上下文不生成）；自动切备用 endpoint | 会议正常，AI 功能暂停 |
| ASR 流断开 | WS 异常/心跳超时 | 指数退避自动重连，重连后重新握手 | 短暂缺字 |
| LLM 超时/异常 | 6 s 超时 | 控制台提示重试；一键切备用模型 | 该次调用取消 |
| 声卡掉线 | PortAudio 回调异常 | 自动重开流 + 红条告警 + 提示切换设备 | 短暂中断 |
| 应用崩溃 | 进程退出 | `run.sh` 自动重启；SQLite WAL 保证转写不丢；重启后 5 s 内恢复采集 | 短暂中断 |
| 大屏浏览器断连 | WS 心跳 | 自动重连 + `SNAPSHOT` 补齐 | 无感 |

**现场兜底原则**：任何 AI 组件故障都不得影响会议进行；最差情况下系统退化为"一块只显示字幕的屏"，操作员可一键 `AI_DISABLE` 完全隐藏 AI 区域。

---

## 9. 与需求实施阶段的映射

| 需求阶段 | 本架构交付物 | 验收标准 |
|---|---|---|
| 阶段 0 离线模拟 | `tools/offline_replay.py` + `agent/` + `providers/` | 用过往录音/文字稿跑通 5 个任务；研究员评估准确性/有用性/误导风险 |
| 阶段 1 半自动现场试点 | `audio/` + `asr/` + `/console` + `/screen` + `DisplayController` | 麦克风实时转写、按钮触发、预览确认后投屏展示、端到端 p50 ≤ 5 s |
| 阶段 2 口令自动触发 | `WakewordMatcher`（开启 `WAKEWORD_AUTO`） | 固定口令识别准确率 ≥ 95%，Kill Switch 始终有效 |
| 阶段 3 session 级扩展 | `context/materials.py` + 导读卡/术语表生成脚本 + 导出包 | 会前产出导读卡与术语表，会后产出经确认的连接线索清单 |

对应需求"十三、建议时间表"：第 2–3 周完成阶段 0（离线原型），第 4 周完成阶段 1（半自动现场原型），第 5 周彩排调优提示词与术语表，第 6 周现场试点并产出复盘包。

---

## 10. 风险对照（架构层控制）

| 需求风险 | 架构控制点 |
|---|---|
| AI 解释不准确 | 确认句强制校验 + `confirmation` 表 + 大屏"待确认"角标 + 学术校验人可直接改 `agent/prompts/*.md` |
| AI 抢占会议节奏 | 字数窗口 + 口播时长预估拦截 + 控制台调用次数提醒 + 默认静默 |
| ASR 识别错误 | session 专属热词（科学家背景自动生成）+ 转写行内修正 + LLM 只读修正文本 |
| 现场延迟过高 | §7 预算 + 预生成 + 流式边生成边读 |
| 学术隐私风险 | 内存音频、Provider 可内网化、启动时出网范围确认、日志最小化 |
| 现场接受度不足 | 大屏用词固定为"AI 跨学科翻译尝试"、不出现评分/评价类 UI、评价词表硬拦截 |

---

## 11. 待确认事项（需求方决策）

1. **LLM 选型**：已确定 taiji `hy3`（`https://ml-serv.ssv.qq.com/tencent/llm/taiji`，思考模型，返回 `reasoning_content`）。若涉及未发表内容需私有化，仍可通过 `LLM_BASE/LLM_CHAT_PATH` 切换内网/本地模型。
2. **FunASR 流式端点**：已确认 `wss://ml-serv.ssv.qq.com/s2/ws`（Fun-ASR-Nano vLLM 协议），可用 `FUNASR_WS_URL` 覆盖。
3. **音频源接入方式**：会场调音台是否可提供 line-out 给采集机（首选），还是只能用独立麦克风拾音。
4. **是否分轨采集**：主持人麦单独一路可显著提升口令识别与角色标注，需会务/音响配合。
5. **术语表来源**：已支持科学家背景自动生成热词；若会前能拿到报告摘要与 PPT，可放入 `sessions/<id>/materials/` 进一步提升 STATIC 上下文质量。
6. **大屏位置与内容**：AI 卡片是否与报告 PPT 共屏（需第二块屏），或仅在问答环节切到 AI 屏。
7. **音频留存策略**：是否需要留存录音用于会后评估；若需，保留期限与授权流程。
