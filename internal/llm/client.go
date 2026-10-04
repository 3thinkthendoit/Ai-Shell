// Package llm 是一个最小实现的 OpenAI 兼容 Chat Completions 客户端。
//
// 之所以只依赖标准库：这个包里会流经 API Key，依赖越少攻击面越小。
// 兼容性：任何实现 /v1/chat/completions 与 function calling 的服务都能接
// （OpenAI、Azure OpenAI、DeepSeek、通义、vLLM、Ollama、LM Studio …）。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role      string `json:"role"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning_content,omitempty"` // 推理型模型的思考内容（仅展示用，不入上下文）
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ToolCall 是模型请求调用某个工具。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具调用的具体内容。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串
}

// Tool 是暴露给模型的工具声明。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述一个函数的签名。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Client 是 LLM 客户端。
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// New 创建客户端。apiKey 可为空（本地模型通常不需要）。
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: NormalizeBaseURL(baseURL),
		apiKey:  apiKey,
		model:   model,
		http: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

// NormalizeBaseURL 规范化用户填写的 Base URL。
//
// 本客户端自己会追加 /chat/completions，所以 Base URL 应该填到 /v1 为止。
// 但「把文档里那个完整端点粘进来」是极常见的手滑 —— 一旦发生，请求会打到
// .../v1/chat/completions/chat/completions 而返回 404，
// 而错误信息完全不会提示你「地址填多了」。
//
// 这里顺手把这个尾巴剥掉。多剥一层比让用户对着 404 猜半天划算得多。
func NormalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/chat/completions")
	s = strings.TrimRight(s, "/")
	return s
}

// Model 返回当前模型名。
func (c *Client) Model() string { return c.model }

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// streamChunk 是流式响应中的一个 SSE 事件载荷。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			// 推理型模型的思考增量：DeepSeek 用 reasoning_content，
			// OpenRouter 等用 reasoning —— 两个都收，谁有值用谁。
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Delta 是流式响应的一段增量。
type Delta struct {
	Content   string
	Reasoning string
}

// EmptyReplyError 表示模型成功返回但内容为空。
//
// 单独成一个类型而不是裸 error：agent 的 chatOnce 依赖错误形状决定
// 是否退回非流式重试——空回复说明 SSE 流本身工作正常（HTTP 200、
// 事件都能解析），重试只会白白多花一次请求和两倍延迟，必须能区分开。
type EmptyReplyError struct {
	FinishReason string
}

func (e *EmptyReplyError) Error() string {
	if e.FinishReason != "" {
		return fmt.Sprintf("模型返回了空回复（finish_reason: %s），请检查模型配置或稍后重试", e.FinishReason)
	}
	return "模型返回了空回复，请检查模型配置或稍后重试"
}

// emptyReplyErr 把「模型成功返回但内容为空」整理成显式错误。
//
// 空回复若被当成合法的终局回答放行，agent 会静默结束这一轮：
// 前端渲染一条空消息，用户看到的就是「发出去没反应」，连排查入口都没有。
// finish_reason 往往能直接指出原因（content_filter / length …），必须带出来。
// 只有正文为空且**没有任何工具调用**才算空回复——纯工具调用轮返回空正文是正常的。
func emptyReplyErr(msg Message, finish string) error {
	if strings.TrimSpace(msg.Content) != "" || len(msg.ToolCalls) > 0 {
		return nil
	}
	return &EmptyReplyError{FinishReason: finish}
}

// newRequest 构造一次 chat/completions 请求。stream 为 true 时要求服务端以 SSE 返回。
func (c *Client) newRequest(ctx context.Context, msgs []Message, tools []Tool, stream bool) (*http.Request, error) {
	body := chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.2,
		Stream:      stream,
	}
	if len(tools) > 0 {
		body.Tools = tools
		body.ToolChoice = "auto"
	}
	return c.newRequestWith(ctx, body)
}

// newRequestWith 用给定的请求体构造 HTTP 请求。
// 单独拆出来是因为 Ping 需要自己控制 max_tokens，不能走 newRequest 的固定形状。
func (c *Client) newRequestWith(ctx context.Context, body chatRequest) (*http.Request, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("未配置 LLM Base URL")
	}
	if c.model == "" {
		return nil, fmt.Errorf("未配置 LLM 模型名")
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if body.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// PingResult 是一次连通性探测的结果。
type PingResult struct {
	URL     string        // 实际请求的地址（不含密钥，可安全展示）
	Model   string        // 请求时使用的模型名
	Status  int           // HTTP 状态码；0 表示连请求都没发出去
	Reply   string        // 模型返回的正文（截断后）
	Elapsed time.Duration // 往返耗时
}

// Ping 发一次最小请求，验证配置是否**真的**可用。
//
// 刻意不做成「只检查配置形状」：形状对但地址错是最常见的情况
// （把完整的 /v1/chat/completions 填进 Base URL 就是典型），
// 只有真发一次请求才能发现。max_tokens 压到很小，避免产生费用或长输出。
//
// 返回的 error 是给用户看的；调用方可以用 Status 区分「HTTP 错误」与「压根没连上」。
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	req, err := c.newRequestWith(ctx, chatRequest{
		Model:       c.model,
		Messages:    []Message{{Role: "user", Content: "ping"}},
		Temperature: 0,
		MaxTokens:   8,
	})
	if err != nil {
		return PingResult{}, err
	}

	start := time.Now()
	res := PingResult{URL: req.URL.String(), Model: c.model}

	resp, err := c.http.Do(req)
	if err != nil {
		res.Elapsed = time.Since(start)
		return res, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		res.Elapsed = time.Since(start)
		return res, fmt.Errorf("读取响应失败: %w", err)
	}
	res.Status = resp.StatusCode
	res.Elapsed = time.Since(start)

	if resp.StatusCode != http.StatusOK {
		return res, c.redactErr(resp.StatusCode, data)
	}

	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return res, fmt.Errorf("响应不是合法的 JSON（地址可能指向了非 LLM 服务）: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return res, fmt.Errorf("LLM 返回错误: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return res, fmt.Errorf("响应里没有 choices 字段（地址可能指向了非 LLM 服务）")
	}
	reply := strings.TrimSpace(out.Choices[0].Message.Content)
	if len(reply) > 200 {
		reply = reply[:200] + "…"
	}
	res.Reply = reply
	return res, nil
}

// redactErr 把错误响应体整理成可安全展示的信息（剔除可能被回显的 API Key）。
//
// 截断上限从 500 放宽到 2000：网关的报错往往是嵌套 JSON（外层包一次上游原文），
// 真正有用的那句「为什么被拒」常在第 600 字符之后 —— 原样截到 500 时，
// 用户看到的是一串没头没尾的转义引号，等于什么都没说。
func (c *Client) redactErr(status int, data []byte) error {
	msg := strings.TrimSpace(string(data))
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	if c.apiKey != "" {
		msg = strings.ReplaceAll(msg, c.apiKey, "[REDACTED]")
	}
	return fmt.Errorf("LLM 返回 %d: %s", status, msg)
}

// Chat 发送一轮对话，返回模型的回复（可能包含 tool_calls）。
//
// 非流式调用。保留它是为了兼容不支持 SSE 的服务端（不少自建网关会忽略 stream 参数），
// ChatStream 在完全没收到增量就失败时会退回这里。
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error) {
	req, err := c.newRequest(ctx, msgs, tools, false)
	if err != nil {
		return Message{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Message{}, fmt.Errorf("读取 LLM 响应失败: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Message{}, c.redactErr(resp.StatusCode, data)
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return Message{}, fmt.Errorf("解析 LLM 响应失败: %w", err)
	}
	if cr.Error != nil {
		return Message{}, fmt.Errorf("LLM 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Message{}, fmt.Errorf("LLM 未返回任何候选结果")
	}
	msg := cr.Choices[0].Message
	if err := emptyReplyErr(msg, cr.Choices[0].FinishReason); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// ChatStream 以流式方式发送一轮对话。
//
// onDelta 会随响应到达被**同步**调用（可为 nil），用于把正文增量推给界面。
// 返回值是**聚合完成**的完整消息，与 Chat 完全同构 ——
// 也就是说流式只是传输层的事，调用方后续的工具调用逻辑一行都不用改。
func (c *Client) ChatStream(ctx context.Context, msgs []Message, tools []Tool, onDelta func(Delta)) (Message, error) {
	req, err := c.newRequest(ctx, msgs, tools, true)
	if err != nil {
		return Message{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("请求 LLM 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Message{}, c.redactErr(resp.StatusCode, data)
	}

	// 有些兼容网关会忽略 stream 参数、直接返回一整个 JSON。
	// 这种情况下按非流式解析，而不是把 JSON 当 SSE 逐行啃。
	ctype := resp.Header.Get("Content-Type")
	if !strings.Contains(ctype, "event-stream") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return Message{}, fmt.Errorf("读取 LLM 响应失败: %w", err)
		}
		var cr chatResponse
		if err := json.Unmarshal(data, &cr); err != nil {
			return Message{}, fmt.Errorf("服务端未以 SSE 返回，且响应不是合法 JSON（Content-Type: %s）", ctype)
		}
		if cr.Error != nil {
			return Message{}, fmt.Errorf("LLM 错误: %s", cr.Error.Message)
		}
		if len(cr.Choices) == 0 {
			return Message{}, fmt.Errorf("LLM 未返回任何候选结果")
		}
		msg := cr.Choices[0].Message
		if err := emptyReplyErr(msg, cr.Choices[0].FinishReason); err != nil {
			return Message{}, err
		}
		if onDelta != nil {
			if msg.Reasoning != "" {
				onDelta(Delta{Reasoning: msg.Reasoning})
			}
			if msg.Content != "" {
				onDelta(Delta{Content: msg.Content})
			}
		}
		return msg, nil
	}

	return c.consumeSSE(ctx, resp.Body, onDelta)
}

// consumeSSE 解析 SSE 流并聚合出完整消息。
func (c *Client) consumeSSE(ctx context.Context, body io.Reader, onDelta func(Delta)) (Message, error) {
	type accum struct {
		id, name string
		args     strings.Builder
	}
	var (
		content      strings.Builder
		tools        = map[int]*accum{}
		unparsed     int
		events       int
		doneFlag     bool
		errInBody    error
		finishReason string
	)

	dispatch := func(payload string) bool {
		if payload == "[DONE]" {
			doneFlag = true
			return false
		}
		events++
		var ch streamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			// 部分服务端会插入非 JSON 的心跳行，不应因此中断整个流。
			unparsed++
			return true
		}
		if ch.Error != nil {
			errInBody = fmt.Errorf("LLM 错误: %s", ch.Error.Message)
			return false
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			if d := choice.Delta.Content; d != "" {
				content.WriteString(d)
			}
			// 推理增量不进正文聚合，只透传给界面
			r := choice.Delta.ReasoningContent
			if r == "" {
				r = choice.Delta.Reasoning
			}
			if onDelta != nil {
				d := Delta{Content: choice.Delta.Content, Reasoning: r}
				if d.Content != "" || d.Reasoning != "" {
					onDelta(d)
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &accum{}
					tools[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				// 工具参数是逐段拼接的 JSON 字符串，必须按序累加
				acc.args.WriteString(tc.Function.Arguments)
			}
		}
		return true
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	var dataLines []string
	flushEvent := func() bool {
		if len(dataLines) == 0 {
			return true
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		return dispatch(payload)
	}

	for sc.Scan() {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		line := sc.Text()
		if line == "" {
			// 空行 = 一个事件结束
			if !flushEvent() {
				break
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释 / 心跳
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue // event / id / retry 等字段本场景用不到
		}
		dataLines = append(dataLines, strings.TrimPrefix(value, " "))
	}

	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("读取流式响应失败: %w", err)
	}
	// 流末尾可能没有空行，补一次收尾
	if !doneFlag {
		flushEvent()
	}

	if errInBody != nil {
		return Message{}, errInBody
	}
	if events == 0 {
		return Message{}, fmt.Errorf("LLM 未返回任何流式数据")
	}
	if unparsed == events {
		return Message{}, fmt.Errorf("流式响应全部无法解析（共 %d 个事件），该服务端可能不支持流式输出", events)
	}

	msg := Message{Role: "assistant", Content: content.String()}
	// 工具调用的 index 理论上从 0 连续，但不依赖这个假设
	idxs := make([]int, 0, len(tools))
	for i := range tools {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		acc := tools[i]
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:       acc.id,
			Type:     "function",
			Function: FunctionCall{Name: acc.name, Arguments: acc.args.String()},
		})
	}
	// 空回复检查必须放在工具调用聚合**之后**：纯工具调用轮正文为空是正常的，
	// 提前检查会把每一次工具调用都误判成空回复。
	if err := emptyReplyErr(msg, finishReason); err != nil {
		return Message{}, err
	}
	return msg, nil
}
