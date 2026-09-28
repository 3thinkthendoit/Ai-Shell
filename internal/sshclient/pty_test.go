package sshclient

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-shell/internal/sshtest"
)

// 本文件是交互终端（PTY）的端到端测试。
//
// 它跑的是**真实的**链路：真 TCP + SSH 握手 + 申请伪终端 + shell 通道，
// 而不是断言「函数被调用了」。终端这块的坑几乎全在「参数顺序」和「异步时序」上，
// 这两类问题都只有真跑一遍才能发现：
//   - RequestPty(term, 高, 宽) 与报文里的「宽、高」顺序相反；
//   - 启动时的提示符、按键的回显都是异步分块到达的。

// ptySink 收集终端的输出与退出回调。
//
// 必须加锁：onData / onExit 是在 ssh 库自己的 goroutine 上被调用的，
// 而断言发生在测试 goroutine 上。
type ptySink struct {
	mu    sync.Mutex
	data  []byte
	exits []string
}

func (s *ptySink) write(b []byte) {
	s.mu.Lock()
	s.data = append(s.data, b...)
	s.mu.Unlock()
}

func (s *ptySink) exit(reason string) {
	s.mu.Lock()
	s.exits = append(s.exits, reason)
	s.mu.Unlock()
}

func (s *ptySink) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.data)
}

func (s *ptySink) exitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.exits)
}

func (s *ptySink) exitReasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.exits...)
}

// waitFor 轮询直到条件成立。
//
// 不用固定 sleep：终端输出是异步到达的，睡短了会偶发失败、睡长了拖慢整个套件，
// 而这两者都会让人开始不信任测试本身。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// openTestPTY 起一个测试服务器并在 h1 上开一条终端。
func openTestPTY(t *testing.T, term string, size TerminalSize) (*Client, *sshtest.Server, *PTY, *ptySink) {
	t.Helper()
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h1", srv.Addr)
	c := New(v)
	t.Cleanup(c.Close)

	sink := &ptySink{}
	p, err := c.OpenPTY("h1", term, size, sink.write, sink.exit)
	if err != nil {
		t.Fatalf("打开交互终端失败: %v", err)
	}
	return c, srv, p, sink
}

// 尺寸与 TERM 必须原样到达远端。
//
// 这是整条链路里最容易写反的地方：客户端侧是 RequestPty(term, 高, 宽)，
// 而 SSH 报文里是「宽、高」。任何一处写反都不会报错，只会得到一个尺寸颠倒的
// 终端，表现为「排版有点怪」。所以列和行用两个不同的数字，写反必然暴露。
func TestOpenPTYSendsRequestedTerminalSize(t *testing.T) {
	_, srv, _, _ := openTestPTY(t, "screen-256color", TerminalSize{Cols: 132, Rows: 43})

	waitFor(t, "服务器收到 pty-req", func() bool {
		_, ok := srv.LastPTY()
		return ok
	})

	got, _ := srv.LastPTY()
	if got.Term != "screen-256color" {
		t.Errorf("TERM 应为 screen-256color，实得 %q", got.Term)
	}
	if got.Cols != 132 {
		t.Errorf("列数应为 132，实得 %d（行列写反了？）", got.Cols)
	}
	if got.Rows != 43 {
		t.Errorf("行数应为 43，实得 %d（行列写反了？）", got.Rows)
	}
}

// 终端没指定 TERM 时要兜一个默认值，不能发空串。
// 发空串的话远端程序拿不到 TERM，ncurses 那类程序会直接罢工。
func TestOpenPTYDefaultsTerminalType(t *testing.T) {
	_, srv, _, _ := openTestPTY(t, "", TerminalSize{Cols: 80, Rows: 24})

	waitFor(t, "服务器收到 pty-req", func() bool {
		_, ok := srv.LastPTY()
		return ok
	})
	got, _ := srv.LastPTY()
	if got.Term == "" {
		t.Error("未指定 TERM 时应兜一个默认值，不能发空串")
	}
}

// 按键要到得了远端，远端的输出要**边产生边到**（而不是攒到结束）。
func TestPTYForwardsKeystrokesAndStreamsOutput(t *testing.T) {
	_, _, p, sink := openTestPTY(t, "", TerminalSize{Cols: 80, Rows: 24})

	// 启动时的提示符是最容易被前端漏掉的一段输出：
	// 界面若「先调 OpenTerminal 再建渲染对象」，它就落进虚空了。
	waitFor(t, "收到启动提示符", func() bool {
		return strings.Contains(sink.output(), "sh$ ")
	})

	if err := p.Write([]byte("echo hi\r")); err != nil {
		t.Fatalf("写入按键失败: %v", err)
	}
	waitFor(t, "收到命令执行结果", func() bool {
		return strings.Contains(sink.output(), "got:echo hi")
	})
}

// 尺寸变化要真的传到远端，否则 top 之类会按旧尺寸排版。
func TestPTYResizeReachesRemote(t *testing.T) {
	_, srv, p, _ := openTestPTY(t, "", TerminalSize{Cols: 80, Rows: 24})

	if err := p.Resize(TerminalSize{Cols: 120, Rows: 40}); err != nil {
		t.Fatalf("调整尺寸失败: %v", err)
	}

	waitFor(t, "服务器收到 window-change", func() bool {
		return len(srv.Resizes()) > 0
	})
	all := srv.Resizes()
	got := all[len(all)-1]
	if got.Cols != 120 || got.Rows != 40 {
		t.Errorf("远端应收到 120x40，实得 %dx%d（行列写反了？）", got.Cols, got.Rows)
	}
}

// 远端 shell 退出时，onExit 恰好触发一次，且原因说得清。
func TestPTYExitFiresExactlyOnce(t *testing.T) {
	_, _, p, sink := openTestPTY(t, "", TerminalSize{})
	waitFor(t, "收到启动提示符", func() bool {
		return strings.Contains(sink.output(), "sh$ ")
	})

	if err := p.Write([]byte("exit\r")); err != nil {
		t.Fatalf("写入 exit 失败: %v", err)
	}
	waitFor(t, "onExit 触发", func() bool { return sink.exitCount() > 0 })

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("终端结束后 Done 应当关闭")
	}

	// 留一点时间给「第二次触发」：Wait() 返回和通道关闭都会走到 finish，
	// 幂等一旦破了，这里会看到两条。
	time.Sleep(150 * time.Millisecond)
	if n := sink.exitCount(); n != 1 {
		t.Errorf("onExit 应恰好触发一次，实得 %d 次：%v", n, sink.exitReasons())
	}
	if r := sink.exitReasons()[0]; !strings.Contains(r, "退出") {
		t.Errorf("退出原因应说明是远端 shell 退出，实得 %q", r)
	}
}

// 关掉之后再写入/改尺寸必须被明确拒绝，而不是 panic 或静默无效果。
func TestPTYWriteAndResizeAfterCloseAreRejected(t *testing.T) {
	_, _, p, _ := openTestPTY(t, "", TerminalSize{})

	if err := p.Close(); err != nil {
		t.Fatalf("关闭终端失败: %v", err)
	}
	// 重复关闭是正常的：界面可能有两条路径都会关一次，不该因此报错。
	if err := p.Close(); err != nil {
		t.Errorf("重复关闭不该报错，实得 %v", err)
	}

	if err := p.Write([]byte("x")); !errors.Is(err, ErrPTYClosed) {
		t.Errorf("关闭后写入应返回 ErrPTYClosed，实得 %v", err)
	}
	if err := p.Resize(TerminalSize{Cols: 100, Rows: 30}); !errors.Is(err, ErrPTYClosed) {
		t.Errorf("关闭后调整尺寸应返回 ErrPTYClosed，实得 %v", err)
	}
}

// 已经开着活终端时要复用，不能另开一条。
//
// 新开一条会把用户已经 cd 过去的目录、正在跑的进程全部丢掉，
// 而界面那边以为自己只是「又打开了一次」。
func TestOpenPTYReusesLiveTerminal(t *testing.T) {
	c, _, p, sink := openTestPTY(t, "", TerminalSize{})
	waitFor(t, "收到启动提示符", func() bool {
		return strings.Contains(sink.output(), "sh$ ")
	})

	again, err := c.OpenPTY("h1", "", TerminalSize{}, sink.write, sink.exit)
	if err != nil {
		t.Fatalf("再次打开应成功: %v", err)
	}
	if again != p {
		t.Error("已有活终端时应复用同一条，而不是新开一条")
	}
	if n := c.PTYCount(); n != 1 {
		t.Errorf("应只有 1 条终端，实得 %d", n)
	}
}

// 这是本文件最重要的一条：**无关命令失败不能连坐终端**。
//
// Exec 在超时路径上会调 Disconnect(hostID) 关掉连接池里那条连接。
// 终端如果和它共用连接，就会跟着一起死 —— 而用户看到的只是
// 「另一个标签里的命令超时了，我的终端没了」，完全看不出因果。
func TestExecFailureDoesNotKillOpenTerminal(t *testing.T) {
	c, _, p, sink := openTestPTY(t, "", TerminalSize{})
	waitFor(t, "收到启动提示符", func() bool {
		return strings.Contains(sink.output(), "sh$ ")
	})

	// 同一主机上跑一条必然超时的命令（1 秒超时 + sleep 30）。
	if _, err := c.Exec("h1", "sleep 30", 1*time.Second); err == nil {
		t.Fatal("这条命令应当超时")
	}

	if !p.alive() {
		t.Fatal("无关命令超时不该把交互终端一起掐掉")
	}
	if err := p.Write([]byte("echo alive\r")); err != nil {
		t.Fatalf("超时之后终端应仍可写入: %v", err)
	}
	waitFor(t, "终端在无关命令超时后仍能收到回显", func() bool {
		return strings.Contains(sink.output(), "got:echo alive")
	})
}

// 关掉终端后注册表要清干净，否则会一路攒到「已达上限」。
func TestClosePTYRemovesFromRegistry(t *testing.T) {
	c, _, _, _ := openTestPTY(t, "", TerminalSize{})

	if !c.ClosePTY("h1") {
		t.Error("关闭一条已开的终端应返回 true")
	}
	if c.ClosePTY("h1") {
		t.Error("再关一次应返回 false")
	}
	if n := c.PTYCount(); n != 0 {
		t.Errorf("关掉之后不该还剩终端，实得 %d", n)
	}
}

// 每条终端占一条独立连接，所以必须有数量上限。
func TestOpenPTYEnforcesTotalCap(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	for i := 0; i <= MaxPTYTotal; i++ {
		addPasswordHost(t, v, fmt.Sprintf("cap-%d", i), srv.Addr)
	}
	c := New(v)
	t.Cleanup(c.Close)

	for i := 0; i < MaxPTYTotal; i++ {
		if _, err := c.OpenPTY(fmt.Sprintf("cap-%d", i), "", TerminalSize{}, nil, nil); err != nil {
			t.Fatalf("第 %d 条终端应能打开: %v", i+1, err)
		}
	}

	_, err := c.OpenPTY(fmt.Sprintf("cap-%d", MaxPTYTotal), "", TerminalSize{}, nil, nil)
	if err == nil {
		t.Fatal("超过上限应报错，而不是无限开连接")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应说明是数量上限，实得 %q", err.Error())
	}
}

func TestOpenPTYUnknownHostFails(t *testing.T) {
	v := newVault(t)
	c := New(v)
	t.Cleanup(c.Close)

	if _, err := c.OpenPTY("nope", "", TerminalSize{}, nil, nil); err == nil {
		t.Fatal("主机不存在时应当报错")
	}
}

// 非法尺寸要退化成能用的默认值：前端的 FitAddon 在容器还没布局完时
// 可能算出 0，那不是错误，只是「还不知道」。传 0 给远端会让 top 按 0 列排版。
func TestTerminalSizeNormalized(t *testing.T) {
	cases := []struct{ in, want TerminalSize }{
		{TerminalSize{}, TerminalSize{Cols: 80, Rows: 24}},
		{TerminalSize{Cols: 0, Rows: 40}, TerminalSize{Cols: 80, Rows: 40}},
		{TerminalSize{Cols: 100, Rows: -1}, TerminalSize{Cols: 100, Rows: 24}},
		{TerminalSize{Cols: 120, Rows: 30}, TerminalSize{Cols: 120, Rows: 30}},
	}
	for _, c := range cases {
		if got := c.in.normalized(); got != c.want {
			t.Errorf("%+v 归一化后应为 %+v，实得 %+v", c.in, c.want, got)
		}
	}
}

// onData 收到的字节块必须是**独立的副本**。
//
// ssh 库复用它传给 Write 的那块缓冲区，而 onData 会把数据交给另一个
// goroutine 去编码、发事件。不复制的话，用户会看到后面一段输出
// 把前面一段覆盖掉 —— 而且只在输出量大、切块多的时候才出现。
func TestPTYDataChunksAreIndependentCopies(t *testing.T) {
	_, _, p, sink := openTestPTY(t, "", TerminalSize{Cols: 200, Rows: 50})
	waitFor(t, "收到启动提示符", func() bool {
		return strings.Contains(sink.output(), "sh$ ")
	})

	// 敲一长串，制造多次读、多块输出。
	long := strings.Repeat("abcdefghij", 40)
	if err := p.Write([]byte("echo " + long + "\r")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	waitFor(t, "收到完整回显", func() bool {
		return strings.Contains(sink.output(), "got:echo "+long)
	})

	// 再确认一次内容没有被后续数据污染。
	if !bytes.Contains([]byte(sink.output()), []byte(long)) {
		t.Error("回显内容不完整，可能是数据块被复用后覆盖了")
	}
}
