package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-shell/internal/agent"
	"ai-shell/internal/vault"
)

// 本文件锁住 App 层的**会话绑定**（ListSessions / CreateSession / RenameSession /
// DeleteSession，以及 Ask / ClearSession / CompactSession 上的会话 ID 参数）。
//
// 为什么值得单独写：这一层是纯粹的转发，看起来「薄得不需要测」——
// 但它恰恰是最容易静默出错的一层。`Ask` 若把 sessionID 丢掉（或写成空串），
// **所有**会话的对话都会落进默认会话：界面上没有任何异常，
// 用户只会觉得「我明明选的是『nginx 排查』，内容却跑到了别的地方」。
// 而 agent 层与前端测试都断言不到这个接缝 —— 前端的用例只检查
// 「传了正确的参数」，传进去之后有没有被用上，只有这里能验。
//
// 实测：把 `a.ag.Run(a.ctx, hostID, sessionID, prompt)` 改成
// `a.ag.Run(a.ctx, hostID, "", prompt)`，在本文件出现之前没有任何用例变红。

// newSessionTestApp 装配一个能真的跑完一轮对话的 App。
//
// 与 newTestApp 的区别：那个只用到 a.v（测「测试连接」），
// 这里的会话绑定要落到 agent 上，所以必须把 agent 也接起来，
// 并把 LLM 指向一个假服务端 —— 否则 Run 会在「未配置 API Key」处提前返回，
// 会话永远不会有内容，用例就成了空转。
func newSessionTestApp(t *testing.T, fake *fakeLLM) *App {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")

	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开临时凭证库失败: %v", err)
	}
	key := "sk-test-0123456789"
	if err := v.SetLLM(fake.URL+"/v1", "test-model", &key); err != nil {
		t.Fatalf("写入 LLM 配置失败: %v", err)
	}

	a := &App{v: v}
	// ctx 照抄 startup() 的赋值：Ask 是异步的，它把 a.ctx 交给
	// 后台 goroutine，nil 会在 context.WithCancel 里直接 panic。
	// 这里给 Background 就够 —— 用例不测取消。
	a.ctx = context.Background()
	// 注意 emitter 传的是收集函数、**不是** a.emit：
	// a.emit 会调到 runtime.EventsEmit，而测试进程里没有 Wails 的
	// 生命周期 context，它会直接判为非法并中断用例。会话绑定这件事
	// 只跟 agent 的状态有关，跟事件通道无关，所以收下来即可
	// （收下来还有个好处：将来要断言「切会话时发过哪些事件」不必再改夹具）。
	sink := &eventSink{}
	// ssh 传 nil：这些用例里模型只回一句纯文本（不带 tool_calls），
	// 走不到任何需要 SSH 的路径。真给了反而会掩盖「不该有工具调用」这件事。
	a.ag = agent.New(v, nil, sink.emit)
	// 会话落盘也接上，跟 startup() 保持一致 —— 少了它，
	// 「新建/删除立刻落盘」这条路径在 App 层就成了盲区。
	if err := a.ag.SetSessionStore(agent.NewSessionStore(v.Dir(), v)); err != nil {
		t.Fatalf("注入会话落盘失败: %v", err)
	}
	return a
}

// waitFor 轮询直到条件成立。
//
// 刻意**不**用「等 a.ag.Running() 变 false」做屏障：Ask 起的是 goroutine，
// Ask 返回时那一轮很可能还没开始跑，此时 Running() 恰好是 false ——
// 屏障会立刻放行，断言就跑在对话开始之前，得到「什么都没发生」的假象。
// 直接等「期望的状态出现」既没有这个竞态，失败信息也更贴近意图。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（10s）", what)
}

// turnsOf 读某条会话当前的轮数（-1 表示它不在列表里）。
func turnsOf(a *App, hostID, sessionID string) int {
	for _, s := range a.ag.ListSessions(hostID) {
		if s.ID == sessionID {
			return s.Turns
		}
	}
	return -1
}

// 一轮对话必须落进**指定的**那条会话。
func TestAskBindingRoutesToTheGivenSession(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	if err := a.Ask("h1", "s1", "nginx 起不来了"); err != nil {
		t.Fatalf("Ask 不该同步报错: %v", err)
	}
	waitFor(t, "s1 收到 1 轮", func() bool { return turnsOf(a, "h1", "s1") == 1 })

	if n := turnsOf(a, "h1", agent.DefaultSessionID); n != 0 {
		t.Errorf("默认会话不该被写入 —— 说明会话 ID 在 App 层被丢掉了，实得 %d 轮", n)
	}
}

// 空会话 ID 落到默认会话上：前端不必知道默认会话的 ID 叫什么，
// 也就不会出现「前端写死了一个 ID、后端改了常量」这种静默错位。
func TestAskBindingEmptySessionIDLandsOnDefault(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	if err := a.Ask("h1", "", "看看磁盘"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "默认会话收到 1 轮", func() bool {
		return turnsOf(a, "h1", agent.DefaultSessionID) == 1
	})
}

// 空 prompt 在 App 层就被拦下，不打后端。
//
// 放过去的话，agent 会先建好会话、再因为没有可发的内容而失败，
// 用户会看到一个凭空多出来的空会话。
func TestAskBindingRejectsBlankPrompt(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	for _, p := range []string{"", "   ", "\n\t"} {
		if err := a.Ask("h1", "s1", p); err == nil {
			t.Errorf("空 prompt %q 应被拒绝", p)
		}
	}
	if n := turnsOf(a, "h1", "s1"); n > 0 {
		t.Errorf("被拒绝的提问不该留下记录，实得 %d 轮", n)
	}
}

// 清空只作用于指定的那条会话。
//
// 界面上「清空上下文」按钮紧挨着会话选择器，用户点它时想的是
// 「把当前这条清掉」—— 若后端清的是默认会话，他会发现当前这段一点没变，
// 而另一条莫名其妙空了。
func TestClearSessionBindingOnlyClearsThatSession(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))
	fill(t, a, "s1", "第一段")
	fill(t, a, "s2", "第二段")

	res := a.ClearSession("h1", "s1")
	if res.Cleared != 1 || res.Busy {
		t.Fatalf("清空应返回 (1,false)，实得 (%d,%v)", res.Cleared, res.Busy)
	}
	if n := turnsOf(a, "h1", "s1"); n != 0 {
		t.Errorf("s1 应被清空，实得 %d 轮", n)
	}
	if n := turnsOf(a, "h1", "s2"); n != 1 {
		t.Errorf("s2 不该被动 —— 说明会话 ID 没被透传，实得 %d 轮", n)
	}
}

// 压缩同样只作用于指定的那条会话，而且它是**花一次 API 调用**换来的 ——
// 压错会话等于白花，用户还会以为当前这段已经被压过了。
func TestCompactSessionBindingOnlyCompactsThatSession(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))
	fill(t, a, "s1", "第一段")
	fill(t, a, "s2", "第二段")

	res, err := a.CompactSession("h1", "s1")
	if err != nil {
		t.Fatalf("压缩不该报错: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("应压缩成功，实得 %+v", res)
	}
	if n := turnsOf(a, "h1", "s1"); n != 0 {
		t.Errorf("s1 压缩后当前轮数应为 0，实得 %d", n)
	}
	if n := turnsOf(a, "h1", "s2"); n != 1 {
		t.Errorf("s2 不该被动 —— 说明会话 ID 没被透传，实得 %d 轮", n)
	}
}

// 会话的增删改查四个绑定要能串起来用。
//
// 串起来测而不是各测各的：这四个操作共享同一份会话表，
// 单测每一个都绿、合起来却错位（比如新建返回的 ID 与列表里的对不上）
// 是完全可能的，而那正是界面会踩到的形态。
func TestSessionCrudBindings(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	// 全新主机也该有一条默认会话 —— 这是不变式，也是选择器永远有内容的原因。
	list := a.ListSessions("h1")
	if len(list) != 1 {
		t.Fatalf("全新主机应只有一条默认会话，实得 %+v", list)
	}
	if list[0].ID != agent.DefaultSessionID || !list[0].IsDefault {
		t.Fatalf("那条应是默认会话，实得 %+v", list[0])
	}

	info, err := a.CreateSession("h1", "nginx 排查")
	if err != nil {
		t.Fatalf("新建会话失败: %v", err)
	}
	if info.Name != "nginx 排查" || info.IsDefault {
		t.Errorf("新建结果不对: %+v", info)
	}
	// 新建返回的 ID 必须能在列表里找到 —— 界面立刻要用它去切会话。
	if turnsOf(a, "h1", info.ID) != 0 {
		t.Errorf("新建的会话没出现在列表里: %+v", a.ListSessions("h1"))
	}

	if err := a.RenameSession("h1", info.ID, "nginx 与证书"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	var renamed string
	for _, s := range a.ListSessions("h1") {
		if s.ID == info.ID {
			renamed = s.Name
		}
	}
	if renamed != "nginx 与证书" {
		t.Errorf("改名没生效，实得 %q", renamed)
	}

	if err := a.DeleteSession("h1", info.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if list = a.ListSessions("h1"); len(list) != 1 || list[0].ID != agent.DefaultSessionID {
		t.Fatalf("删完之后应只剩默认会话，实得 %+v", list)
	}
}

// 默认会话删不掉，且原因要说清楚。
//
// 这里**不**在 App 层再判一次 —— 规则只在 agent 那一处实现。
// 但错误必须原样透传，否则用户点了「删除」只看到一句没头没尾的报错。
func TestDeleteSessionBindingRefusesDefault(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	err := a.DeleteSession("h1", agent.DefaultSessionID)
	if err == nil {
		t.Fatal("默认会话不该被删掉")
	}
	if !strings.Contains(err.Error(), "默认会话不能删除") {
		t.Errorf("错误里应说清原因，实得 %q", err.Error())
	}
}

// vault 没就绪时，会话类绑定不能崩，也不能假装成功。
//
// 返回值的差别是有意的：ListSessions 返回 nil（空列表是合理的降级），
// 而增删改必须返回 error —— 静默成功会让界面显示「已删除」
// 而实际什么都没发生，用户会反复点。
func TestSessionBindingsRequireVault(t *testing.T) {
	a := &App{} // 未初始化：v 与 ag 都是 nil

	if got := a.ListSessions("h1"); got != nil {
		t.Errorf("未就绪时列表应为 nil，实得 %+v", got)
	}
	if _, err := a.CreateSession("h1", "x"); err == nil {
		t.Error("未就绪时新建会话应报错")
	}
	if err := a.RenameSession("h1", "s1", "x"); err == nil {
		t.Error("未就绪时改名应报错")
	}
	if err := a.DeleteSession("h1", "s1"); err == nil {
		t.Error("未就绪时删除应报错")
	}
	if err := a.Ask("h1", "s1", "x"); err == nil {
		t.Error("未就绪时 Ask 应报错")
	}
}

// 失败必须原样报出来，不能被吞成「成功」。
//
// 这一条是补上来的：先前的用例只走「改名成功」这条路，而成功时名字
// 确实变了 —— 吞不吞错误完全看不出来。变异测试里
// 「RenameSession 吞掉错误」因此存活。失败路径才是这个绑定的价值所在：
// 界面拿不到 error 就会显示「已改名」，用户回头发现名字没变，
// 只会反复点那个按钮。
func TestRenameAndCreateBindingsReportFailure(t *testing.T) {
	a := newSessionTestApp(t, newFakeLLM(t))

	if err := a.RenameSession("h1", "根本不存在", "新名字"); err == nil {
		t.Error("改一条不存在的会话应报错")
	}
	if err := a.RenameSession("h1", agent.DefaultSessionID, "   "); err == nil {
		t.Error("改成空名字应报错")
	}
	if _, err := a.CreateSession("h1", "  "); err == nil {
		t.Error("新建一条空名字的会话应报错")
	}

	// 报错归报错，默认会话的名字不该被顺手改掉。
	for _, s := range a.ListSessions("h1") {
		if s.ID == agent.DefaultSessionID && s.Name != "" {
			t.Errorf("失败的改名不该留下痕迹，默认会话名字成了 %q", s.Name)
		}
	}
}

// fill 在某条会话里跑一轮对话，等它落地。
func fill(t *testing.T, a *App, sessionID, prompt string) {
	t.Helper()
	if err := a.Ask("h1", sessionID, prompt); err != nil {
		t.Fatalf("在 %s 上发起对话失败: %v", sessionID, err)
	}
	waitFor(t, sessionID+" 收到 1 轮", func() bool { return turnsOf(a, "h1", sessionID) == 1 })
}

// eventSink 把 agent 发出的事件收集起来。
//
// 存在的理由是**隔离 Wails 运行时**：生产代码的 a.emit 会调到
// runtime.EventsEmit，而那是靠 Wails 生命周期 context 生效的，
// 在 go test 里必然被判非法并中断整个用例进程。
type eventSink struct {
	mu     sync.Mutex
	events []string
}

func (s *eventSink) emit(ev string, _ any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *eventSink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}
