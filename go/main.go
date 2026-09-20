// AI 跨学科实时翻译席 —— Go 实现（仓库唯一实现）。
//
// 前端页面（web/console.html、web/screen.html）已内嵌进二进制。
// 早期曾有一份功能等价的 Python 版（app/），已于 2026-09 移除（历史可查 git 提交）。
//
// 构建：
//
//	CGO_ENABLED=1 go build -o seat .
//
// 运行：
//
//	./seat              # 读取 BASE_DIR/.env（默认当前目录，其次上级目录）
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"seat/internal/config"
	"seat/internal/server"
)

//go:embed web
var webAssets embed.FS

func main() {
	log.SetFlags(log.LstdFlags)

	// 自探活：容器运行镜像内没有 curl/python，用自身做 HEALTHCHECK 探针
	healthcheck := flag.Bool("healthcheck", false, "请求本地 /api/health 并按其结果退出（0=健康）")
	flag.Parse()

	settings := config.Load()

	if *healthcheck {
		os.Exit(runHealthcheck(settings.Port))
	}

	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("内嵌前端资源异常: %v", err)
	}

	rt, err := server.NewRuntime(settings)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer rt.Close()

	srv := server.New(rt, sub)

	addr := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout / IdleTimeout：长连接 WebSocket 需要无限期保持
	}

	srv.LogStartup(settings.Host, settings.Port)
	log.Printf("[seat] 配置目录：  %s（协议 %s，LLM %s）",
		settings.BaseDir, settings.ASRProtocol, orNone(settings.LLMModel))

	// 优雅退出：收到信号后停止流水线并关闭监听
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("[seat] 收到退出信号，正在停止…")
		rt.StopPipeline()
		_ = httpSrv.Close()
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务启动失败: %v", err)
	}
	log.Printf("[seat] 已退出")
}

func orNone(s string) string {
	if s == "" {
		return "(未配置)"
	}
	return s
}

// runHealthcheck 请求本地 /api/health，返回进程退出码。
func runHealthcheck(port int) int {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
