# AI 跨学科实时翻译席 —— Go 版容器镜像
#
# 构建（构建上下文为仓库根目录 translation_seat/）：
#   docker build -t translation-seat:latest .
#
# 运行：
#   docker run -d --name translation-seat -p 8081:8081 \
#     -v "$PWD/.env:/app/.env:ro" \
#     -v "$PWD/sessions:/app/sessions" \
#     translation-seat:latest
#
# 访问：控制台 http://<host>:8081/console ，投屏 http://<host>:8081/screen
# 换端口：docker run -e PORT=9000 ...
#
# 说明：
#   - 前端页面（console.html / screen.html）已用 go:embed 编进二进制，运行镜像无需静态文件。
#   - 镜像内 ENV 已固定 HOST=0.0.0.0 / PORT=8081；godotenv 不覆盖已有环境变量，
#     因此挂载进来的 .env 里若写有 HOST/PORT 不会生效，改端口请用 -e PORT=xxx。
#   - CGO 依赖：webrtcvad（VAD）、gojieba（中文分词，C++）、go-sqlite3、malgo（本机声卡）。
#     运行镜像需 libstdc++6；miniaudio 用 dlopen 加载 ALSA，故 libasound2 仅为可选增强。
#   - 容器内一般无音频设备，程序会自动降级为「浏览器收音」（控制台勾选后点「开始收音」）。
#     若需容器直连声卡：加 --device /dev/snd 并保证宿主机音频权限。
#   - 镜像内附 mockasr（本地假 ASR 服务），可在无外网环境离线验证整条流水线：
#       docker exec -it <容器> /app/mockasr -addr 127.0.0.1:18900
#
# 另：Python 版镜像保留在 Dockerfile.python（docker build -f Dockerfile.python .）

# ---------- 构建阶段 ----------
FROM golang:1.24-bookworm AS builder

ENV CGO_ENABLED=1 \
    GOFLAGS=-buildvcs=false \
    GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct \
    GOTOOLCHAIN=local

WORKDIR /src

# 先只拷依赖清单，命中缓存后改代码不必重下依赖
COPY go/go.mod go/go.sum ./
RUN go mod download

COPY go/ ./
# gojieba 含 C++ 源码，首次编译约 1~2 分钟
RUN go build -trimpath -ldflags="-s -w" -o /out/seat . \
    && go build -trimpath -ldflags="-s -w" -o /out/mockasr ./tools/mockasr

# gojieba 默认用 runtime.Caller 从「编译期源码路径」推导词典目录，该路径在运行镜像里
# 不存在（gojieba 会 panic）。这里把词典从模块缓存提取到固定位置，运行期用
# JIEBA_DICT_DIR 指过去。词典含 pos_dict 子目录（词性标注必需）。
RUN cp -r "$(go env GOMODCACHE)"/github.com/yanyiwu/gojieba@*/deps/cppjieba/dict /out/jieba_dict

# ---------- 运行阶段 ----------
FROM debian:bookworm-slim

ENV TZ=Asia/Shanghai \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    HOST=0.0.0.0 \
    PORT=8081 \
    JIEBA_DICT_DIR=/app/jieba_dict

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata libstdc++6 libasound2 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /out/seat /app/seat
COPY --from=builder /out/mockasr /app/mockasr
COPY --from=builder /out/jieba_dict /app/jieba_dict
COPY hotwords_dict.txt ./
COPY .env.example ./

RUN mkdir -p /app/sessions
VOLUME ["/app/sessions"]

EXPOSE 8081

# 运行镜像内没有 curl/python，用二进制自带的探活参数
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/seat", "-healthcheck"]

CMD ["/app/seat"]
