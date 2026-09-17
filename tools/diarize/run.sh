#!/usr/bin/env bash
# CAM++ 说话人区分 sidecar 启动器：**运行时**建 venv、拉依赖、下载模型，然后起服务。
#
#   tools/diarize/run.sh                     # 默认 127.0.0.1:18901
#   tools/diarize/run.sh --port 18902 --mock # 换端口 / 无模型联调
#
# 依赖体积（首次运行）：
#   - funasr + modelscope + kaldi-native-fbank ≈ 200MB（若系统已装 torch 则复用之）
#   - CAM++ 模型 iic/speech_campplus_sv_zh-cn_16k-common ≈ 28MB（下到 ~/.cache/modelscope）
# 之后就绪后启动约 2~4s（模型已缓存）。
#
# 环境变量：
#   DIARIZE_PYTHON     指定 python3（默认 python3）
#   DIARIZE_DEVICE     cpu / cuda:0（默认 cpu）
#   DIARIZE_THRESHOLD  同一说话人余弦阈值（默认 0.5，调大更"保守"、更容易拆出新说话人）
#   DIARIZE_STATE_DIR  声纹状态文件目录（默认由主程序指定为 sessions/<sid>/）
#   MODELSCOPE_CACHE   模型缓存目录（默认 ~/.cache/modelscope）
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV="$DIR/.venv"
PY="${DIARIZE_PYTHON:-python3}"
MARK="$VENV/.deps-ok"

if ! command -v "$PY" >/dev/null 2>&1; then
  echo "[diarize] 找不到 $PY；请安装 python3，或用 DIARIZE_PYTHON 指定解释器" >&2
  exit 1
fi

if [ ! -x "$VENV/bin/python" ]; then
  echo "[diarize] 首次运行：创建虚拟环境 $VENV"
  # --system-site-packages：复用系统里已装的 torch，避免重复下载 ~900MB
  "$PY" -m venv --system-site-packages "$VENV"
fi

if [ ! -f "$MARK" ]; then
  echo "[diarize] 安装依赖（funasr / modelscope / kaldi-native-fbank）…"
  "$VENV/bin/pip" install --quiet --disable-pip-version-check --upgrade pip
  if ! "$VENV/bin/python" -c "import torch" >/dev/null 2>&1; then
    echo "[diarize] 未检测到 torch，将一并安装（CPU 版体积较大，请耐心等待）"
  fi
  "$VENV/bin/pip" install --quiet --disable-pip-version-check \
    funasr modelscope kaldi-native-fbank
  # 依赖装好了才落标记；想强制重装就 rm 掉它
  touch "$MARK"
  echo "[diarize] 依赖就绪"
fi

export PYTHONUNBUFFERED=1
exec "$VENV/bin/python" "$DIR/server.py" "$@"
