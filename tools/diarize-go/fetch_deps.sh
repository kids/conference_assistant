#!/usr/bin/env bash
# 拉取 seat-diarize 的构建/运行依赖到 third_party/（幂等，可重复执行）：
#   1) sherpa-onnx 预编译库（C API 动态库 + 头文件，约 20MB）
#   2) CAM++ ONNX 声纹模型（约 28MB）
#
# 用法：./fetch_deps.sh
# 环境变量：
#   SHERPA_ONNX_VERSION   sherpa-onnx 版本（默认 1.13.8）
#   DIARIZE_MODEL_URL     模型地址（默认 sherpa-onnx 官方 release 的 CAM++ 中文模型）
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
THIRD="$DIR/third_party"
SHERPA_VER="${SHERPA_ONNX_VERSION:-1.13.8}"
MODEL_URL="${DIARIZE_MODEL_URL:-https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/3dspeaker_speech_campplus_sv_zh-cn_16k-common.onnx}"

mkdir -p "$THIRD"

# ---- 平台 → 预编译包名（其他平台的命名请对照 release 页面核实）----
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)  PKG="sherpa-onnx-v${SHERPA_VER}-osx-arm64-shared.tar.bz2" ;;
  Darwin/x86_64) PKG="sherpa-onnx-v${SHERPA_VER}-osx-x86_64-shared.tar.bz2" ;;
  Linux/x86_64)  PKG="sherpa-onnx-v${SHERPA_VER}-linux-x64-shared.tar.bz2" ;;
  Linux/aarch64) PKG="sherpa-onnx-v${SHERPA_VER}-linux-aarch64-shared.tar.bz2" ;;
  *) echo "[deps] 不支持的平台: $(uname -s)/$(uname -m)，请手动下载 sherpa-onnx 到 $THIRD/sherpa-onnx" >&2; exit 1 ;;
esac

if [ ! -f "$THIRD/sherpa-onnx/include/sherpa-onnx/c-api/c-api.h" ]; then
  echo "[deps] 下载 sherpa-onnx v${SHERPA_VER}（$PKG）…"
  curl -fL --retry 3 -o "$THIRD/$PKG" \
    "https://github.com/k2-fsa/sherpa-onnx/releases/download/v${SHERPA_VER}/${PKG}"
  echo "[deps] 解压到 $THIRD/sherpa-onnx …"
  mkdir -p "$THIRD/sherpa-onnx"
  tar xjf "$THIRD/$PKG" -C "$THIRD/sherpa-onnx" --strip-components=1
  rm -f "$THIRD/$PKG"
fi
echo "[deps] sherpa-onnx 就绪: $THIRD/sherpa-onnx"

mkdir -p "$THIRD/models"
if [ ! -f "$THIRD/models/campplus.onnx" ]; then
  echo "[deps] 下载 CAM++ 声纹模型…"
  curl -fL --retry 3 -o "$THIRD/models/campplus.onnx" "$MODEL_URL"
fi
ls -l "$THIRD/models/campplus.onnx"
echo "[deps] 完成。构建：go build -o seat-diarize ."
