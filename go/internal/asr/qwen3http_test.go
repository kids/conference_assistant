package asr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newInferTestClient 构造一个只用来直接调 infer 的客户端（不喂音频、不起流水线）。
// 超时给 2s：死地址/异常响应都能快速失败，测试不必等默认的 60s。
func newInferTestClient(t *testing.T, baseURL string) *Qwen3AsrHttpClient {
	t.Helper()
	c, err := NewQwen3AsrHttpClient(baseURL, "qwen3asr17b", "中文", nil,
		1.0, 600, 2, 15, 2.0, 0, Handlers{}) // 0=关闭冷静期，测试直接调 infer
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestInferRetriesOn5xx：5xx 属临时故障，应当重试；最终失败时状态必须是 error，
// 且带上原因与连续失败次数（这正是「有语音却一直没字」能被看见的前提）。
func TestInferRetriesOn5xx(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	if got := c.infer(make([]byte, 16000*2)); got != "" {
		t.Fatalf("失败时应返回空串，实际 %q", got)
	}
	if got := atomic.LoadInt32(&reqs); got != qwenInferAttempts {
		t.Errorf("5xx 应重试到 %d 次，实际只发了 %d 次请求", qwenInferAttempts, got)
	}
	si := c.StatusInfo()
	if si.State != "error" {
		t.Errorf("状态应为 error（旧版会被 connected 覆盖），实际 %q", si.State)
	}
	if si.InferFail != 1 {
		t.Errorf("连续失败次数应为 1，实际 %d", si.InferFail)
	}
	if !strings.Contains(si.Detail, "HTTP 500") {
		t.Errorf("detail 应含 HTTP 500，实际 %q", si.Detail)
	}
}

// TestInferNoRetryOn4xx：4xx 是请求本身的问题（模型名、参数非法），重试只会拿到同样结果，
// 必须只发一次，并把服务端返回的原因带出来（例如语言码不合法）。
func TestInferNoRetryOn4xx(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported language: 'xx'"}}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	c.infer(make([]byte, 16000*2))

	if got := atomic.LoadInt32(&reqs); got != 1 {
		t.Errorf("4xx 不应重试，实际发了 %d 次请求", got)
	}
	si := c.StatusInfo()
	if !strings.Contains(si.Detail, "Unsupported language") {
		t.Errorf("detail 应带上服务端返回的原因，实际 %q", si.Detail)
	}
	if si.InferFail != 1 {
		t.Errorf("连续失败次数应为 1，实际 %d", si.InferFail)
	}
}

// TestInferSuccessClearsFail：一次失败后紧接着成功，状态必须回到 connected、
// 计数清零，且文本正常返回 —— 否则「恢复」这件事在页面上永远看不出来。
func TestInferSuccessClearsFail(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= qwenInferAttempts {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"  你好世界  "}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)

	if got := c.infer(make([]byte, 16000*2)); got != "" {
		t.Fatalf("第一次应失败返回空串，实际 %q", got)
	}
	if si := c.StatusInfo(); si.InferFail == 0 || si.State != "error" {
		t.Fatalf("第一次失败后应处于 error 且计数 >0，实际 state=%q fail=%d", si.State, si.InferFail)
	}

	if got := c.infer(make([]byte, 16000*2)); got != "你好世界" {
		t.Fatalf("第二次应成功并去除首尾空白，实际 %q", got)
	}
	si := c.StatusInfo()
	if si.State != "connected" {
		t.Errorf("成功后状态应回到 connected，实际 %q", si.State)
	}
	if si.InferFail != 0 {
		t.Errorf("成功后连续失败计数应清零，实际 %d", si.InferFail)
	}
	if si.Detail != "" {
		t.Errorf("成功后 detail 应清空，实际 %q", si.Detail)
	}
}

// TestInferLanguageMappedToISO：中文别名必须转成 ISO 码再发出。
// 服务端只接受 ISO 码，发「中文」会 400 —— 这条曾经被误判为故障根因，固化成测试。
func TestInferLanguageMappedToISO(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		got = r.FormValue("language")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	c.infer(make([]byte, 16000*2))

	if got != "zh" {
		t.Errorf("language 应被转换为 zh，实际发出 %q", got)
	}
}

// TestInferAutoLanguageOmitsField：语种=自动时不得发送 language 字段。
// 该字段一旦出现，服务端就会把解码语种强制定死 —— 英文报告在复杂现场条件下会被
// 硬解码成中文（空耳）甚至翻译成中文，正是本次问题的根因；省略字段才走自动语种检测。
func TestInferAutoLanguageOmitsField(t *testing.T) {
	var (
		has  bool
		body string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, has = r.MultipartForm.Value["language"]
		body = r.FormValue("language")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	c.SetLanguage("自动")
	c.infer(make([]byte, 16000*2))

	if has {
		t.Errorf("自动语种不应发送 language 字段（当前收到 %q）", body)
	}
}

// TestInferSendsTokenWhenConfigured：QWEN3_TOKEN 填了就必须带 Authorization: Bearer
// （服务端开 --api-key 时唯一凭据）；留空则绝不发该头 —— 配置项必须真的生效。
func TestInferSendsTokenWhenConfigured(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	c.infer(make([]byte, 16000*2))
	if got != "" {
		t.Fatalf("token 空时不应发送 Authorization，实际 %q", got)
	}

	c.SetToken("secret-key")
	c.infer(make([]byte, 16000*2))
	if got != "Bearer secret-key" {
		t.Errorf("token 应作为 Bearer 头发送，实际 %q", got)
	}
}

// TestSetLanguageAppliesNextInfer：运行期切语种，下一次推理即用新值（无需重建客户端）。
func TestSetLanguageAppliesNextInfer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		got = r.FormValue("language")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer srv.Close()

	c := newInferTestClient(t, srv.URL)
	c.SetLanguage("英文")
	c.infer(make([]byte, 16000*2))
	if got != "en" {
		t.Fatalf("切到英文后应发出 en，实际 %q", got)
	}

	c.SetLanguage("自动")
	c.infer(make([]byte, 16000*2))
	if got != "" {
		t.Errorf("切回自动后不应发出 language 字段，实际 %q", got)
	}
}

// TestNormalizeLanguage：别名归一化是语种配置的唯一入口，各协议据此再转写。
func TestNormalizeLanguage(t *testing.T) {
	cases := map[string]string{
		"":        LanguageAuto,
		"auto":    LanguageAuto,
		"自动":      LanguageAuto,
		"自动识别":    LanguageAuto,
		" zh ":    "中文",
		"中文":      "中文",
		"汉语":      "中文",
		"en":      "英文",
		"英语":      "英文",
		"English": "英文",
		"ja":      "日语",
		"ko":      "韩语",
		"de":      "de", // 未收录的 ISO 码原样透传，由协议侧决定
	}
	for in, want := range cases {
		if got := NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q，期望 %q", in, got, want)
		}
	}
	for _, known := range []string{LanguageAuto, "中文", "英文", "日语", "韩语"} {
		if !KnownLanguage(known) {
			t.Errorf("%q 应为已知语种", known)
		}
	}
	for _, unknown := range []string{"", "乱七八糟", "zh"} {
		if KnownLanguage(unknown) {
			t.Errorf("%q 不应被判为已知语种", unknown)
		}
	}
}
