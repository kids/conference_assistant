#!/usr/bin/env bash
set -e
cd "$(dirname "$0")"

# 若无 .env，从 .env.example 生成
if [ ! -f .env ]; then
  echo "[run] .env 不存在，已从 .env.example 生成，请填写 LLM_BASE/LLM_KEY 后重跑"
  cp .env.example .env
fi

# 若无虚拟环境，用 uv 创建并安装依赖
if [ ! -x ".venv/bin/python" ]; then
  echo "[run] 未发现 .venv，正在用 uv 创建并安装依赖..."
  uv venv .venv --python 3.12
  uv pip install --python .venv/bin/python -r requirements.txt
  uv pip install --python .venv/bin/python "setuptools<81"
fi

# 默认绑定所有网卡，方便远端浏览器直接打开服务（仅本机访问：HOST=127.0.0.1 ./run.sh）
HOST="${HOST:-0.0.0.0}"
# 默认端口 8081（8080 可能被 IDE 等占用），可用 PORT 环境变量覆盖
PORT="${PORT:-8081}"

# 打印访问地址：远端使用可配合「浏览器收音」（浏览器采音，服务器无需本地麦克风）
LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
if [ -z "$LAN_IP" ]; then LAN_IP="$(ipconfig getifaddr 2>/dev/null || true)"; fi
echo "[run] 控制台：    http://127.0.0.1:${PORT}/console"
echo "[run] 投屏大屏：  http://127.0.0.1:${PORT}/screen"
if [ -n "$LAN_IP" ]; then
  echo "[run] 远端访问：  http://${LAN_IP}:${PORT}/console  （远端收音请在控制台勾选「浏览器收音」）"
fi

# 固定使用 venv 内的 uvicorn，避免系统 PATH 的 python 版本混乱
exec .venv/bin/python -m uvicorn app.main:app \
  --host "$HOST" \
  --port "$PORT" \
  --reload
