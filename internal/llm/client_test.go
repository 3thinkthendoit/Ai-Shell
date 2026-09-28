package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatReturnsContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("请求路径错误: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"你好，我来帮你排查。"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", "test-model")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if msg.Content != "你好，我来帮你排查。" {
		t.Fatalf("内容不符: %q", msg.Content)
	}
}

// tool_calls 的解析是 agent 循环的基础，必须正确。
func TestChatParsesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",
			"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"run_command",
			"arguments":"{\"host_id\":\"h1\",\"command\":\"uptime\"}"}}]}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m")
	msg, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "查负载"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("期望 1 个工具调用，实际 %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Function.Name != "run_command" {
		t.Fatalf("工具调用解析错误: %+v", tc)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", err)
	}
	if args["host_id"] != "h1" || args["command"] != "uptime" {
		t.Fatalf("参数内容错误: %+v", args)
	}
}

func TestChatSendsAuthHeader(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-secret-key", "gpt-4o-mini")
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer sk-secret-key" {
		t.Fatalf("Authorization 头不符: %q", gotAuth)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Fatalf("model 未正确传递: %v", gotBody["model"])
	}
	// 未传工具时不应带 tools 字段，避免部分服务端报错
	if _, ok := gotBody["tools"]; ok {
		t.Fatal("无工具时不应发送 tools 字段")
	}
}

func TestChatSendsTools(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m")
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "run_command",
			Description: "执行命令",
			Parameters:  map[string]any{"type": "object"},
		},
	}}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, tools); err != nil {
		t.Fatal(err)
	}
	arr, ok := gotBody["tools"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("tools 未正确发送: %v", gotBody["tools"])
	}
	if gotBody["tool_choice"] != "auto" {
		t.Fatalf("tool_choice 应为 auto: %v", gotBody["tool_choice"])
	}
}

// 本地模型通常不需要 Key，此时不应带 Authorization 头。
func TestChatOmitsAuthHeaderWhenNoKey(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadAuth = r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "local-model")
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Fatal("无 Key 时不应发送 Authorization 头")
	}
}

// 关键安全断言：上游回显了请求头时，错误信息里也不能出现 API Key。
func TestChatErrorDoesNotLeakAPIKey(t *testing.T) {
	const key = "sk-super-secret-key-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// 模拟一个会回显 Authorization 头的粗糙网关
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key: Bearer ` + key + `"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, key, "m")
	_, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "x"}}, nil)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误信息泄漏了 API Key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应含状态码: %v", err)
	}
}

func TestChatErrors(t *testing.T) {
	c := New("", "k", "m")
	if _, err := c.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("缺少 BaseURL 应报错")
	}

	c2 := New("http://example.com/v1", "k", "")
	if _, err := c2.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("缺少模型名应报错")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "k", "m").Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("非法 JSON 应报错")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv2.Close()
	if _, err := New(srv2.URL, "k", "m").Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("空候选应报错")
	}

	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
	}))
	defer srv3.Close()
	if _, err := New(srv3.URL, "k", "m").Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("上游 error 字段应被识别")
	}
}

// BaseURL 末尾斜杠不应产生双斜杠路径。
func TestBaseURLTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "k", "m")
	if _, err := c.Chat(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("路径拼接错误: %q", gotPath)
	}
}

// ---- Base URL 规范化 ----

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// 最常见的手滑：把文档里的完整端点粘进来
		{"https://token.sensenova.cn/v1/chat/completions", "https://token.sensenova.cn/v1"},
		{"https://api.deepseek.com/v1/chat/completions/", "https://api.deepseek.com/v1"},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1"},
		{"  https://api.openai.com/v1  ", "https://api.openai.com/v1"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		// 反向断言：不该动的别动
		{"http://localhost:11434/v1", "http://localhost:11434/v1"},
		{"https://host/chat/completionsX", "https://host/chat/completionsX"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 回归测试：把完整端点填进 Base URL 时，请求**不得**拼成
// .../chat/completions/chat/completions（那会 404，而且错误信息完全看不出原因）。
func TestEndpointSuffixInBaseURLDoesNotDouble(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/chat/completions", "k", "m")
	if _, err := c.Chat(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("端点被重复拼接了: %q（期望 /chat/completions）", gotPath)
	}
}

// ---- Ping ----

func TestPingSendsMinimalRequest(t *testing.T) {
	var body map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL, "sk-secret", "deepseek-flash").Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping 应成功: %v", err)
	}
	if path != "/chat/completions" {
		t.Fatalf("路径错误: %q", path)
	}
	if auth != "Bearer sk-secret" {
		t.Fatalf("Authorization 头错误: %q", auth)
	}
	if res.Reply != "pong" {
		t.Fatalf("回复不符: %q", res.Reply)
	}
	if res.Status != 200 {
		t.Fatalf("状态码应为 200，实得 %d", res.Status)
	}
	if res.URL == "" || strings.Contains(res.URL, "sk-secret") {
		t.Fatalf("回显的 URL 应为空或不含密钥，实得 %q", res.URL)
	}

	// 必须是「最小请求」：只带一条消息、max_tokens 压到很小、不带 tools、不流式
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("应只发一条消息，实得 %d 条", len(msgs))
	}
	mt, _ := body["max_tokens"].(float64)
	if mt <= 0 || mt > 16 {
		t.Fatalf("max_tokens 应被压到很小（避免产生费用），实得 %v", body["max_tokens"])
	}
	if _, ok := body["tools"]; ok {
		t.Fatal("连通性测试不该带 tools")
	}
	if s, _ := body["stream"].(bool); s {
		t.Fatal("连通性测试不该用流式")
	}
}

// 404 是最常见的配置错误，必须把「实际请求的地址」带回去，
// 否则用户只能看到一句 "LLM 返回 404"，完全不知道该改什么。
func TestPingSurfacesStatusAndURLOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL, "k", "m").Ping(context.Background())
	if err == nil {
		t.Fatal("404 时 Ping 必须返回错误")
	}
	if res.Status != 404 {
		t.Fatalf("状态码应回传 404，实得 %d", res.Status)
	}
	if !strings.Contains(res.URL, "/chat/completions") {
		t.Fatalf("应回传实际请求的地址，实得 %q", res.URL)
	}
}

func TestPingRejectsNonLLMResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>hello</body></html>"))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", "m").Ping(context.Background())
	if err == nil {
		t.Fatal("返回 HTML 时应报错")
	}
	if !strings.Contains(err.Error(), "非 LLM 服务") {
		t.Fatalf("错误信息应提示地址可能不是 LLM 服务，实得: %v", err)
	}
}

func TestPingRedactsAPIKeyFromError(t *testing.T) {
	const key = "sk-verysecret1234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// 服务端把密钥回显在错误体里 —— 这种服务真实存在
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key: ` + key + `"}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, key, "m").Ping(context.Background())
	if err == nil {
		t.Fatal("401 时应报错")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误信息泄漏了 API Key: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("应把密钥替换成 [REDACTED]，实得: %v", err)
	}
}

func TestPingRequiresConfig(t *testing.T) {
	if _, err := New("", "k", "m").Ping(context.Background()); err == nil {
		t.Fatal("缺 Base URL 时应报错")
	}
	if _, err := New("http://x", "k", "").Ping(context.Background()); err == nil {
		t.Fatal("缺模型名时应报错")
	}
}

func TestPingReportsUnreachable(t *testing.T) {
	// 127.0.0.1:1 上不会有人监听
	res, err := New("http://127.0.0.1:1/v1", "k", "m").Ping(context.Background())
	if err == nil {
		t.Fatal("连不上时应报错")
	}
	if res.Status != 0 {
		t.Fatalf("压根没连上时 Status 应为 0，实得 %d", res.Status)
	}
}
