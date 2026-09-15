"""FastAPI 装配：REST 控制接口 + WebSocket 广播 + 音频流水线 + 静态页面。

【已封板 deprecated】本模块属 Python 版实现，自 2026-09 起不再新增功能，代码保留作
对照与应急回退（Go 版出现疑难问题时切回来比对）。新改动只进 Go 版（go/ 目录，
即 Dockerfile 构建的默认部署版本）。
"""
from __future__ import annotations

import asyncio
import itertools
import json
import threading
import time
from pathlib import Path
from typing import Optional

from fastapi import FastAPI, File, Request, UploadFile, WebSocket, WebSocketDisconnect
from fastapi.responses import FileResponse, JSONResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel

from app.agent.runner import AgentRunner
from app.asr.funasr_nano_ws import FunAsrNanoStreamClient
from app.asr.funasr_ws import FunAsrStreamClient
from app.asr.hy_stream import HyAsrStreamClient
from app.asr.qwen3_http import Qwen3AsrHttpClient
from app.asr.hotwords import load_hotwords, save_hotwords, to_stream_json
from app.asr.refine import TranscriptRefiner
from app.audio.capture import BrowserCapture, FileReplayCapture, MicrophoneCapture
from app.audio.pipeline import AudioPipeline
from app.audio.ringbuffer import RingBuffer
from app.audio.vad import VadSegmenter
from app.config import BASE_DIR, get_settings
from app.context import document, hotword_builder
from app.context.manager import ContextManager
from app.context.terms import score_text
from app.context.store import Store
from app.events import bus
from app.state import display

WEB_DIR = BASE_DIR / "app" / "web"


class Runtime:
    """进程级单例运行时。"""

    def __init__(self) -> None:
        self.settings = get_settings()
        self.store = Store(self.settings.data_dir_path / "transcript.sqlite")
        self.session_id: str | None = None
        self.context: ContextManager | None = None
        self.agent: AgentRunner | None = None
        self.pipeline: AudioPipeline | None = None
        self.capture = None
        self.capture_mode: str = "mic"  # mic（本机声卡）/ browser（远端浏览器收音）
        self.asr_client = None
        self.ring = RingBuffer(seconds=120, sample_rate=self.settings.sample_rate)
        self.base_glossary: dict[str, int] = load_hotwords(self.settings.hotwords_file)
        self.glossary: dict[str, int] = dict(self.base_glossary)
        self.session_hotwords: dict[str, int] = {}
        self.hotwords_enabled: bool = True  # 热词是否参与 ASR 调用（面板开关）
        self.last_output: dict = {}
        self._lock = threading.Lock()
        self._seg_counter = itertools.count(1)
        # 句子键（r{轮次}i{序号}）→ seg_id 映射，用于服务端回退修正定位
        self._round_segs: dict[str, str] = {}
        self.refiner: TranscriptRefiner | None = None
        # 目标学科：把 AI 输出翻译到该学科的表达层次。默认「白话」=非专业听众也能听懂。
        # 注意与 session.discipline（报告人学科，仅用于热词生成）是两个不同概念。
        self.target_discipline: str = "白话"

    # ---- 目标学科 ----
    def set_target_discipline(self, value: str) -> str:
        """设置目标学科（下一次 AI 调用即生效），返回归一化后的值。"""
        v = (value or "").strip() or "白话"
        self.target_discipline = v
        return v

    # ---- 热词管理 ----
    def set_session_hotwords(self, words: dict[str, int]) -> None:
        """设置 session 级热词（科学家专属），并合并进术语打分词表。"""
        self.session_hotwords = dict(words or {})
        self.glossary = {**self.base_glossary, **self.session_hotwords}

    def active_hotwords(self) -> dict[str, int]:
        """ASR 实际使用的热词：优先 session 级，否则全局；总开关关闭则返回空。"""
        if not self.hotwords_enabled:
            return {}
        return self.session_hotwords if self.session_hotwords else self.base_glossary

    # ---- 转写更新（回退修改 / LLM 改写共用）----
    def apply_revision(self, seg_id: str, text: str, source: str) -> None:
        """更新某句转写文本：落库 + 推送前端覆盖显示。

        source: "asr"（服务端回退修正）/ "llm"（顺句改写）
        """
        text = (text or "").strip()
        if not text:
            return
        try:
            self.store.revise_segment(seg_id, text)
        except Exception as e:  # noqa: BLE001 —— 落库失败仍推送显示
            print(f"[store] revise_segment failed: {e}")
        bus.publish({"type": "TRANSCRIPT_REVISED", "seg_id": seg_id,
                     "text": text, "source": source})

    # ---- ASR 回调 ----
    def make_handlers(self):
        def on_online(text: str) -> None:
            if not text:
                return
            bus.publish({"type": "TRANSCRIPT_PARTIAL", "text": text})

        def on_offline(text: str) -> None:
            sid = self.session_id
            if not sid:
                return
            text = text.strip()
            if not text:
                return
            seq = next(self._seg_counter)
            t_end = time.time()
            t_start = t_end - min(60.0, len(text) * 0.3)
            try:
                seg_id = self.store.add_segment(sid, seq, t_start, t_end, text)
            except Exception as e:  # noqa: BLE001 —— 落库失败不炸 ASR 回调线程，转写照常投屏
                print(f"[store] add_segment failed: {e}")
                seg_id = f"{sid}-s{seq:05d}"
            # 记录句子键 → seg_id，供服务端回退修正定位（键含会话轮次，跨轮不串）
            key = getattr(self.asr_client, "last_sentence_key", "")
            if key:
                self._round_segs[key] = seg_id
                # 只保留最近 200 句映射，避免长会议内存增长
                if len(self._round_segs) > 200:
                    for k in list(self._round_segs)[:100]:
                        del self._round_segs[k]
            bus.publish({
                "type": "TRANSCRIPT_FINAL", "seg_id": seg_id, "text": text,
                "t_start": t_start, "t_end": t_end,
            })
            items = score_text(text, self.glossary)
            if items:
                bus.publish({"type": "TERM_CANDIDATES", "items": items, "seg_id": seg_id})
            # 异步 LLM 顺句改写（失败/超时静默降级，不影响已显示的原文）
            if self.refiner is not None:
                self.refiner.submit(seg_id, text, self.glossary)

        def on_revise(key: str, text: str) -> None:
            """服务端修正了本轮已确认的句子（回退更新）。"""
            seg_id = self._round_segs.get(key)
            if seg_id:
                self.apply_revision(seg_id, text, source="asr")

        return on_online, on_offline, on_revise

    def start_pipeline(self, replay_path: Optional[str] = None,
                       capture_mode: str = "mic") -> dict:
        """启动采集→VAD→ASR 流水线。

        capture_mode: mic（本机声卡/USB 麦）/ browser（远端浏览器收音，前端经 /ws/audio 推 PCM）。
        返回状态描述。
        """
        if self.pipeline is not None and self.pipeline.is_alive():
            if self.capture_mode == capture_mode:
                return {"ok": True, "msg": "already running", "capture": self.capture_mode}
            # 请求切换音源（本机麦 ↔ 浏览器收音）：停掉旧流水线，按新音源重启
            self.stop_pipeline()

        s = self.settings
        on_online, on_offline, on_revise = self.make_handlers()
        self._round_segs = {}
        if s.asr_protocol == "hy_stream":
            # HY-ASR-3-Stream：逐词增量 + 语义 VAD 自动断句（无需本地切句）
            asr = HyAsrStreamClient(
                s.hy_asr_ws_url,
                token=s.hy_asr_token,
                model=s.hy_asr_model,
                hotwords=list(self.active_hotwords().keys()),
                on_online=on_online,
                on_offline=on_offline,
                on_status=lambda st: bus.publish({"type": "ASR_STATUS", "status": st}),
                on_revise=on_revise,
            )
            server_vad = True
        elif s.asr_protocol == "funasr_nano":
            hotword_list = list(self.active_hotwords().keys())
            asr = FunAsrNanoStreamClient(
                s.ws_url,
                language=s.asr_language,
                hotwords=hotword_list,
                on_online=on_online,
                on_offline=on_offline,
                on_status=lambda st: bus.publish({"type": "ASR_STATUS", "status": st}),
                on_revise=on_revise,
            )
            # 服务端 VAD 断句过粗（实测 10~20s 才出一句），改用本地 VAD 主动切句：
            # 句尾静音即发 STOP 让服务端立刻 flush，出字延迟降到约 0.8s
            server_vad = not s.local_vad_segment
        elif s.asr_protocol == "qwen3_http":
            # Qwen3-ASR HTTP 客户端：音频全量透传，VAD 切句与草稿调度在客户端类内部
            # （每 asr_partial_interval 秒整段重推出草稿，句尾静音出确认句，实测前缀稳定）
            hotword_list = list(self.active_hotwords().keys())
            asr = Qwen3AsrHttpClient(
                s.qwen3_backend,
                model=s.qwen3_model,
                language=s.asr_language,
                hotwords=hotword_list,
                partial_interval_s=s.asr_partial_interval,
                # 推理超时用 asr_infer_timeout 而非 llm_timeout：两者合理值不同，
                # 且服务端排队抖动时 ASR 需要更宽的容忍度（实测 0.8s~60s）。
                infer_timeout_s=s.asr_infer_timeout,
                on_online=on_online,
                on_offline=on_offline,
                on_status=lambda st: bus.publish({"type": "ASR_STATUS", "status": st}),
                on_revise=on_revise,
            )
            server_vad = True  # 持续透传，切句由 Qwen3AsrHttpClient 自管
        else:
            asr = FunAsrStreamClient(
                s.ws_url,
                mode=s.asr_mode,
                chunk_size=s.chunk_size,
                hotwords=to_stream_json(self.active_hotwords()),
                audio_fs=s.sample_rate,
                on_online=on_online,
                on_offline=on_offline,
                on_status=lambda st: bus.publish({"type": "ASR_STATUS", "status": st}),
            )
            server_vad = False

        vad = None
        if not server_vad:
            vad = VadSegmenter(
                s.sample_rate, s.frame_ms, s.vad_silence_ms, s.vad_aggressiveness, s.max_segment_s
            )

        note = ""
        try:
            if replay_path:
                capture = FileReplayCapture(replay_path, s.sample_rate, s.frame_ms)
            elif capture_mode == "browser":
                capture = BrowserCapture(sample_rate=s.sample_rate, block_ms=s.frame_ms)
            else:
                capture = MicrophoneCapture(
                    sample_rate=s.sample_rate, block_ms=s.frame_ms
                )
                capture.start()
        except Exception as e:  # 无音频设备等
            if replay_path:
                return {"ok": False, "msg": f"音频采集失败: {e}"}
            # 远端/服务器部署常无声卡：自动降级为浏览器收音
            capture = BrowserCapture(sample_rate=s.sample_rate, block_ms=s.frame_ms)
            capture_mode = "browser"
            note = f"本机音频采集失败（{e}），已切换为浏览器收音"

        # LLM 顺句改写器（可关闭；失败静默降级，不影响原始转写）
        if s.refine_enabled and self.agent is not None and self.agent.llm is not None:
            self.refiner = TranscriptRefiner(
                self.agent.llm,
                on_refined=lambda seg_id, text: self.apply_revision(seg_id, text, source="llm"),
                min_chars=s.refine_min_chars,
            )
            self.refiner.start()
        else:
            self.refiner = None

        self.capture = capture
        self.capture_mode = capture_mode
        self.asr_client = asr
        self.pipeline = AudioPipeline(capture, vad, asr, self.ring, server_vad=server_vad)
        self.pipeline.start()
        res = {"ok": True, "msg": "started", "capture": capture_mode}
        if note:
            res["note"] = note
        return res

    def stop_pipeline(self) -> None:
        if self.pipeline is not None:
            self.pipeline.stop()
            self.pipeline = None
        self.capture = None
        self.capture_mode = "mic"
        self.asr_client = None
        if self.refiner is not None:
            self.refiner.stop()
            self.refiner = None


runtime = Runtime()
app = FastAPI(title="AI 跨学科实时翻译席")
app.mount("/static", StaticFiles(directory=WEB_DIR), name="static")


# ===================== 页面 =====================
@app.get("/")
async def root(request: Request):
    # 挂在路径前缀下时（Kong base path + strip_path），后端看到的是去掉前缀的路径，
    # 此时直接跳 /console 会让浏览器丢掉前缀、落到网关的其它路由上。
    # 前缀由网关通过 X-Forwarded-Prefix 告知（未配则为空，行为不变）。
    prefix = (request.headers.get("x-forwarded-prefix") or "").rstrip("/")
    return RedirectResponse(prefix + "/console")


@app.get("/console")
async def console_page():
    return FileResponse(WEB_DIR / "console.html")


@app.get("/screen")
async def screen_page():
    return FileResponse(WEB_DIR / "screen.html")


# ===================== REST =====================
class SessionIn(BaseModel):
    title: str = "Workshop"
    speaker: str = ""
    institution: str = ""
    discipline: str = ""
    ai_enabled: bool = True
    replay_path: Optional[str] = None
    capture: str = "mic"  # mic（本机声卡）/ browser（远端浏览器收音）
    target_discipline: str = "白话"  # 目标学科：把 AI 输出翻译到该学科的表达层次


@app.post("/api/session")
async def create_session(body: SessionIn):
    sid = runtime.store.create_session(body.title, body.speaker, body.discipline,
                                       body.ai_enabled, body.institution)
    runtime.session_id = sid
    # 新 session 从表单取值；未传则回到默认「白话」
    runtime.set_target_discipline(body.target_discipline)

    # 每个 session 独立目录：materials 存报告背景等资料
    session_dir = runtime.settings.data_dir_path / sid
    materials_dir = session_dir / "materials"
    materials_dir.mkdir(parents=True, exist_ok=True)
    runtime.context = ContextManager(runtime.store, materials_dir)
    runtime.agent = AgentRunner(runtime.settings, runtime.context)

    # 每次点「开始 Session」都重新加载全局热词文件，改动即时生效（无需重启）
    runtime.base_glossary = load_hotwords(runtime.settings.hotwords_file)
    runtime.set_session_hotwords({})  # 先清空；科学家热词生成完成后异步应用

    # 立即启动流水线（先用全局热词），不阻塞等热词生成 —— 点开始马上有转写
    res = runtime.start_pipeline(body.replay_path, body.capture)
    bus.publish({"type": "SESSION", "session_id": sid, "title": body.title,
                 "ai_enabled": body.ai_enabled, "speaker": body.speaker,
                 "hotwords": 0})

    # 热词后台异步生成：完成后无缝切换 ASR 热词（仅重连 ASR 连接，不中断流水线）
    if body.speaker and runtime.agent is not None and runtime.agent.llm is not None:
        asyncio.create_task(_generate_hotwords_bg(
            sid, session_dir, materials_dir,
            body.speaker, body.institution, body.discipline,
        ))
    else:
        bus.publish({"type": "HOTWORDS_STATUS",
                     "status": "skipped:no_speaker" if not body.speaker else "skipped:llm_unavailable"})

    return {"session_id": sid, "pipeline": res, "hotwords": None, "profile": ""}


async def _generate_hotwords_bg(sid: str, session_dir: Path, materials_dir: Path,
                                speaker: str, institution: str, discipline: str) -> None:
    """后台生成科学家专属热词；完成后应用并触发 ASR 重连使热词即时生效。"""
    bus.publish({"type": "HOTWORDS_STATUS", "status": "generating", "speaker": speaker})
    try:
        result = await asyncio.to_thread(
            hotword_builder.build_speaker_profile,
            runtime.agent.llm, speaker, institution, discipline,
        )
    except Exception as e:  # noqa: BLE001 —— 生成失败降级，不影响开会
        bus.publish({"type": "HOTWORDS_STATUS", "status": f"failed:{e}", "speaker": speaker})
        return

    # 用户已切换到新 session：丢弃结果，避免热词串场
    if runtime.session_id != sid:
        return

    hotwords = result["hotwords"]
    profile = result["profile"]
    if hotwords:
        save_hotwords(session_dir / "hotwords.txt", hotwords)
    if profile:
        (materials_dir / "speaker_profile.md").write_text(
            f"# 报告人背景\n\n{profile}\n", encoding="utf-8"
        )
        # 让 STATIC 上下文重新加载，报告人背景才能进入 LLM 提示词
        if runtime.context is not None:
            runtime.context.invalidate_static()
    runtime.set_session_hotwords(hotwords)
    bus.publish({"type": "HOTWORDS_STATUS", "status": f"generated:{len(hotwords)}",
                 "speaker": speaker})
    # 触发 ASR 重连应用新热词（采集/转写流水线不中断）
    if runtime.asr_client is not None:
        runtime.asr_client.update_hotwords(runtime.active_hotwords())


@app.post("/api/session/{sid}/start")
async def start_session(sid: str, body: Optional[SessionIn] = None):
    runtime.session_id = sid
    res = runtime.start_pipeline(None, body.capture if body else "mic")
    return res


@app.post("/api/session/{sid}/stop")
async def stop_session(sid: str):
    runtime.stop_pipeline()
    runtime.store.set_ended(sid)
    return {"ok": True}


class HotwordIn(BaseModel):
    name: str
    institution: str = ""
    discipline: str = ""


class HotwordsTextIn(BaseModel):
    text: str  # 面板编辑内容：每行一个词，可附权重（词<TAB>60 或 词,60）


class HotwordsToggleIn(BaseModel):
    enabled: bool


def _push_hotwords() -> None:
    """把当前生效热词推送到运行中的 ASR 客户端（即时生效，不中断流水线）。"""
    if runtime.asr_client is not None:
        runtime.asr_client.update_hotwords(runtime.active_hotwords())


@app.get("/api/hotwords")
async def get_hotwords():
    """热词面板数据：当前生效词表（session 级优先，否则全局）+ 参与开关。"""
    words = runtime.session_hotwords if runtime.session_hotwords else runtime.base_glossary
    return {"words": words, "enabled": runtime.hotwords_enabled}


@app.put("/api/hotwords")
async def put_hotwords(body: HotwordsTextIn):
    """手动编辑保存：面板全文作为 session 级热词（所见即所得），已存在的词保留原权重。"""
    old = runtime.session_hotwords or {}
    new: dict[str, int] = {}
    for line in body.text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = [p.strip() for p in line.replace(",", "\t").split("\t") if p.strip()]
        if not parts:
            continue
        w, weight = parts[0], old.get(parts[0], 60)
        if len(parts) >= 2:
            try:
                weight = max(1, min(100, int(float(parts[1]))))
            except ValueError:
                pass
        new[w] = weight
    runtime.set_session_hotwords(new)
    _push_hotwords()
    return {"ok": True, "count": len(new)}


@app.post("/api/hotwords/toggle")
async def toggle_hotwords(body: HotwordsToggleIn):
    """开始/停止热词参与 ASR 调用；停止时立即推送空热词。"""
    runtime.hotwords_enabled = body.enabled
    _push_hotwords()
    return {"ok": True, "enabled": body.enabled}


@app.post("/api/hotwords/generate")
async def generate_hotwords(body: HotwordIn):
    """手动触发：根据科学家姓名+机构，生成研究背景与专属热词。"""
    if runtime.agent is None or runtime.agent.llm is None:
        return JSONResponse({"error": "LLM 未配置"}, status_code=400)
    if not runtime.session_id:
        return JSONResponse({"error": "请先创建 session"}, status_code=400)
    try:
        result = await asyncio.to_thread(
            hotword_builder.build_speaker_profile,
            runtime.agent.llm, body.name, body.institution, body.discipline,
        )
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": str(e)}, status_code=500)

    hotwords = result["hotwords"]
    profile = result["profile"]
    session_dir = runtime.settings.data_dir_path / runtime.session_id
    materials_dir = session_dir / "materials"
    materials_dir.mkdir(parents=True, exist_ok=True)
    if hotwords:
        save_hotwords(session_dir / "hotwords.txt", hotwords)
    if profile:
        (materials_dir / "speaker_profile.md").write_text(
            f"# 报告人背景\n\n{profile}\n", encoding="utf-8"
        )
        # 让 STATIC 上下文重新加载，报告人背景才能进入 LLM 提示词
        if runtime.context is not None:
            runtime.context.invalidate_static()
    runtime.set_session_hotwords(hotwords)
    # 热词已变：只重连 ASR 连接即可生效，不中断采集/转写流水线
    if runtime.asr_client is not None:
        runtime.asr_client.update_hotwords(runtime.active_hotwords())
        applied = "asr_reconnect"
    else:
        applied = runtime.start_pipeline()
    bus.publish({"type": "HOTWORDS_STATUS", "status": f"generated:{len(hotwords)}"})
    return {"hotwords": hotwords, "fields": result["fields"], "profile": profile,
            "applied": applied}


@app.post("/api/materials/upload")
async def upload_material(file: UploadFile = File(...)):
    """上传演示稿：解析正文 → 抽热词（即时用于 ASR）+ 生成浓缩摘要（AI 背景）。

    摘要落成 materials/<名>.md，load_materials 会自动读它作为每次 AI 调用的
    STATIC 上下文 —— 因此上传讲稿后，所有 AI 翻译都能引用讲稿内容。
    解析原文另存 uploads/，仅供追溯，不进上下文。
    """
    if runtime.agent is None or runtime.agent.llm is None:
        return JSONResponse({"error": "LLM 未配置"}, status_code=400)
    if not runtime.session_id:
        return JSONResponse({"error": "请先创建 session"}, status_code=400)

    raw = await file.read()
    if not raw:
        return JSONResponse({"error": "文件为空"}, status_code=400)
    # 只取 basename，防止用 ../ 之类构造路径穿越
    filename = Path(file.filename or "document").name
    if not filename or filename == ".":
        filename = "document"

    settings = runtime.settings
    # 体积预检：网关对请求体有 2 MiB 上限，超出会被静默截断，
    # 上游只回一个看不懂的 multipart 解析错误。这里提前拦住。
    try:
        document.check_upload_size(len(raw), settings.doc_max_bytes)
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": str(e)}, status_code=413)

    try:
        doc = await asyncio.to_thread(
            document.parse_document, settings.doc_parse_url, filename, raw
        )
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": f"文档解析失败：{e}"}, status_code=502)

    try:
        profile = await asyncio.to_thread(
            document.build_from_document, runtime.agent.llm,
            doc["content"], settings.doc_digest_chars,
        )
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": f"热词抽取失败：{e}"}, status_code=500)

    session_dir = settings.data_dir_path / runtime.session_id
    materials_dir = session_dir / "materials"
    materials_dir.mkdir(parents=True, exist_ok=True)
    stem = Path(filename).stem or "document"

    if profile["digest"]:
        (materials_dir / f"{stem}.md").write_text(
            f"# 讲稿摘要：{stem}\n\n{profile['digest']}\n", encoding="utf-8"
        )
    uploads_dir = session_dir / "uploads"
    uploads_dir.mkdir(parents=True, exist_ok=True)
    (uploads_dir / f"{stem}.txt").write_text(doc["content"], encoding="utf-8")

    # 热词与已有 session 热词合并（可连续上传多份资料），同名词以新权重覆盖
    merged = dict(runtime.session_hotwords)
    added = 0
    for word, weight in profile["hotwords"].items():
        if word not in merged:
            added += 1
        merged[word] = weight
    if profile["hotwords"]:
        save_hotwords(session_dir / "hotwords.txt", merged)
    runtime.set_session_hotwords(merged)

    # 摘要改变了 STATIC 上下文，让上下文缓存失效
    if runtime.context is not None:
        runtime.context.invalidate_static()

    # 热词已变：只重连 ASR 连接即可生效，不中断采集/转写流水线
    if runtime.asr_client is not None:
        runtime.asr_client.update_hotwords(runtime.active_hotwords())
        applied = "asr_reconnect"
    else:
        applied = runtime.start_pipeline()
    bus.publish({"type": "HOTWORDS_STATUS", "status": f"generated:{len(profile['hotwords'])}"})

    return {
        "ok": True,
        "filename": doc["filename"],
        "parsed_chars": len(doc["content"]),
        "truncated": doc["truncated"],
        "digest_chars": len(profile["digest"]),
        "fields": profile["fields"],
        "hotwords": len(profile["hotwords"]),
        "hotwords_new": added,
        "hotwords_total": len(merged),
        "applied": applied,
    }


@app.get("/api/materials")
async def list_materials():
    """列出当前 session 已加载的资料（materials/ 目录）。"""
    if not runtime.session_id:
        return {"items": []}
    materials_dir = runtime.settings.data_dir_path / runtime.session_id / "materials"
    if not materials_dir.exists():
        return {"items": []}
    items = [
        {"name": f.name, "chars": f.stat().st_size}
        for f in sorted(materials_dir.iterdir())
        if f.is_file()
    ]
    return {"items": items}


class TargetDisciplineIn(BaseModel):
    discipline: str = "白话"


@app.get("/api/target-discipline")
async def get_target_discipline():
    """当前目标学科（页面加载时回填下拉框）。"""
    return {"discipline": runtime.target_discipline}


@app.post("/api/target-discipline")
async def set_target_discipline(body: TargetDisciplineIn):
    """设置目标学科（下一次 AI 调用即生效，无需重启 session）。"""
    return {"ok": True, "discipline": runtime.set_target_discipline(body.discipline)}


@app.get("/api/devices")
async def list_devices():
    try:
        import sounddevice as sd

        return {"inputs": sd.query_devices(kind="input"), "devices": sd.query_devices()}
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": str(e)}, status_code=500)


class InvokeIn(BaseModel):
    task: str
    target: Optional[str] = None


@app.post("/api/invoke")
async def invoke(body: InvokeIn):
    if runtime.agent is None or not runtime.session_id:
        return JSONResponse({"error": "请先创建 session"}, status_code=400)
    focus = runtime.store.recent_segments(runtime.session_id, seconds=60)
    # 可中断的生成：把 LLM 客户端的 abort 注册到状态机，急停/丢弃即可立刻中断
    # 本次生成（含 hy3 只流思考内容、没有正文增量的阶段）
    llm_client = runtime.agent.llm
    display.set_cancel(llm_client.abort if llm_client is not None else None)
    try:
        iid = await asyncio.to_thread(
            runtime.agent.generate, runtime.session_id, body.task, body.target, focus,
            runtime.target_discipline,
        )
    except Exception as e:  # noqa: BLE001
        return JSONResponse({"error": str(e)}, status_code=500)
    finally:
        display.set_cancel(None)
    return {"invocation_id": iid}


class ReviseIn(BaseModel):
    text: str


class ShowIn(BaseModel):
    text: Optional[str] = None  # 操作员编辑后的文本（可选，投屏时以编辑版为准）


@app.post("/api/invocation/{iid}/show")
async def show_invocation(iid: str, body: ShowIn = None):
    display.show()
    text = None
    if body and body.text and body.text.strip():
        text = body.text.strip()
        runtime.store.revise_invocation(iid, text)  # 编辑版落库（复盘导出用编辑后文本）
    inv = runtime.store.get_invocation(iid)
    text = text or (inv["output_text"] if inv else "")
    task = inv["task"] if inv else ""
    runtime.store.set_status(iid, "shown")
    bus.publish({"type": "AI_SHOWING", "invocation_id": iid, "text": text, "task": task})
    return {"ok": True}


@app.patch("/api/invocation/{iid}")
async def revise_invocation(iid: str, body: ReviseIn):
    """编辑 AI 输出卡；若该卡正在投屏，大屏实时同步更新。"""
    runtime.store.revise_invocation(iid, body.text)
    if display.state.value == "SHOWING" and display.current == iid:
        inv = runtime.store.get_invocation(iid)
        bus.publish({"type": "AI_SHOWING", "invocation_id": iid, "text": body.text,
                     "task": inv["task"] if inv else ""})
    return {"ok": True}


@app.post("/api/invocation/{iid}/discard")
async def discard_invocation(iid: str):
    # 丢弃语义 = 不要这次输出：若它还在生成，一并中止（否则十几秒后 AI_READY 又会把卡片放回来，
    # 等于丢弃被撤销）。这里只置中止标记，随后用 clear() 清展示状态、保留标记给在飞的生成自行退出。
    if display.state.value == "GENERATING" and display.current == iid:
        display.kill()
    display.clear()
    runtime.store.set_status(iid, "discarded")
    bus.publish({"type": "AI_STATE", "state": "IDLE", "invocation_id": iid})
    return {"ok": True}


@app.post("/api/invocation/{iid}/confirm")
async def confirm_invocation(iid: str, body: dict):
    runtime.store.confirm(iid, body.get("by", "speaker"), body.get("verdict", "ok"), body.get("note", ""))
    return {"ok": True}


@app.post("/api/kill")
async def kill():
    display.kill()
    # 先发事件（此刻 current 还有值，大屏据此下屏），再清展示状态。
    # 注意用 clear() 而非 reset()：reset() 会清掉急停标志，那样正在飞的生成就不会中止了。
    bus.publish({"type": "AI_KILLED", "invocation_id": display.current or ""})
    display.clear()
    return {"ok": True}


@app.patch("/api/segment/{seg_id}")
async def revise_segment(seg_id: str, body: ReviseIn):
    runtime.store.revise_segment(seg_id, body.text)
    return {"ok": True}


@app.get("/api/health")
async def health():
    s = runtime.settings
    # ASR 状态直接读流水线真实连接（不另建探测连接，避免状态打架）
    if runtime.asr_client is not None:
        info = runtime.asr_client.status_info()
        asr_status = info["state"]
        asr_detail = info["detail"]
        asr_idle_sec = info["idle_sec"]
        asr_infer_fail = info.get("infer_fail", 0)
    else:
        asr_status = "idle"
        asr_detail = "流水线未启动（点「开始 Session」）"
        asr_idle_sec = -1
        asr_infer_fail = 0
    llm_status = "ok" if (s.llm_base and s.llm_model) else "not_configured"
    # 音源与浏览器收音连接状态
    mic_state = ""
    if isinstance(runtime.capture, BrowserCapture):
        mic_state = "connected" if runtime.capture.connected.is_set() else "waiting"
    capture_mode = runtime.capture_mode if runtime.pipeline is not None else ""
    return {
        "asr": asr_status,
        "asr_detail": asr_detail,
        "asr_idle_sec": asr_idle_sec,
        # 连续推理失败次数：用来区分「确实没语音」与「有语音但推理一直失败」——
        # 没有这个字段时两者都只表现为 asr_idle_sec 不断增长，从外部无法分辨。
        "asr_infer_fail": asr_infer_fail,
        "llm": llm_status,
        "llm_model": s.llm_model or "",
        "protocol": s.asr_protocol,
        "asr_ws_url": (s.hy_asr_ws_url if s.asr_protocol == "hy_stream"
                       else s.qwen3_backend if s.asr_protocol == "qwen3_http"
                       else s.ws_url),
        "capture": capture_mode,
        "mic": mic_state,
        "state": display.state.value,
    }


@app.get("/api/session/{sid}/export")
async def export_session(sid: str):
    segs = runtime.store.recent_segments(sid)
    md = ["# 转写记录\n"]
    for s in segs:
        t = s.get("revised_text") or s.get("text") or ""
        md.append(f"- {t}")
    return {"transcript_md": "\n".join(md), "segments": segs}


# ===================== WebSocket =====================
async def _ws_loop(websocket: WebSocket, screen_only: bool) -> None:
    await websocket.accept()
    q = bus.subscribe()
    recv_task = None
    ev_task = None
    try:
        # 连接后先补发快照（客户端可能已断开，需纳入 try 以保证 unsubscribe）
        await websocket.send_json({"type": "SNAPSHOT", "state": display.state.value,
                                   "session_id": runtime.session_id,
                                   "events": bus.snapshot()[-30:]})
        while True:
            # 同时监听客户端消息（PING/SET_FOCUS）与服务端事件
            recv_task = asyncio.ensure_future(websocket.receive_text())
            ev_task = asyncio.ensure_future(q.get())
            done, _ = await asyncio.wait({recv_task, ev_task}, return_when=asyncio.FIRST_COMPLETED)
            if recv_task in done:
                try:
                    msg = recv_task.result()
                    ev_task.cancel()
                    if msg == "PING":
                        continue
                except WebSocketDisconnect:
                    break
            if ev_task in done:
                ev = ev_task.result()
                if screen_only and ev.get("type") not in _SCREEN_TYPES:
                    continue
                await websocket.send_json(ev)
    except WebSocketDisconnect:
        pass  # 客户端正常断开，前端会自动重连
    finally:
        for t in (recv_task, ev_task):
            if t is not None and not t.done():
                t.cancel()
        bus.unsubscribe(q)


_SCREEN_TYPES = {
    "TRANSCRIPT_PARTIAL", "TRANSCRIPT_FINAL", "TRANSCRIPT_REVISED", "AI_STATE", "AI_DELTA",
    "AI_READY", "AI_SHOWING", "AI_DONE", "AI_KILLED", "HEALTH", "SNAPSHOT", "SESSION",
}


@app.websocket("/ws/console")
async def ws_console(websocket: WebSocket):
    await _ws_loop(websocket, screen_only=False)


@app.websocket("/ws/screen")
async def ws_screen(websocket: WebSocket):
    await _ws_loop(websocket, screen_only=True)


@app.websocket("/ws/audio")
async def ws_audio(websocket: WebSocket):
    """远端浏览器收音：前端推送 16k/mono/int16 PCM 二进制帧，喂入采集队列。"""
    await websocket.accept()
    try:
        while True:
            msg = await websocket.receive()
            if msg["type"] == "websocket.disconnect":
                break
            # 动态读取 runtime.capture：切换/重启 session 后音频不会喂到旧采集对象
            cap = runtime.capture
            if not isinstance(cap, BrowserCapture):
                await websocket.close(code=4001, reason="流水线未开启浏览器收音")
                break
            if not cap.connected.is_set():
                cap.mark_connected()
                bus.publish({"type": "MIC_STATUS", "status": "connected"})
            data = msg.get("bytes")
            if data:
                cap.feed(data)
    except WebSocketDisconnect:
        pass
    finally:
        cap = runtime.capture
        if isinstance(cap, BrowserCapture):
            cap.mark_disconnected()
            bus.publish({"type": "MIC_STATUS", "status": "disconnected"})


# ===================== 生命周期 =====================
@app.on_event("startup")
async def on_startup():
    bus.bind_loop(asyncio.get_running_loop())
    # Python 版已封板：不再新增功能，仅作对照与应急回退。生产部署请用 Go 版（Dockerfile）。
    print("[deprecated] 注意：Python 版已封板，仅用于对照排查；生产部署请使用 Go 版。", flush=True)


def main() -> None:
    import uvicorn

    s = runtime.settings
    uvicorn.run("app.main:app", host=s.host, port=s.port, reload=False)


if __name__ == "__main__":
    main()
