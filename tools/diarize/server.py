#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""CAM++ 说话人区分 sidecar（HTTP，纯标准库 + torch/funasr）。

职责：接收主程序（Go 版 seat）送来的「一段语音的 16k/mono/int16 PCM」，
用 CAM++ 提取说话人 embedding，并在 session 内做增量聚类，返回稳定的说话人编号
（S1/S2/...），供主程序把「说话人」对齐到每一句转写上。

为什么做成 sidecar：
  - CAM++ 是 PyTorch 模型，主程序是 Go（cgo 只带了 VAD/分词/数据库）；
  - 模型与推理依赖（funasr/modelscope/torch）体积大，不适合进主镜像，
    由 tools/diarize/run.sh 在**运行时**建 venv 拉依赖并下载模型（约 28MB）。

接口：
  GET  /health                      → {"ok":true,...}
  POST /assign?session=<sid>[&dir=<会话目录>][&threshold=0.5]
          body = 裸 PCM（16k/mono/int16 LE）
          → {"speaker":"S1","label":"说话人 1","confidence":0.71,"margin":0.08,
             "is_new":false,"seconds":3.2,"speaker_count":2}
  POST /reset?session=<sid>[&dir=...]  → 清空该 session 的说话人状态

运行（推荐用 run.sh 自动装依赖）：
  python3 tools/diarize/server.py --port 18901
  python3 tools/diarize/server.py --port 18901 --mock     # 无模型/无网环境的假实现（联调用）
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

MAX_BODY = 8 << 20          # 单次音频上限 8MB（16k/mono/int16 ≈ 4 分钟）
DEFAULT_THRESHOLD = 0.5     # CAM++ 余弦相似度阈值：>= 视为同一说话人
DEFAULT_MIN_MS = 600        # 过短的片段（<0.6s）embedding 不稳，直接跳过
DEFAULT_MAX_SECONDS = 12.0  # 单段取前 N 秒做 embedding（再长也不提升判别力，只增延迟）
MAX_SPEAKERS = 12           # 单场会议最多区分多少个说话人（超出则归到最接近的一位）

STATE_NAME = "speakers.json"


# --------------------------------------------------------------------------
# 说话人聚类（session 内增量）
# --------------------------------------------------------------------------
class Speaker:
    """一位说话人在本场会议中的声纹中心与统计。"""

    def __init__(self, sid: str, label: str, centroid, now: float):
        self.id = sid
        self.label = label
        self.centroid = centroid          # 单位向量（numpy）
        self.count = 0                    # 累计句数
        self.seconds = 0.0                # 累计语音时长
        self.first_at = now
        self.last_at = now

    def to_json(self) -> dict:
        return {
            "id": self.id,
            "label": self.label,
            "count": self.count,
            "seconds": round(self.seconds, 2),
            "first_at": round(self.first_at, 2),
            "last_at": round(self.last_at, 2),
            "centroid": [round(float(x), 6) for x in self.centroid],
        }

    @staticmethod
    def from_json(obj: dict) -> "Speaker":
        import numpy as np

        sp = Speaker(obj["id"], obj.get("label") or obj["id"],
                     np.asarray(obj["centroid"], dtype="float32"),
                     float(obj.get("first_at") or 0.0))
        sp.count = int(obj.get("count") or 0)
        sp.seconds = float(obj.get("seconds") or 0.0)
        sp.last_at = float(obj.get("last_at") or sp.first_at)
        return sp


class SessionState:
    """一个 session 的说话人表（含持久化）。"""

    def __init__(self, sid: str, threshold: float, state_path: str | None):
        self.sid = sid
        self.threshold = threshold
        self.path = state_path
        self.speakers: list[Speaker] = []
        self.lock = threading.Lock()
        self.dirty = False
        self._load()

    # ---- 持久化：掉线/重启后同一 session 的编号继续对齐 ----
    def _load(self):
        if not self.path or not os.path.isfile(self.path):
            return
        try:
            with open(self.path, "r", encoding="utf-8") as f:
                obj = json.load(f)
            for item in obj.get("speakers") or []:
                self.speakers.append(Speaker.from_json(item))
        except Exception as e:  # 坏文件不影响服务
            print(f"[diarize] 读取 {self.path} 失败（忽略）: {e}", flush=True)

    def save(self):
        if not self.path or not self.dirty:
            return
        try:
            os.makedirs(os.path.dirname(self.path), exist_ok=True)
            tmp = self.path + ".tmp"
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump({
                    "session": self.sid,
                    "threshold": self.threshold,
                    "saved_at": time.time(),
                    "speakers": [s.to_json() for s in self.speakers],
                }, f, ensure_ascii=False, indent=1)
            os.replace(tmp, self.path)
            self.dirty = False
        except Exception as e:
            print(f"[diarize] 写 {self.path} 失败（忽略）: {e}", flush=True)

    # ---- 核心：为一段音频分配说话人 ----
    def assign(self, emb, seconds: float) -> dict:
        """emb 为单位向量。返回说话人编号与置信度。"""
        import numpy as np

        now = time.time()
        with self.lock:
            best_i, best_sim, second_sim = -1, -1.0, -1.0
            for i, sp in enumerate(self.speakers):
                sim = float(np.dot(sp.centroid, emb))
                if sim > best_sim:
                    best_i, second_sim, best_sim = i, best_sim, sim
                elif sim > second_sim:
                    second_sim = sim

            if best_i >= 0 and best_sim >= self.threshold:
                sp = self.speakers[best_i]
                # 用时长加权的滑动平均更新中心（越长的片段越可信）
                w = max(0.2, seconds)
                new_c = sp.centroid * sp.seconds + emb * w
                norm = float(np.linalg.norm(new_c)) + 1e-9
                sp.centroid = new_c / norm
                is_new = False
            else:
                if len(self.speakers) >= MAX_SPEAKERS:
                    # 已到上限：归到最接近的一位，避免说话人编号无限增长
                    sp = self.speakers[best_i] if best_i >= 0 else self._new_speaker(emb, now)
                    is_new = False
                else:
                    sp = self._new_speaker(emb, now)
                    is_new = True
                best_sim = float(np.dot(sp.centroid, emb))

            sp.count += 1
            sp.seconds += seconds
            sp.last_at = now
            self.dirty = True
            return {
                "speaker": sp.id,
                "label": sp.label,
                "confidence": round(best_sim, 4),
                "margin": round(best_sim - max(second_sim, 0.0), 4),
                "is_new": is_new,
                "seconds": round(seconds, 2),
                "speaker_count": len(self.speakers),
            }

    def _new_speaker(self, emb, now: float) -> Speaker:
        sp = Speaker(f"S{len(self.speakers) + 1}", f"说话人 {len(self.speakers) + 1}", emb, now)
        # 中心初值用整段 embedding；首句可能偏短，后续会自然被平均修正
        self.speakers.append(sp)
        return sp

    def reset(self):
        with self.lock:
            self.speakers = []
            self.dirty = True
        if self.path and os.path.isfile(self.path):
            try:
                os.remove(self.path)
            except OSError:
                pass

    def roster(self) -> list[dict]:
        with self.lock:
            return [{
                "speaker": s.id, "label": s.label, "count": s.count,
                "seconds": round(s.seconds, 2),
            } for s in self.speakers]


# --------------------------------------------------------------------------
# 声纹提取
# --------------------------------------------------------------------------
class CampplusEmbedder:
    """CAM++ 声纹提取：fbank(80) → CAMPPlus.forward → 192 维单位向量。"""

    def __init__(self, model_id: str, device: str, max_seconds: float):
        import torch  # 延迟导入：--mock 模式无需 torch

        from funasr import AutoModel
        from funasr.utils import fbank as Kaldi

        self.torch = torch
        self.kaldi = Kaldi
        self.max_seconds = max_seconds
        self.device = device
        self.model_id = model_id

        t0 = time.time()
        auto = AutoModel(model=model_id, device=device, disable_update=True)
        net = getattr(auto, "model", None)
        if net is None or not hasattr(net, "forward"):
            raise RuntimeError("funasr AutoModel 未返回可用的 CAM++ 模型（版本不兼容？）")
        self.net = net
        self.net.eval()
        self.load_sec = time.time() - t0
        self.last_ms = 0.0

    def embed(self, pcm: bytes):
        """PCM(16k/mono/int16) → 192 维单位向量。"""
        import numpy as np

        wav = np.frombuffer(pcm, dtype="<i2").astype(np.float32) / 32768.0
        if self.max_seconds > 0 and len(wav) > self.max_seconds * 16000:
            wav = wav[: int(self.max_seconds * 16000)]
        torch = self.torch
        t0 = time.time()
        feat = self.kaldi.fbank(torch.from_numpy(wav).unsqueeze(0), num_mel_bins=80)
        feat = feat - feat.mean(dim=0, keepdim=True)   # 与 funasr 内部 extract_feature 一致
        with torch.no_grad():
            out = self.net(feat.unsqueeze(0).float().to(self.device))
        v = out.detach().cpu().numpy().reshape(-1).astype("float32")
        v = v / (float(np.linalg.norm(v)) + 1e-9)
        self.last_ms = (time.time() - t0) * 1000.0
        return v


class MockEmbedder:
    """--mock：不用模型，用音频的低阶统计量伪造成声纹。

    用途：无外网/无 torch 的环境下验证「主程序 → sidecar → 聚类 → 页面标记」整条链路。
    同一段音频稳定得到同一向量，不同音频大概率不同 —— 足以覆盖链路分支。
    """

    def __init__(self, *_, **__):
        self.device = "mock"
        self.model_id = "mock"
        self.load_sec = 0.0
        self.last_ms = 0.0

    def embed(self, pcm: bytes):
        import numpy as np

        wav = np.frombuffer(pcm, dtype="<i2").astype(np.float32) / 32768.0
        if len(wav) == 0:
            return np.zeros(8, dtype="float32")
        n = 8
        chunks = np.array_split(wav, n)
        feats = []
        for c in chunks:
            energy = float(np.sqrt(np.mean(c ** 2))) if len(c) else 0.0
            zcr = float(np.mean(np.abs(np.diff(np.sign(c)))) / 2) if len(c) > 1 else 0.0
            feats.append(round(energy * 4, 1))       # 量化，让相近音频落到同一说话人
            feats.append(round(zcr * 4, 1))
        v = np.asarray(feats, dtype="float32")
        v = v / (float(np.linalg.norm(v)) + 1e-9)
        self.last_ms = 1.0
        return v


# --------------------------------------------------------------------------
# HTTP 服务
# --------------------------------------------------------------------------
class Service:
    def __init__(self, embedder, threshold: float, min_ms: int, state_dir: str | None,
                 default_dir: str | None, save_audio: bool = False):
        self.embedder = embedder
        self.threshold = threshold
        self.min_ms = min_ms
        self.state_dir = state_dir
        self.default_dir = default_dir
        self.save_audio = save_audio   # 调试：把每段音频与判定结果落盘（DIARIZE_SAVE_AUDIO=1）
        self.save_seq = 0
        self.sessions: dict[str, SessionState] = {}
        self.lock = threading.Lock()
        self.infer_lock = threading.Lock()   # 串行推理：torch 在多线程下并发无收益
        self.started_at = time.time()
        self.calls = 0
        self.errors = 0

    def session(self, sid: str, state_path: str | None) -> SessionState:
        with self.lock:
            st = self.sessions.get(sid)
            if st is None:
                st = SessionState(sid, self.threshold, state_path)
                self.sessions[sid] = st
            elif state_path and not st.path:
                # 主程序新建 session 时会先发一次 /reset（那时还没把会话目录带过来），
                # 之后 /assign 才带上 dir。这里补上路径，否则本场说话人表永远不会落盘
                # （现象：sessions/<sid>/speakers.json 一直不生成，sidecar 重启后编号重排）。
                st.path = state_path
                st.dirty = True
            return st

    def state_path(self, sid: str, sess_dir: str | None) -> str | None:
        """声纹状态落盘位置：优先主程序给的会话目录（sessions/<sid>/），否则用默认目录。"""
        base = sess_dir or self.default_dir
        if not base:
            return None
        return os.path.join(base, STATE_NAME)

    def assign(self, sid: str, sess_dir: str | None, pcm: bytes, threshold: float | None) -> dict:
        if len(pcm) < 2:
            return {"error": "空音频"}
        seconds = len(pcm) / 2.0 / 16000.0
        if seconds * 1000.0 < self.min_ms:
            return {"speaker": "", "reason": "too_short", "seconds": round(seconds, 3)}
        st = self.session(sid, self.state_path(sid, sess_dir))
        if threshold:
            st.threshold = threshold
        with self.infer_lock:
            emb = self.embedder.embed(pcm)
            res = st.assign(emb, seconds)
        self.calls += 1
        st.save()
        if self.save_audio:
            try:
                self._save_segment(sid, sess_dir, pcm, res, emb)
            except Exception as e:      # 保存失败不影响判定主链路
                print(f"[diarize] 保存音频失败: {e}", flush=True)
        # 每次判定留一行日志：现场排查（编号乱跳/标记缺失）时这是唯一能看到
        # "同一段音频被判成谁"的地方，主程序侧只保留了聚合计数。
        print(f"[diarize] {sid} {seconds:5.1f}s → {res.get('speaker') or '-'} "
              f"conf={res.get('confidence')} new={res.get('is_new')} "
              f"本场 {res.get('speaker_count')} 人", flush=True)
        return res

    # ---- 调试用：音频落盘（--save-audio / DIARIZE_SAVE_AUDIO=1）----
    def _save_segment(self, sid: str, sess_dir: str | None, pcm: bytes, res: dict, emb):
        """把每段音频（WAV）与判定结果（index.jsonl，含 embedding）存到 <会话目录>/audio/。

        用途：离线分析「谁是谁」、重算相似度矩阵、定量验证阈值 —— 现场仅看日志数值
        不够时用。生产环境应保持关闭（音频含会议内容，注意隐私与磁盘占用：
        16k/mono ≈ 32KB/s）。
        """
        import wave

        base = sess_dir or self.state_dir or "."
        outdir = os.path.join(base, "audio")
        os.makedirs(outdir, exist_ok=True)
        with self.lock:
            self.save_seq += 1
            seq = self.save_seq
        conf = res.get("confidence")
        spk = res.get("speaker") or "none"
        name = f"seg{seq:04d}_{spk}_conf{conf if conf is not None else '-'}.wav"
        with wave.open(os.path.join(outdir, name), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(16000)
            w.writeframes(pcm)
        meta = {
            "seq": seq, "file": name, "session": sid, "ts": round(time.time(), 2),
            "seconds": round(len(pcm) / 2.0 / 16000.0, 2),
            "speaker": res.get("speaker"), "confidence": conf,
            "margin": res.get("margin"), "is_new": res.get("is_new"),
            "speaker_count": res.get("speaker_count"),
            "emb": [round(float(x), 6) for x in emb] if emb is not None else None,
        }
        with open(os.path.join(outdir, "index.jsonl"), "a", encoding="utf-8") as f:
            f.write(json.dumps(meta, ensure_ascii=False) + "\n")


class Handler(BaseHTTPRequestHandler):
    service: Service = None      # 由 main 注入
    protocol_version = "HTTP/1.1"
    server_version = "seat-diarize/1.0"

    # ---- 工具 ----
    def _json(self, code: int, obj: dict):
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _query(self) -> dict:
        return {k: v[0] for k, v in parse_qs(urlparse(self.path).query).items()}

    def _read_body(self) -> bytes:
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return b""
        if n > MAX_BODY:
            raise ValueError(f"请求体过大：{n} 字节（上限 {MAX_BODY}）")
        return self.rfile.read(n)

    def log_message(self, fmt, *args):     # 默认逐请求打日志太吵，压到只留错误
        pass

    # ---- 路由 ----
    def do_GET(self):
        if urlparse(self.path).path == "/health":
            emb = self.service.embedder
            self._json(200, {
                "ok": True,
                "model": emb.model_id,
                "device": emb.device,
                "mock": isinstance(emb, MockEmbedder),
                "load_sec": round(emb.load_sec, 2),
                "embed_ms": round(emb.last_ms, 1),
                "threshold": self.service.threshold,
                "min_ms": self.service.min_ms,
                "uptime_sec": round(time.time() - self.service.started_at, 1),
                "calls": self.service.calls,
                "errors": self.service.errors,
                "sessions": len(self.service.sessions),
                "save_audio": self.service.save_audio,
            })
            return
        self._json(404, {"error": "not found"})

    def do_POST(self):
        q = self._query()
        sid = (q.get("session") or "").strip() or "default"
        sess_dir = (q.get("dir") or "").strip() or None
        path = urlparse(self.path).path
        try:
            if path == "/assign":
                pcm = self._read_body()
                th = float(q["threshold"]) if q.get("threshold") else None
                res = self.service.assign(sid, sess_dir, pcm, th)
                self._json(400 if res.get("error") else 200, res)
                return
            if path == "/reset":
                st = self.service.session(sid, self.service.state_path(sid, sess_dir))
                st.reset()
                st.save()
                self._json(200, {"ok": True, "session": sid})
                return
            if path == "/roster":
                st = self.service.session(sid, self.service.state_path(sid, sess_dir))
                self._json(200, {"ok": True, "speakers": st.roster()})
                return
            self._json(404, {"error": "not found"})
        except Exception as e:      # 任何异常都不能让 sidecar 挂掉
            self.service.errors += 1
            print(f"[diarize] {path} 失败: {e}", flush=True)
            self._json(500, {"error": str(e)})


def main() -> int:
    ap = argparse.ArgumentParser(description="CAM++ 说话人区分 sidecar")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=18901)
    ap.add_argument("--model", default=os.environ.get("DIARIZE_MODEL",
                                                      "iic/speech_campplus_sv_zh-cn_16k-common"))
    ap.add_argument("--device", default=os.environ.get("DIARIZE_DEVICE", "cpu"))
    ap.add_argument("--threshold", type=float,
                    default=float(os.environ.get("DIARIZE_THRESHOLD", DEFAULT_THRESHOLD)))
    ap.add_argument("--min-ms", type=int,
                    default=int(os.environ.get("DIARIZE_MIN_MS", DEFAULT_MIN_MS)))
    ap.add_argument("--max-seconds", type=float, default=DEFAULT_MAX_SECONDS)
    ap.add_argument("--state-dir", default=os.environ.get("DIARIZE_STATE_DIR", ""))
    ap.add_argument("--save-audio", action="store_true",
                    default=os.environ.get("DIARIZE_SAVE_AUDIO", "").strip().lower()
                    in ("1", "true", "yes", "on"),
                    help="把每段音频与判定结果存到会话目录 audio/（调试用）")
    ap.add_argument("--mock", action="store_true", help="不用模型（联调用）")
    ap.add_argument("--no-preload", action="store_true", help="不在启动时加载模型")
    args = ap.parse_args()

    if args.mock:
        print("[diarize] mock 模式：不加载 CAM++，仅用于链路联调", flush=True)
        embedder = MockEmbedder()
    elif args.no_preload:
        embedder = None    # 首次 /assign 时再加载
    else:
        t0 = time.time()
        print(f"[diarize] 加载 CAM++（{args.model}，device={args.device}）…", flush=True)
        embedder = CampplusEmbedder(args.model, args.device, args.max_seconds)
        print(f"[diarize] 模型就绪，耗时 {time.time() - t0:.1f}s", flush=True)

    if embedder is None:
        class _Lazy:
            def __init__(self):
                self._inner = None
                self.model_id = args.model + "（懒加载）"
                self.device = args.device
                self.load_sec = 0.0
                self.last_ms = 0.0

            def embed(self, pcm):
                if self._inner is None:
                    print("[diarize] 首次请求，开始加载 CAM++…", flush=True)
                    self._inner = CampplusEmbedder(args.model, args.device, args.max_seconds)
                    self.model_id = self._inner.model_id
                    self.load_sec = self._inner.load_sec
                v = self._inner.embed(pcm)
                self.last_ms = self._inner.last_ms
                return v

        embedder = _Lazy()

    Handler.service = Service(embedder, args.threshold, args.min_ms,
                              args.state_dir or None, args.state_dir or None,
                              save_audio=args.save_audio)
    srv = ThreadingHTTPServer((args.host, args.port), Handler)
    srv.daemon_threads = True
    print(f"[diarize] 监听 http://{args.host}:{args.port}  "
          f"(阈值 {args.threshold}，最短 {args.min_ms}ms；状态目录 {args.state_dir or '由主程序指定'}"
          + ("；音频留存=开" if args.save_audio else "") + ")",
          flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("[diarize] 退出", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
