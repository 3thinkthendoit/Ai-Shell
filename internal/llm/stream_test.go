package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- 辅助 ----

// sseServer 起一个按给定分片写出 SSE 的假服务端。
// 分片会刻意切在 JSON 中间，用来验证解析器不依赖「一次读完一行」。
func sseServer(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			if _, err := io.WriteString(w, c); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New(srv.URL, "sk-secret-key", "test-model")
	c.http.Timeout = 10 * time.Second
	return c
}

// delta 生成一个只含正文增量的 SSE 事件。
func delta(text string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
	})
	return "data: " + string(b) + "\n\n"
}

// ---- 正文流式 ----

func TestChatStreamAggregatesContent(t *testing.T) {
	srv := sseServer(t, []string{
		delta("nginx "), delta("起不来"), delta("，我看一下"), "data: [DONE]\n\n",
	})
	c := newTestClient(t, srv)

	var frags []string
	msg, err := c.ChatStream(context.Background(), nil, nil, func(d Delta) {
		frags = append(frags, d.Content)
	})
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if msg.Content != "nginx 起不来，我看一下" {
		t.Fatalf("聚合内容错误: %q", msg.Content)
	}
	if msg.Role != "assistant" {
		t.Fatalf("角色应为 assistant，实得 %q", msg.Role)
	}
	// 推给界面的增量之和必须等于最终内容，否则界面与真实回复会不一致
	if got := strings.Join(frags, ""); got != msg.Content {
		t.Fatalf("增量之和 %q 与最终内容 %q 不一致", got, msg.Content)
	}
	if len(frags) != 3 {
		t.Fatalf("期望 3 段增量，实得 %d", len(frags))
	}
}

// JSON 被任意切分（甚至切在 "data:" 中间）也不应影响解析。
func TestChatStreamToleratesArbitraryChunkBoundaries(t *testing.T) {
	full := delta("abcdef") + delta("ghij") + "data: [DONE]\n\n"

	// 逐字节切开，制造最恶劣的边界
	var chunks []string
	for _, r := range full {
		chunks = append(chunks, string(r))
	}
	srv := sseServer(t, chunks)
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("逐字节分片下不应失败: %v", err)
	}
	if msg.Content != "abcdefghij" {
		t.Fatalf("内容错误: %q", msg.Content)
	}
}

func TestChatStreamNilCallbackIsSafe(t *testing.T) {
	srv := sseServer(t, []string{delta("hi"), "data: [DONE]\n\n"})
	c := newTestClient(t, srv)
	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil || msg.Content != "hi" {
		t.Fatalf("onDelta 为 nil 时应当正常工作，实得 %q / %v", msg.Content, err)
	}
}

// 流末尾没有空行收尾时也要把最后一段取到。
func TestChatStreamHandlesMissingTrailingBlankLine(t *testing.T) {
	srv := sseServer(t, []string{delta("尾巴")}) // 注意：没有末尾的 "\n\n"
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if msg.Content != "尾巴" {
		t.Fatalf("末尾事件被丢弃: %q", msg.Content)
	}
}

// 服务端插入注释行（心跳）不应中断解析。
func TestChatStreamIgnoresCommentsAndKeepAlives(t *testing.T) {
	srv := sseServer(t, []string{
		": ping\n\n",
		delta("ok"),
		": keep-alive\n\n",
		"data: [DONE]\n\n",
	})
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("心跳不应导致失败: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("内容错误: %q", msg.Content)
	}
}

// 单个 data 行超过默认 64KB scanner 缓冲时必须仍然可读。
func TestChatStreamHandlesLongLine(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	srv := sseServer(t, []string{delta(long), "data: [DONE]\n\n"})
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("长行读取失败: %v", err)
	}
	if len(msg.Content) != len(long) {
		t.Fatalf("长行内容被截断：期望 %d 字节，实得 %d", len(long), len(msg.Content))
	}
}

// ---- 流式下的工具调用 ----

func TestChatStreamAggregatesToolCallArguments(t *testing.T) {
	// 工具参数是逐段拼接的 JSON 字符串，必须按序累加
	arg := `{"host_id":"h1","command":"systemctl status nginx"}`
	part1, part2, part3 := arg[:10], arg[10:30], arg[30:]

	chunk := func(index int, id, name, args string) string {
		fn := map[string]any{}
		if name != "" {
			fn["name"] = name
		}
		if args != "" {
			fn["arguments"] = args
		}
		tc := map[string]any{"index": index, "function": fn}
		if id != "" {
			tc["id"] = id
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{tc}}}},
		})
		return "data: " + string(b) + "\n\n"
	}

	srv := sseServer(t, []string{
		chunk(0, "call_abc", "run_command", part1),
		chunk(0, "", "", part2),
		chunk(0, "", "", part3),
		"data: [DONE]\n\n",
	})
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("期望 1 个工具调用，实得 %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Function.Name != "run_command" {
		t.Fatalf("工具调用元信息错误: %+v", tc)
	}
	if tc.Function.Arguments != arg {
		t.Fatalf("参数拼接错误:\n 期望 %s\n 实得 %s", arg, tc.Function.Arguments)
	}
	// 参数必须是合法 JSON —— 拼接错一位这里就会炸
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
		t.Fatalf("拼接出的参数不是合法 JSON: %v", err)
	}
	if parsed["command"] != "systemctl status nginx" {
		t.Fatalf("参数内容错误: %v", parsed)
	}
}

// 多个工具调用交错到达时，必须按 index 归位而不是按到达顺序。
func TestChatStreamKeepsInterleavedToolCallsSeparate(t *testing.T) {
	chunk := func(index int, id, name, args string) string {
		tc := map[string]any{"index": index, "function": map[string]any{"name": name, "arguments": args}}
		if id != "" {
			tc["id"] = id
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{tc}}}},
		})
		return "data: " + string(b) + "\n\n"
	}

	srv := sseServer(t, []string{
		chunk(0, "c0", "run_command", `{"a":`),
		chunk(1, "c1", "read_file", `{"b":`),
		chunk(0, "", "", `1}`),
		chunk(1, "", "", `2}`),
		"data: [DONE]\n\n",
	})
	c := newTestClient(t, srv)

	msg, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("期望 2 个工具调用，实得 %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "c0" || msg.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("第 0 个工具调用错位: %+v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].ID != "c1" || msg.ToolCalls[1].Function.Arguments != `{"b":2}` {
		t.Fatalf("第 1 个工具调用错位: %+v", msg.ToolCalls[1])
	}
}

// 流式与非流式必须对同一份语义内容给出一致结果，
// 否则「换一种传输方式」就会悄悄改变 Agent 的行为。
func TestStreamAndNonStreamAgree(t *testing.T) {
	payload := map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role":    "assistant",
				"content": "答案",
				"tool_calls": []any{map[string]any{
					"id": "c1", "type": "function",
					"function": map[string]any{"name": "run_command", "arguments": `{"x":1}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	body, _ := json.Marshal(payload)

	// 非流式
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer plain.Close()

	// 流式（拆成若干 delta：正文只在第一段出现，工具参数跨段拼接）
	fnChunk := func(content, args string) string {
		deltaBody := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "c1",
				"function": map[string]any{"name": "run_command", "arguments": args},
			}},
		}
		if content != "" {
			deltaBody["content"] = content
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": deltaBody}}})
		return "data: " + string(b) + "\n\n"
	}
	streamed := sseServer(t, []string{fnChunk("答案", `{"x":`), fnChunk("", `1}`), "data: [DONE]\n\n"})

	m1, err := newTestClient(t, plain).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("非流式失败: %v", err)
	}
	m2, err := newTestClient(t, streamed).ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("流式失败: %v", err)
	}

	if m1.Content != m2.Content {
		t.Fatalf("正文不一致: %q vs %q", m1.Content, m2.Content)
	}
	if len(m1.ToolCalls) != len(m2.ToolCalls) {
		t.Fatalf("工具调用数量不一致: %d vs %d", len(m1.ToolCalls), len(m2.ToolCalls))
	}
	for i := range m1.ToolCalls {
		if m1.ToolCalls[i].ID != m2.ToolCalls[i].ID ||
			m1.ToolCalls[i].Function.Name != m2.ToolCalls[i].Function.Name ||
			m1.ToolCalls[i].Function.Arguments != m2.ToolCalls[i].Function.Arguments {
			t.Fatalf("第 %d 个工具调用不一致:\n 非流式 %+v\n 流式   %+v",
				i, m1.ToolCalls[i], m2.ToolCalls[i])
		}
	}
}

// ---- 兼容性与错误路径 ----

// 不少自建网关会忽略 stream 参数、直接返回一整个 JSON。
// 此时必须按非流式解析，而不是把 JSON 当 SSE 逐行啃。
func TestChatStreamFallsBackWhenServerIgnoresStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "整体返回"},
			}},
		})
	}))
	defer srv.Close()

	var frags []string
	msg, err := newTestClient(t, srv).ChatStream(context.Background(), nil, nil, func(d Delta) {
		frags = append(frags, d.Content)
	})
	if err != nil {
		t.Fatalf("网关忽略 stream 时不应失败: %v", err)
	}
	if msg.Content != "整体返回" {
		t.Fatalf("内容错误: %q", msg.Content)
	}
	// 界面仍应拿到一次增量，否则这条回答会完全不显示
	if strings.Join(frags, "") != "整体返回" {
		t.Fatalf("回退路径没有把内容推给界面: %v", frags)
	}
}

func TestChatStreamSurfacesErrorInsideStream(t *testing.T) {
	srv := sseServer(t, []string{
		delta("部分"),
		`data: {"error":{"message":"rate limited","type":"rate_limit"}}` + "\n\n",
	})
	c := newTestClient(t, srv)

	_, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("流内的 error 事件必须被当作失败")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("错误信息应包含服务端原因，实得: %v", err)
	}
}

func TestChatStreamRejectsEmptyStream(t *testing.T) {
	srv := sseServer(t, []string{"data: [DONE]\n\n"})
	c := newTestClient(t, srv)

	if _, err := c.ChatStream(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("只有 [DONE] 的空流应当报错，而不是返回空回答")
	}
}

func TestChatStreamRejectsAllUnparseableEvents(t *testing.T) {
	srv := sseServer(t, []string{
		"data: not-json\n\n",
		"data: also-not-json\n\n",
	})
	c := newTestClient(t, srv)

	_, err := c.ChatStream(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("全部无法解析时必须报错（该服务端可能不支持流式）")
	}
	if !strings.Contains(err.Error(), "不支持流式") {
		t.Fatalf("错误信息应给出可操作的提示，实得: %v", err)
	}
}

func TestChatStreamHTTPErrorRedactsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// 模拟把请求头回显进错误体的服务端
		fmt.Fprintf(w, `{"error":{"message":"invalid key: sk-secret-key"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ChatStream(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("401 应当报错")
	}
	if strings.Contains(err.Error(), "sk-secret-key") {
		t.Fatalf("错误信息泄露了 API Key: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("应当把 Key 替换为占位符，实得: %v", err)
	}
}

func TestChatStreamRespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		io.WriteString(w, delta("开始"))
		if fl != nil {
			fl.Flush()
		}
		<-release // 卡住，模拟服务端迟迟不继续
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := newTestClient(t, srv).ChatStream(ctx, nil, nil, nil)
	if err == nil {
		t.Fatal("上下文取消后应当返回错误")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("取消后没有及时返回 —— 中断功能会失效")
	}
}

func TestChatStreamRequiresConfig(t *testing.T) {
	c := New("", "", "")
	if _, err := c.ChatStream(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("未配置 Base URL 时应当报错")
	}
}

// ---- 空回复必须显式报错 ----

// 模型成功返回（HTTP 200）但正文为空时，绝不能当成合法的空回答放行：
// 那样 agent 会静默结束这一轮，前端渲染一条看不见的空消息，
// 用户看到的就是「发出去没响应」。finish_reason 能直接指出原因，必须带出来。
func TestEmptyReplyIsAnError(t *testing.T) {
	// 流式：只发 finish_reason，不给正文
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	_, err := newTestClient(t, srv).ChatStream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("流式空回复应报错并带 finish_reason，实得: %v", err)
	}

	// 网关忽略 stream、整体返回 JSON 的空回复
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}]}`)
	}))
	defer plain.Close()
	_, err = newTestClient(t, plain).ChatStream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("整体 JSON 空回复应报错并带 finish_reason，实得: %v", err)
	}

	// 非流式 Chat 同样处理
	_, err = newTestClient(t, plain).Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("非流式空回复应报错并带 finish_reason，实得: %v", err)
	}

	// 没有任何事件的空流也会报错（命中「未返回任何流式数据」，同样是显式失败）
	bare := sseServer(t, []string{"data: [DONE]\n\n"})
	if _, err = newTestClient(t, bare).ChatStream(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("只有 [DONE] 的空流应当报错")
	}
}

// 流式路径下「正文为空 + 工具调用」的聚合顺序必须正确：
// 空回复检查若放在工具调用聚合之前，会把每一次纯工具调用轮误判成空回复，
// agent 的多轮工具调用会全部退化为非流式重试（并多消耗一次请求）。
func TestChatStreamEmptyContentWithToolCallsIsNotAnEmptyReply(t *testing.T) {
	tc := func(index int, part string) string {
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": part}}},
				},
			}},
		})
		return "data: " + string(b) + "\n\n"
	}
	srv := sseServer(t, []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"run_command"}}]}}]}` + "\n\n",
		tc(0, `{"command":`),
		tc(0, `"ls"}`),
		"data: [DONE]\n\n",
	})
	msg, err := newTestClient(t, srv).ChatStream(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("空正文+工具调用的流式轮不应报空回复: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("工具调用聚合错误: %+v", msg.ToolCalls)
	}
}

// 纯工具调用轮（正文为空、只有 tool_calls）是 agent 的正常中间步骤，不能误伤。
func TestToolCallsWithoutContentIsNotAnEmptyReply(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call1",
					"type": "function",
					"function": map[string]any{
						"name":      "run_command",
						"arguments": `{"command":"ls"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	msg, err := newTestClient(t, srv).Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("纯工具调用轮不应报空回复: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "run_command" {
		t.Fatalf("工具调用解析错误: %+v", msg.ToolCalls)
	}
}

// 全空白正文等同于空回复：渲染出来同样不可见。
func TestWhitespaceOnlyReplyIsAnError(t *testing.T) {
	srv := sseServer(t, []string{delta("  \n  "), "data: [DONE]\n\n"})
	_, err := newTestClient(t, srv).ChatStream(context.Background(), nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "空回复") {
		t.Fatalf("全空白回复应报错，实得: %v", err)
	}
}
