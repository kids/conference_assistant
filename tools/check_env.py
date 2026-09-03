"""环境自检：依赖 / 音频设备 / ASR / LLM 配置。"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app.config import get_settings  # noqa: E402


def main() -> None:
    s = get_settings()
    print("== 配置 ==")
    print(f"  FunASR 服务 : {s.funasr_serv}")
    print(f"  FunASR WS   : {s.ws_url}")
    print(f"  ASR 模式    : {s.asr_mode}  chunk_size={s.chunk_size}")
    print(f"  热词词典    : {s.hotwords_file} ({s.hotwords_file.exists() and '存在' or '缺失'})")
    print(f"  LLM         : {s.llm_base or '(未配置，将用占位输出)'} 模型={s.llm_model or '-'}")

    print("\n== 依赖 ==")
    for mod in ("sounddevice", "webrtcvad", "numpy", "websocket", "httpx", "fastapi"):
        try:
            __import__(mod)
            print(f"  [ok] {mod}")
        except ImportError:
            print(f"  [MISSING] {mod}  -> pip install -r requirements.txt")

    print("\n== 音频设备 ==")
    try:
        import sounddevice as sd

        print(f"  默认输入 : {sd.default.device[0]}")
        for i, d in enumerate(sd.query_devices()):
            mark = " <== 默认" if i == sd.default.device[0] else ""
            print(f"  [{i}] {d['name']} in={d['max_input_channels']} out={d['max_output_channels']}{mark}")
    except Exception as e:  # noqa: BLE001
        print(f"  无法枚举设备: {e}")

    print("\n== ASR 连通性 ==")
    try:
        import httpx

        r = httpx.get(s.funasr_serv.rstrip("/") + "/health", timeout=3)
        print(f"  /health -> {r.status_code} {r.text[:80]}")
    except Exception as e:  # noqa: BLE001
        print(f"  /health 失败: {e}")


if __name__ == "__main__":
    main()
