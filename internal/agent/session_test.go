package agent

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"ai-shell/internal/llm"
	"ai-shell/internal/vault"
)

// ---- 会话上下文的单元测试 ----
//
// 这些用例只碰 sessions 这一块状态，不需要 vault/ssh/LLM，
// 所以直接 New(nil, nil, …) 就够了。端到端的串联在 agent_e2e_test.go 里。

// noEmit 是一个什么都不做的 Emitter。
func noEmit(string, any) {}

// defaultLimits 是测试里最常用的一组会话上限。与产品默认值一致，
// 保证用例跑在「用户没改过设置」的那条路上。
func defaultLimits() sessionLimits {
	return sessionLimits{turns: 8, toolByte: 8 << 10, total: 256 << 10}
}

// limitsWith 只覆盖关心的那一项，其余保持默认 —— 免得每个用例都要写全三个数。
func limitsWith(f func(*sessionLimits)) sessionLimits {
	l := defaultLimits()
	f(&l)
	return l
}

// simpleTurn 造一轮「user → assistant」的完整对话。
func simpleTurn(q, a string) []llm.Message {
	return []llm.Message{
		{Role: "user", Content: q},
		{Role: "assistant", Content: a},
	}
}

// toolTurn 造一轮带工具调用的完整对话：user → assistant(tool_calls) → tool → assistant。
// 这是最容易出错的一种轮次 —— 中间那条 tool 消息必须能对上 tool_call_id。
func toolTurn(q, toolID string, toolOut string) []llm.Message {
	return []llm.Message{
		{Role: "user", Content: q},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: toolID, Type: "function",
			Function: llm.FunctionCall{Name: "run_command", Arguments: `{"host_id":"h1","command":"uptime"}`},
		}}},
		{Role: "tool", ToolCallID: toolID, Name: "run_command", Content: toolOut},
		{Role: "assistant", Content: "结论：一切正常。"},
	}
}

// assertTurnsWellFormed 检查历史里每一轮都是一条**完整**的消息链。
//
// 这是本文件最要紧的一条不变量：OpenAI 协议要求每条 tool 消息前面必须有
// 携带同一 tool_call_id 的 assistant 消息。从中间切断（比如只保留后半轮）
// 会让下一轮请求直接 400 —— 而 400 在界面上只会显示成「模型调用失败」，
// 用户完全看不出是上下文裁剪切坏了。
func assertTurnsWellFormed(t *testing.T, hist []llm.Message) {
	t.Helper()
	if len(hist) == 0 {
		return
	}
	if hist[0].Role != "user" {
		t.Fatalf("历史的第一条必须是 user（否则就是从中间切断的），实得 %q", hist[0].Role)
	}
	if last := hist[len(hist)-1].Role; last != "assistant" {
		t.Fatalf("历史的最后一条必须是 assistant（终局回答），实得 %q", last)
	}

	pending := map[string]bool{} // 已发出、尚未收到结果的 tool_call_id
	for i, m := range hist {
		switch m.Role {
		case "user":
			if i != 0 && hist[i-1].Role != "assistant" {
				t.Fatalf("第 %d 条 user 前面应是 assistant（上一轮的终局回答），实得 %q", i, hist[i-1].Role)
			}
			if len(pending) != 0 {
				t.Fatalf("第 %d 条 user 出现时还有 %d 个工具调用没拿到结果", i, len(pending))
			}
		case "assistant":
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				t.Fatalf("第 %d 条 tool 消息的 tool_call_id=%q 没有对应的 assistant 工具调用", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		default:
			t.Fatalf("第 %d 条出现未知角色 %q", i, m.Role)
		}
	}
}

// 记进去再读出来，顺序与内容都要原样。
func TestRememberThenHistoryRoundTrip(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("第一问", "第一答"), defaultLimits())
	rememberDefault(a, "h1", simpleTurn("第二问", "第二答"), defaultLimits())

	hist := histDefault(a, "h1")
	if len(hist) != 4 {
		t.Fatalf("应保留 4 条消息，实得 %d 条", len(hist))
	}
	want := []string{"第一问", "第一答", "第二问", "第二答"}
	for i, w := range want {
		if hist[i].Content != w {
			t.Errorf("第 %d 条内容应为 %q，实得 %q", i, w, hist[i].Content)
		}
	}
	assertTurnsWellFormed(t, hist)
}

// remember 必须**复制**调用方传进来的切片。
//
// Run 里的 turn 是用 append 一路攒出来的，调用方（或以后重构后的代码）
// 完全可能在 remember 之后继续往里写。一旦共享底层数组，
// 历史里已经存下的那条消息会被悄悄改掉 —— 而且是下一轮才发作。
func TestRememberDoesNotAliasCallerSlice(t *testing.T) {
	a := New(nil, nil, noEmit)
	turn := simpleTurn("原始提问", "原始回答")
	rememberDefault(a, "h1", turn, defaultLimits())

	// 调用方随后改自己那份
	turn[0].Content = "被改过的提问"
	turn[1].Content = "被改过的回答"

	hist := histDefault(a, "h1")
	if hist[0].Content != "原始提问" {
		t.Fatalf("历史被调用方的后续修改污染了：%q", hist[0].Content)
	}
	if hist[1].Content != "原始回答" {
		t.Fatalf("历史被调用方的后续修改污染了：%q", hist[1].Content)
	}
}

// 空轮次不记，而且**连会话对象都不该建出来**。
//
// 后半句是这条用例真正要挡的东西：只断言「历史为空」是不够的 ——
// 哪怕存了一轮空消息，拼出来的历史照样是空的，
// 但 sessions 里会多出一个空壳条目，ClearSession 于是会报「清掉了 1 轮」，
// 界面显示「已清空 1 轮对话上下文」，而用户其实一轮都没聊过。
func TestRememberIgnoresEmptyTurn(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", nil, defaultLimits())
	rememberDefault(a, "h1", []llm.Message{}, defaultLimits())
	if hist := histDefault(a, "h1"); len(hist) != 0 {
		t.Fatalf("空轮次不该被记入，实得 %d 条", len(hist))
	}
	if len(a.sessions) != 0 {
		t.Fatalf("空轮次不该建出会话对象，sessions 里却有 %d 台主机", len(a.sessions))
	}
	if n, _ := a.ClearSession("h1", DefaultSessionID); n != 0 {
		t.Fatalf("空轮次不该让清空报出轮数，实得 %d", n)
	}
}

// 不完整的轮次一律拒收。这是本文件最要紧的一条防线。
//
// 一条悬空的 tool 消息（或一轮以 tool 结尾的链）会让该主机**此后每一次**
// 请求都 400 —— 而界面只显示「模型调用失败」。这种失败极难定位，
// 所以宁可在入口处丢掉半轮对话，也不能把它存进去。
func TestRememberRefusesIncompleteTurn(t *testing.T) {
	cases := []struct {
		name string
		turn []llm.Message
	}{
		{"只有一条 user", []llm.Message{{Role: "user", Content: "问"}}},
		{"以 tool 结尾", []llm.Message{
			{Role: "user", Content: "问"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.FunctionCall{Name: "run_command"}}}},
			{Role: "tool", ToolCallID: "c1", Content: "输出"},
		}},
		{"以带工具调用的 assistant 结尾", []llm.Message{
			{Role: "user", Content: "问"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.FunctionCall{Name: "run_command"}}}},
		}},
		{"不是以 user 开头", []llm.Message{
			{Role: "assistant", Content: "答"},
			{Role: "assistant", Content: "又答"},
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := New(nil, nil, noEmit)
			rememberDefault(a, "h1", c.turn, defaultLimits())
			if hist := histDefault(a, "h1"); len(hist) != 0 {
				t.Fatalf("不完整的轮次不该被记入，实得 %d 条: %v", len(hist), hist)
			}
		})
	}
}

// 没聊过的主机返回 nil，而不是一个空 slice。
// 两者对调用方（append 到 msgs）行为一样，但 nil 能让「有没有历史」这件事
// 在日志和调试里一眼看出来。
func TestHistoryForUnknownHostIsNil(t *testing.T) {
	a := New(nil, nil, noEmit)
	if hist := histDefault(a, "nobody"); hist != nil {
		t.Fatalf("未知主机应返回 nil，实得 %v", hist)
	}
}

// 每台主机各记各的，互不可见。这是本轮改动的核心诉求。
func TestSessionsAreIsolatedPerHost(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("A机的提问", "A机的回答"), defaultLimits())

	if hist := histDefault(a, "h2"); len(hist) != 0 {
		t.Fatalf("h2 不该看到 h1 的会话，实得 %d 条: %v", len(hist), hist)
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Fatalf("h1 的会话应完好，实得 %d 条", len(hist))
	}
}

// 清空返回被丢弃的轮数，并真的清掉。
func TestClearSessionDropsEverythingAndReportsCount(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())
	rememberDefault(a, "h1", simpleTurn("二", "2"), defaultLimits())

	n, busy := a.ClearSession("h1", DefaultSessionID)
	if busy {
		t.Fatal("没有会话在跑，不该报 busy")
	}
	if n != 2 {
		t.Fatalf("应报告清掉 2 轮，实得 %d", n)
	}
	if hist := histDefault(a, "h1"); len(hist) != 0 {
		t.Fatalf("清空后不该还有历史，实得 %d 条", len(hist))
	}

	// 再点一次：返回 0 —— 界面据此说「本来就没有上下文」，
	// 而不是让用户对着一个没反应的按钮猜。
	if n2, busy2 := a.ClearSession("h1", DefaultSessionID); n2 != 0 || busy2 {
		t.Fatalf("重复清空应返回 (0,false)，实得 (%d,%v)", n2, busy2)
	}
}

// 清空只影响那一台主机。
func TestClearSessionOnlyTouchesOneHost(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())
	rememberDefault(a, "h2", simpleTurn("二", "2"), defaultLimits())

	a.ClearSession("h1", DefaultSessionID)

	if hist := histDefault(a, "h1"); len(hist) != 0 {
		t.Fatal("h1 应被清空")
	}
	if hist := histDefault(a, "h2"); len(hist) != 2 {
		t.Fatalf("h2 不该被牵连，实得 %d 条", len(hist))
	}
}

// 有会话在跑时拒绝清空，并且**不能**顺手清掉 —— 拒绝就得是真拒绝。
//
// 为什么必须拒绝：那一轮跑完还会把消息 remember 回来，
// 于是用户看到「已清空」之后，模型下一轮**依然**记得刚才的对话。
// 谎报成功比直接说「现在不行」更糟。
func TestClearSessionRefusesWhileRunning(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())

	a.running = true // 模拟 Run 正在进行

	n, busy := a.ClearSession("h1", DefaultSessionID)
	if !busy {
		t.Fatal("运行中应报 busy=true")
	}
	if n != 0 {
		t.Fatalf("运行中不该报告清掉了任何轮次，实得 %d", n)
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Fatalf("被拒绝时历史必须原封不动，实得 %d 条", len(hist))
	}
}

// Forget 是「主机被删除」时用的无条件丢弃，跟 ClearSession 不同 ——
// 它没有 busy 检查，也不报告轮次。删主机那一刻没人想被问「现在有个会话在跑」，
// 而且删了之后该主机的会话本来就没意义了。
func TestForgetAlwaysDropsEvenWhileRunning(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())
	a.running = true

	a.Forget("h1")

	if hist := histDefault(a, "h1"); hist != nil {
		t.Fatalf("Forget 必须清掉历史，实得 %v", hist)
	}
	if _, ok := a.sessions["h1"]; ok {
		t.Fatal("sessions 里不该再有这个 key，否则内存里会留一个空壳条目")
	}
}

func TestForgetIsNoopForUnknownHost(t *testing.T) {
	a := New(nil, nil, noEmit)
	a.Forget("nobody") // 不该 panic，也不该影响别的
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())
	a.Forget("h2") // 未知主机：同上
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Fatalf("Forget 未知主机不该误伤 h1，实得 %d 条", len(hist))
	}
}

// 轮数超限时，**整轮**丢弃最旧的，绝不从中间切。
func TestTrimDropsWholeTurnsFromTheFront(t *testing.T) {
	limits := limitsWith(func(l *sessionLimits) { l.turns = 3 })

	a := New(nil, nil, noEmit)
	for i := 0; i < 6; i++ {
		rememberDefault(a, "h1", simpleTurn(fmt.Sprintf("提问%d", i), fmt.Sprintf("回答%d", i)), limits)
	}

	hist := histDefault(a, "h1")
	if len(hist) != 6 { // 3 轮 × 2 条
		t.Fatalf("应只保留最近 3 轮（6 条消息），实得 %d 条", len(hist))
	}
	if hist[0].Content != "提问3" {
		t.Fatalf("应保留最新 3 轮（从提问3 开始），实得首条 %q", hist[0].Content)
	}
	assertTurnsWellFormed(t, hist)
}

// 带工具调用的轮次被裁掉后，历史里不能留下孤立的 tool 消息。
//
// 这是「按轮裁剪」而不是「按消息条数裁剪」的理由：按条数裁会把一轮切成两半，
// 留下一条对不上 tool_call_id 的 tool 消息，下一轮请求直接 400。
func TestTrimKeepsToolPairsIntact(t *testing.T) {
	limits := limitsWith(func(l *sessionLimits) { l.turns = 2 })

	a := New(nil, nil, noEmit)
	for i := 0; i < 4; i++ {
		rememberDefault(a, "h1", toolTurn(fmt.Sprintf("提问%d", i), fmt.Sprintf("call%d", i), "uptime 输出"), limits)
	}

	hist := histDefault(a, "h1")
	if len(hist) != 8 { // 2 轮 × 4 条
		t.Fatalf("应保留 2 轮共 8 条消息，实得 %d 条", len(hist))
	}
	assertTurnsWellFormed(t, hist)
}

// 哪怕单轮就超了总字节上限，也要留住这一轮 —— 总比上下文整个清零好。
func TestTrimAlwaysKeepsAtLeastOneTurn(t *testing.T) {
	limits := limitsWith(func(l *sessionLimits) {
		l.total = 16
		l.turns = 100
	})

	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("长提问", strings.Repeat("内容", 200)), limits)
	rememberDefault(a, "h1", simpleTurn("第二问", "第二答"), limits)

	hist := histDefault(a, "h1")
	if len(hist) == 0 {
		t.Fatal("不该把上下文整个清零")
	}
	if hist[0].Content != "第二问" {
		t.Fatalf("应只留下最新一轮，实得首条 %q", hist[0].Content)
	}
	assertTurnsWellFormed(t, hist)
}

// 工具输出超上限时被裁剪，并**明确标注**。
//
// 不标注的话，模型会以为「这就是全部输出」，
// 进而像对待被截断的命令输出一样得出「后面没有问题」的错误结论。
func TestRememberClipsLongToolOutputWithNotice(t *testing.T) {
	a := New(nil, nil, noEmit)
	limits := defaultLimits()
	long := strings.Repeat("A", limits.toolByte+4096)
	rememberDefault(a, "h1", []llm.Message{
		{Role: "user", Content: "看日志"},
		{Role: "tool", ToolCallID: "c1", Content: long},
		{Role: "assistant", Content: "看完了"},
	}, limits)

	hist := histDefault(a, "h1")
	got := hist[1].Content
	if len(got) >= len(long) {
		t.Fatalf("超限的工具输出应被裁剪，实得 %d 字节（原 %d）", len(got), len(long))
	}
	if !strings.HasSuffix(got, sessionClipNotice) {
		t.Fatalf("裁剪后必须带可见的标注，实得结尾 %q", tail(got, 80))
	}
	if len(got) > limits.toolByte+len(sessionClipNotice) {
		t.Fatalf("裁剪后不该超过上限 + 标注长度，实得 %d", len(got))
	}
}

// 没超限的工具输出一个字都不动 —— 不能平白给每轮都加上标注。
func TestRememberLeavesShortToolOutputAlone(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", []llm.Message{
		{Role: "user", Content: "看负载"},
		{Role: "tool", ToolCallID: "c1", Content: "load average: 0.00"},
		{Role: "assistant", Content: "负载很低"},
	}, defaultLimits())
	if got := histDefault(a, "h1")[1].Content; got != "load average: 0.00" {
		t.Fatalf("未超限的输出不该被改动，实得 %q", got)
	}
}

// 只裁工具输出，不裁 user/assistant 的正文。
//
// 正文是人和模型自己写的（短且是对话的骨架），
// 会撑爆窗口的是命令回显这种大块远端数据。
func TestRememberDoesNotClipConversationText(t *testing.T) {
	a := New(nil, nil, noEmit)
	limits := defaultLimits()
	bigUser := strings.Repeat("问", limits.toolByte+100)
	rememberDefault(a, "h1", []llm.Message{
		{Role: "user", Content: bigUser},
		{Role: "assistant", Content: "答"},
	}, limits)
	if got := histDefault(a, "h1")[0].Content; len(got) != len(bigUser) {
		t.Fatalf("user 正文不该被裁，原 %d 字节实得 %d", len(bigUser), len(got))
	}
}

// 总字节数必须跟着裁剪一起减下去，否则每清一轮就白算一次、迟早漏算成负数。
func TestSessionBytesStayConsistentAfterTrim(t *testing.T) {
	limits := limitsWith(func(l *sessionLimits) { l.turns = 2 })

	a := New(nil, nil, noEmit)
	for i := 0; i < 5; i++ {
		rememberDefault(a, "h1", simpleTurn(fmt.Sprintf("提问%d", i), fmt.Sprintf("回答%d", i)), limits)
	}

	s := a.sessions["h1"][DefaultSessionID]
	want := 0
	for _, turn := range s.turns {
		want += turnSize(turn)
	}
	if s.bytes != want {
		t.Fatalf("bytes 应与实际内容一致：记录 %d，实际 %d", s.bytes, want)
	}
	if s.bytes < 0 {
		t.Fatalf("bytes 变成了负数：%d", s.bytes)
	}
}

// turnSize 必须把工具调用的名字与参数算进去 —— 那是单条消息里最大的一块，
// 漏掉它等于整个上限失效。
func TestTurnSizeCountsToolCallArguments(t *testing.T) {
	args := strings.Repeat("x", 500)
	turn := []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "run_command", Arguments: args},
	}}}}
	if got := turnSize(turn); got < 500 {
		t.Fatalf("turnSize 应把工具参数算进去，实得 %d", got)
	}

	// 单条消息的估算不能小于它的正文长度 —— 低估就等于上限失效。
	plain := []llm.Message{{Role: "user", Content: strings.Repeat("y", 300)}}
	if got := turnSize(plain); got < 300 {
		t.Fatalf("turnSize 不应低估正文，实得 %d", got)
	}
}

// ---- 上限来自设置 ----

// limitsFrom 必须如实取用设置里的值 —— 这是「用户可以调整上下文窗口」
// 这条功能的唯一接口。取错字段（比如把单条上限和总上限弄反）
// 会让用户调了半天没有效果。
func TestLimitsFromUsesPolicyValues(t *testing.T) {
	pol := vault.PolicySettings{
		MaxSessionTurns:    3,
		MaxStoredToolBytes: 4 << 10,
		MaxSessionBytes:    64 << 10,
	}
	l := limitsFrom(pol, "h1")
	if l.turns != 3 {
		t.Errorf("轮数上限应取 MaxSessionTurns=3，实得 %d", l.turns)
	}
	if l.toolByte != 4<<10 {
		t.Errorf("单条上限应取 MaxStoredToolBytes=4KiB，实得 %d", l.toolByte)
	}
	if l.total != 64<<10 {
		t.Errorf("总上限应取 MaxSessionBytes=64KiB，实得 %d", l.total)
	}
}

// 零值/负值必须兜底成默认值，绝不能退化成「一轮都不留」。
//
// 退化的后果是静默的：模型每轮都失忆，用户只会觉得「这模型怎么老忘事」，
// 不会想到是设置里的上下文上限被填成了 0。
func TestLimitsFromFallsBackForNonPositive(t *testing.T) {
	l := limitsFrom(vault.PolicySettings{}, "h1") // 全零
	if l.turns <= 0 || l.toolByte <= 0 || l.total <= 0 {
		t.Fatalf("全零设置必须兜底成默认值，实得 %+v", l)
	}

	l = limitsFrom(vault.PolicySettings{MaxSessionTurns: -1, MaxStoredToolBytes: -1, MaxSessionBytes: -1}, "h1")
	if l.turns <= 0 || l.toolByte <= 0 || l.total <= 0 {
		t.Fatalf("负值设置必须兜底成默认值，实得 %+v", l)
	}
}

// ---- 主机级覆盖 ----

// 同一份策略下，有覆盖的主机取覆盖值，没覆盖的主机取全局值。
//
// 这条防的是「覆盖表读进来了但查的是别的 key」—— 比如按主机名查而
// 存的是 ID。那种情况下两台机器会一起用全局值，界面上看不出异常。
func TestLimitsFromAppliesHostOverride(t *testing.T) {
	pol := vault.PolicySettings{
		MaxSessionTurns:    4,
		MaxStoredToolBytes: 8 << 10,
		MaxSessionBytes:    256 << 10,
		SessionOverrides: map[string]vault.SessionLimits{
			"deep": {MaxSessionTurns: 30},
		},
	}

	if got := limitsFrom(pol, "deep"); got.turns != 30 {
		t.Errorf("有覆盖的主机应取 30 轮，实得 %d", got.turns)
	}
	if got := limitsFrom(pol, "casual"); got.turns != 4 {
		t.Errorf("没覆盖的主机应取全局的 4 轮，实得 %d", got.turns)
	}
}

// 覆盖必须**逐项**生效：只想调轮数的主机，另外两项仍跟着全局走。
//
// 若实现改成「整块替换」，另外两项会变成 0 再被兜底成默认值 ——
// 表现为「这台机器的单条工具输出上限被悄悄重置成 8KiB」，
// 而且之后改全局也带不动它，用户完全看不出是哪一步弄丢的。
func TestLimitsFromOverrideIsPerField(t *testing.T) {
	pol := vault.PolicySettings{
		MaxSessionTurns:    4,
		MaxStoredToolBytes: 64 << 10,
		MaxSessionBytes:    1 << 20,
		SessionOverrides: map[string]vault.SessionLimits{
			"h1": {MaxSessionTurns: 30},
		},
	}

	l := limitsFrom(pol, "h1")
	if l.turns != 30 {
		t.Errorf("轮数应取覆盖值 30，实得 %d", l.turns)
	}
	if l.toolByte != 64<<10 {
		t.Errorf("单条上限未被覆盖，应继承全局 64KiB，实得 %d", l.toolByte)
	}
	if l.total != 1<<20 {
		t.Errorf("总量上限未被覆盖，应继承全局 1024KiB，实得 %d", l.total)
	}
}

// 覆盖项里的零值表示「继承」，不是「上限为 0」。
func TestLimitsFromOverrideZeroInheritsGlobal(t *testing.T) {
	pol := vault.PolicySettings{
		MaxSessionTurns:    5,
		MaxStoredToolBytes: 32 << 10,
		MaxSessionBytes:    128 << 10,
		SessionOverrides: map[string]vault.SessionLimits{
			// 只调总量，另两项留零。
			"h1": {MaxSessionBytes: 2 << 20},
			// 三项全空的条目（vault 层会规整掉，这里直接构造以覆盖
			// 「规整漏了」的情况）。
			"h2": {},
		},
	}

	l := limitsFrom(pol, "h1")
	if l.turns != 5 || l.toolByte != 32<<10 || l.total != 2<<20 {
		t.Errorf("h1 应为 5 轮 / 32KiB / 2048KiB，实得 %d / %d / %d", l.turns, l.toolByte, l.total)
	}

	// 全零条目必须与「完全没有条目」等价：否则这台机器在界面上
	// 看不出设过什么，实际却偏离了全局设置。
	if a, b := limitsFrom(pol, "h2"), limitsFrom(pol, "h3"); a != b {
		t.Errorf("全零覆盖应与没有覆盖一致：h2=%+v h3=%+v", a, b)
	}
}

// 全局值为 0（老配置）而该主机有覆盖时，未被覆盖的那两项仍须兜底。
//
// 兜底若写在合并**之前**，这里 toolByte 会停在 0 —— 单条工具输出
// 一律裁成空串，模型看到的每条命令结果都是空的，而界面一切正常。
func TestLimitsFromOverrideStillFallsBackWhenGlobalIsZero(t *testing.T) {
	pol := vault.PolicySettings{
		SessionOverrides: map[string]vault.SessionLimits{
			"h1": {MaxSessionTurns: 12},
		},
	}

	l := limitsFrom(pol, "h1")
	if l.turns != 12 {
		t.Errorf("轮数应取覆盖值 12，实得 %d", l.turns)
	}
	if l.toolByte <= 0 || l.total <= 0 {
		t.Fatalf("未被覆盖的项在全局为 0 时必须兜底成默认值，实得 %+v", l)
	}
}

// 端到端（不经 Run）：同一份策略、同样的写入序列，
// 被覆盖的主机必须留下更多历史，未覆盖的按全局走。
func TestSessionLimitsPerHostAreIndependent(t *testing.T) {
	pol := vault.PolicySettings{
		MaxSessionTurns:    2,
		MaxStoredToolBytes: 8 << 10,
		MaxSessionBytes:    256 << 10,
		SessionOverrides: map[string]vault.SessionLimits{
			"deep": {MaxSessionTurns: 6},
		},
	}

	a := New(nil, nil, noEmit)
	for i := 0; i < 6; i++ {
		for _, h := range []string{"deep", "casual"} {
			rememberDefault(a, h, simpleTurn(
				fmt.Sprintf("%s-问%d", h, i),
				fmt.Sprintf("%s-答%d", h, i),
			), limitsFrom(pol, h))
		}
	}

	// 全局 2 轮 → 4 条消息；覆盖 6 轮 → 12 条消息。
	if got := len(histDefault(a, "casual")); got != 4 {
		t.Errorf("未覆盖的主机应留 2 轮 = 4 条消息，实得 %d", got)
	}
	if got := len(histDefault(a, "deep")); got != 12 {
		t.Errorf("覆盖的主机应留 6 轮 = 12 条消息，实得 %d", got)
	}

	// 两台主机的历史不能互相串味 —— 这是「按主机独立」的底线。
	for _, m := range histDefault(a, "deep") {
		if strings.Contains(m.Content, "casual") {
			t.Fatalf("deep 的历史里混进了 casual 的内容: %q", m.Content)
		}
	}
}

// 端到端地验证「设置真的会改变裁剪行为」：
// 同样的写入序列，小的上限必须留下更少的历史。
//
// 这条用例防的是「设置读进来了、也存下去了，但 Agent 还在用写死的默认值」——
// 那种情况下前两条用例都过，用户改了设置却毫无效果。
func TestSessionLimitsFromSettingsActuallyChangeTrimming(t *testing.T) {
	tight := limitsFrom(vault.PolicySettings{MaxSessionTurns: 2, MaxStoredToolBytes: 8 << 10, MaxSessionBytes: 256 << 10}, "h1")
	loose := limitsFrom(vault.PolicySettings{MaxSessionTurns: 6, MaxStoredToolBytes: 8 << 10, MaxSessionBytes: 256 << 10}, "h1")

	a := New(nil, nil, noEmit)
	for i := 0; i < 6; i++ {
		rememberDefault(a, "tight", simpleTurn(fmt.Sprintf("提问%d", i), fmt.Sprintf("回答%d", i)), tight)
	}
	b := New(nil, nil, noEmit)
	for i := 0; i < 6; i++ {
		rememberDefault(b, "loose", simpleTurn(fmt.Sprintf("提问%d", i), fmt.Sprintf("回答%d", i)), loose)
	}

	gotTight := len(histDefault(a, "tight"))
	gotLoose := len(histDefault(b, "loose"))
	if gotTight != 4 { // 2 轮 × 2 条
		t.Fatalf("上限 2 轮应留 4 条，实得 %d", gotTight)
	}
	if gotLoose != 12 { // 6 轮 × 2 条
		t.Fatalf("上限 6 轮应留 12 条，实得 %d", gotLoose)
	}
	if gotTight >= gotLoose {
		t.Fatalf("更紧的上限必须留下更少的历史：tight=%d loose=%d", gotTight, gotLoose)
	}
}

// ---- 截断工具函数 ----

// 按字节截断时**不能**把多字节字符切成半个 —— 那会渲染成 U+FFFD（乱码），
// 审计日志里的中文会变成一串问号。
func TestTruncateRunesDoesNotSplitMultibyte(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"你好世界", 5, "你"},     // 5 落在「好」的中间
		{"你好世界", 6, "你好"},    // 正好切在边界上
		{"abc", 10, "abc"},   // 没超限
		{"abc", 3, "abc"},    // 正好等于上限
		{"abc", 0, "abc"},    // max<=0 表示不限制
		{"", 5, ""},          // 空串
		{"a你b", 2, "a"},      // 2 落在「你」的中间
		{"你好世界", 12, "你好世界"}, // 超过总长
	}
	for _, c := range cases {
		got := truncateRunes(c.in, c.max)
		if got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q，期望 %q", c.in, c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateRunes(%q, %d) 产生了非法 UTF-8：%q", c.in, c.max, got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("truncateRunes(%q, %d) 切出了替换字符（乱码）：%q", c.in, c.max, got)
		}
	}
}

func TestClipTextMarksTruncation(t *testing.T) {
	long := strings.Repeat("问", 100)
	got := clipText(long, 30)
	if len(got) <= 30 {
		t.Fatalf("截断后应带上省略号，实得 %d 字节", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("应带省略号，实得 %q", got)
	}
	if utf8.RuneCountInString(got) != utf8.RuneCountInString(truncateRunes(long, 30))+1 {
		t.Fatalf("省略号之外不该多出内容：%q", got)
	}

	// 没超限就不加省略号 —— 否则每条审计记录都带个「…」，看不出哪条真被截了。
	if got := clipText("短提问", 500); got != "短提问" {
		t.Fatalf("未超限不该加省略号，实得 %q", got)
	}
}

func TestClipForHistoryOnlyMarksWhenTruncated(t *testing.T) {
	if got := clipForHistory("short", 100); got != "short" {
		t.Fatalf("未超限不该加标注，实得 %q", got)
	}
	got := clipForHistory(strings.Repeat("z", 50), 10)
	if !strings.HasSuffix(got, sessionClipNotice) {
		t.Fatalf("超限应加标注，实得 %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("z", 10)) {
		t.Fatalf("标注之外应保留开头，实得 %q", got)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
