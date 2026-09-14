// mockasr 本地假 ASR 服务（funasr_nano 协议），用于离线验证整条流水线。
//
// 用途：在无法访问真实 ASR 网关的环境下（如内网隔离、CI），验证
// 采集 → VAD → ASR 客户端 → 回调 → SQLite → 事件总线 → WebSocket 全链路。
//
// 行为：
//   - 按 funasr_nano 协议应答 START / LANGUAGE / HOTWORDS / STOP；
//   - 每收到 sentence_secs 秒音频，吐出一句固定的学术句子（模拟服务端 VAD 断句）；
//   - 期间周期性吐出 partial 草稿。
//
// 运行：
//
//	go run ./tools/mockasr -addr 127.0.0.1:18900
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 固定句子：覆盖热词命中、学术后缀、中英混排、复合名词等术语提取分支。
var sentences = []string{
	"我们用量子纠缠的方法观测了二维材料的超导相变机制",
	"表面张力导致液滴在纳米结构表面呈现柱状铺展",
	"CRISPR基因编辑技术的脱靶效应需要进一步评估",
}

var partials = []string{
	"我们用量子",
	"我们用量子纠缠的方法观测",
	"表面张力导致液滴",
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18900", "监听地址")
	sentenceSecs := flag.Float64("sentence-secs", 2.0, "每多少秒音频吐出一句确认句")
	flag.Parse()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		log.Printf("[mockasr] 连接建立 %s", r.RemoteAddr)
		serve(conn, *sentenceSecs)
		log.Printf("[mockasr] 连接关闭 %s", r.RemoteAddr)
	})

	log.Printf("[mockasr] 监听 %s（协议 funasr_nano）", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatal(err)
	}
}

func serve(conn *websocket.Conn, sentenceSecs float64) {
	const sampleRate = 16000
	bytesPerSentence := int(sentenceSecs * sampleRate * 2)

	var (
		mu         sync.Mutex
		audioBytes int
		partialIdx int
		started    bool
		// 已确认句累积列表：真实 FunASR-Nano 服务端会把确认句持续追加到 sentences 数组，
		// 客户端据此区分"新句"（len 增长）与"回退修正"（同下标文本变化）。
		confirmed []string
	)

	sendJSON := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(v); err != nil {
			log.Printf("[mockasr] 写入失败: %v", err)
		}
	}

	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if mt == websocket.TextMessage {
			cmd := strings.TrimSpace(string(data))
			switch {
			case cmd == "START":
				started = true
				audioBytes, partialIdx = 0, 0
				confirmed = nil
				sendJSON(map[string]any{"event": "started"})
			case strings.HasPrefix(cmd, "LANGUAGE:"):
				sendJSON(map[string]any{"event": "language_set", "language": strings.TrimPrefix(cmd, "LANGUAGE:")})
			case strings.HasPrefix(cmd, "HOTWORDS:"):
				words := strings.Split(strings.TrimPrefix(cmd, "HOTWORDS:"), ",")
				log.Printf("[mockasr] 收到热词 %d 个: %v", len(words), words)
				sendJSON(map[string]any{"event": "hotwords_set", "hotwords": words})
			case cmd == "STOP":
				// 句尾 flush：把剩余音频对应的一句追加进累积数组后整组回发
				mu.Lock()
				confirmed = append(confirmed, sentences[len(confirmed)%len(sentences)])
				list := buildSentenceList(confirmed)
				mu.Unlock()
				sendJSON(map[string]any{"sentences": list, "partial": "", "is_final": true})
				sendJSON(map[string]any{"event": "stopped"})
			}
			continue
		}

		if mt != websocket.BinaryMessage || !started {
			continue
		}

		mu.Lock()
		audioBytes += len(data)
		reached := audioBytes >= bytesPerSentence
		var (
			list    []map[string]any
			partial string
			pidx    int
		)
		if reached {
			audioBytes = 0
			confirmed = append(confirmed, sentences[len(confirmed)%len(sentences)])
			list = buildSentenceList(confirmed)
		} else {
			partial = partials[partialIdx%len(partials)]
			pidx = partialIdx
			partialIdx++
		}
		mu.Unlock()

		if reached {
			sendJSON(map[string]any{"sentences": list, "partial": "", "is_final": false})
			continue
		}
		// 草稿：节流到约每 500ms 一次
		if pidx%25 == 0 {
			sendJSON(map[string]any{"sentences": []any{}, "partial": partial, "is_final": false})
		}
	}
}

// buildSentenceList 把已确认句列表转成协议格式（累积数组，与真实服务端一致）。
func buildSentenceList(confirmed []string) []map[string]any {
	list := make([]map[string]any, 0, len(confirmed))
	for _, t := range confirmed {
		list = append(list, map[string]any{"text": t, "is_final": true})
	}
	return list
}
