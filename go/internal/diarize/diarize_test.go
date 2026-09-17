package diarize

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sidecar 是可选依赖：这里同时验证「协议对不对」与「不可用时主链路不受影响」。

func TestAssignPostsPCMAndParsesResult(t *testing.T) {
	var gotBody []byte
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(Result{Speaker: "S2", Label: "说话人 2", Confidence: 0.71, SpeakerCount: 2})
	}))
	defer srv.Close()

	c := New(srv.URL, 5, 0.55, 600)
	pcm := make([]byte, 16000*2) // 1 秒
	res, err := c.Assign(context.Background(), "sid1", "/data/sessions/sid1", pcm)
	if err != nil {
		t.Fatalf("Assign 失败: %v", err)
	}
	if res.Speaker != "S2" || res.Label != "说话人 2" || res.SpeakerCount != 2 {
		t.Errorf("结果解析异常: %+v", res)
	}
	if len(gotBody) != len(pcm) {
		t.Errorf("应原样发送 PCM（%d 字节），实际 %d", len(pcm), len(gotBody))
	}
	// session / dir / threshold 必须都带上：dir 决定声纹状态落盘位置（会话目录），
	// threshold 决定同一说话人的判定松紧
	for _, want := range []string{"session=sid1", "dir=%2Fdata%2Fsessions%2Fsid1", "threshold=0.550"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("查询串应包含 %q，实际 %q", want, gotQuery)
		}
	}
}

func TestAssignSkipsTooShortLocally(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(Result{Speaker: "S1"})
	}))
	defer srv.Close()

	c := New(srv.URL, 5, 0, 600)
	res, err := c.Assign(context.Background(), "sid", "", make([]byte, 16000)) // 0.5 秒 < 600ms
	if err != nil {
		t.Fatalf("过短片段不应报错: %v", err)
	}
	if calls != 0 {
		t.Error("过短片段不该打 sidecar（省一次推理）")
	}
	if res.Reason != "too_short" {
		t.Errorf("应标记 too_short，实际 %+v", res)
	}
}

func TestAssignSurfacesSidecarError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "空音频"})
	}))
	defer srv.Close()

	c := New(srv.URL, 5, 0, 0)
	if _, err := c.Assign(context.Background(), "sid", "", make([]byte, 16000)); err == nil {
		t.Fatal("非 200 应报错，否则主程序会静默丢标记")
	}
}

func TestDisabledClientIsInert(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Error("零值客户端不应视为启用")
	}
	if _, err := c.Assign(context.Background(), "s", "", nil); err == nil {
		t.Error("未启用时应返回错误（由调用方短路）")
	}
	c2 := New("", 5, 0, 600)
	if c2.Enabled() {
		t.Error("空 URL 不应启用")
	}
	if err := c2.Reset(context.Background(), "s", ""); err != nil {
		t.Errorf("未启用时 Reset 应静默成功，实际 %v", err)
	}
}

func TestHealthAndReset(t *testing.T) {
	var resetCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": "campplus", "sessions": 1})
		case "/reset":
			resetCalled = true
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, 5, 0, 0)
	info, err := c.Health(context.Background())
	if err != nil || info["model"] != "campplus" {
		t.Fatalf("health 解析异常: %v %v", info, err)
	}
	if err := c.Reset(context.Background(), "sid", "/data/sessions/sid"); err != nil {
		t.Fatalf("reset 失败: %v", err)
	}
	if !resetCalled {
		t.Error("reset 应真的打到 sidecar")
	}
}
