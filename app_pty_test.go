package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"ai-shell/internal/audit"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/sshtest"
	"ai-shell/internal/vault"
)

// 交互终端在 App 这一层的测试。
//
// 底层（真开 PTY、按键到达、尺寸传递、连接隔离）已经在 internal/sshclient
// 里端到端测过了，这里只测**绑定层自己的责任**：参数校验、错误措辞、
// 幂等复用，以及审计记没记对。
//
// 事件推送不在这里断言 —— 它需要 Wails 的运行期上下文，在测试里跑不起来。
// 载荷的编码单独抽成了 termDataPayload，由下面的纯函数用例覆盖。

func newTermTestApp(t *testing.T) (*App, string) {
	t.Helper()
	srv := sshtest.Start(t)

	a := newTestApp(t)
	a.ssh = sshclient.New(a.v)
	a.ctx = nil // 事件推送在测试里是空操作（见 a.emit），不影响绑定逻辑

	if err := a.v.SaveHost(vault.Host{
		ID: "h1", Name: "h1", Addr: srv.Addr,
		User: sshtest.User, AuthMethod: vault.AuthPassword,
	}, &vault.HostSecret{Password: sshtest.Password}); err != nil {
		t.Fatal(err)
	}

	// 审计要真的能写：终端的开/关是必须留痕的动作，
	// 用 nil 糊过去等于这块根本没测。
	lg, err := audit.New(a.v.Dir(), a.v)
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	a.audit = lg
	return a, "h1"
}

// waitForTerm 轮询直到条件成立（终端相关状态都是异步落地的）。
func waitForTerm(t *testing.T, what string, cond func() bool) {
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

// 反复打开同一台主机必须复用同一条终端。
//
// 界面在切模式、切主机时会反复调 OpenTerminal。每次都新开一条的话，
// 用户 cd 过去的目录、跑着的进程会被反复丢掉，而他只是切了个标签。
func TestOpenTerminalReusesExistingSession(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("第一次打开失败: %v", err)
	}
	first := a.ssh.PTY(hostID)
	if first == nil {
		t.Fatal("打开之后应当能查到终端")
	}

	if err := a.OpenTerminal(hostID, 100, 30); err != nil {
		t.Fatalf("第二次打开失败: %v", err)
	}
	if a.ssh.PTY(hostID) != first {
		t.Error("再次打开应复用同一条终端，而不是新开")
	}
	if n := a.ssh.PTYCount(); n != 1 {
		t.Errorf("应只有 1 条终端，实得 %d", n)
	}
}

// 打开与关闭都要留审计；重复打开（复用）不该重复留痕。
func TestTerminalOpenCloseIsAuditedOnce(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	// 复用不该再记一条：否则用户每切一次标签就多一条「打开了终端」，
	// 真正的那一次反而被淹没。
	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("再次打开失败: %v", err)
	}

	if !a.CloseTerminal(hostID) {
		t.Fatal("关闭一条已开的终端应返回 true")
	}

	view := a.ListAudit(0)
	if view.Error != "" {
		t.Fatalf("读取审计失败: %s", view.Error)
	}

	var opened, closed int
	for _, e := range view.Entries {
		if e.Kind != audit.KindTerminal {
			continue
		}
		switch {
		case strings.Contains(e.Note, "打开了交互终端"):
			opened++
		case strings.Contains(e.Note, "关闭了交互终端"):
			closed++
		}
	}
	if opened != 1 {
		t.Errorf("「打开交互终端」应恰好记 1 条，实得 %d 条", opened)
	}
	if closed != 1 {
		t.Errorf("「关闭交互终端」应恰好记 1 条，实得 %d 条", closed)
	}
}

// 审计里必须写明它绕过了策略引擎 —— 这是整个功能里最需要被追溯的一件事。
func TestTerminalAuditSaysItBypassesPolicy(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}

	view := a.ListAudit(0)
	var note string
	for _, e := range view.Entries {
		if e.Kind == audit.KindTerminal && strings.Contains(e.Note, "打开了交互终端") {
			note = e.Note
		}
	}
	if note == "" {
		t.Fatal("应当有一条「打开了交互终端」的审计")
	}
	if !strings.Contains(note, "策略引擎") {
		t.Errorf("审计必须写明不经过策略引擎，实得 %q", note)
	}
	if !strings.Contains(note, "登录用户") {
		t.Errorf("审计应说明命令以谁的身份执行，实得 %q", note)
	}
}

func TestOpenTerminalRejectsBlankHost(t *testing.T) {
	a, _ := newTermTestApp(t)
	err := a.OpenTerminal("  ", 80, 24)
	if err == nil {
		t.Fatal("没选主机时应当报错")
	}
	if !strings.Contains(err.Error(), "请先选择一台主机") {
		t.Errorf("错误措辞应告诉用户该做什么，实得 %q", err.Error())
	}
}

func TestOpenTerminalRequiresVault(t *testing.T) {
	a := &App{} // 未初始化
	if err := a.OpenTerminal("h1", 80, 24); err == nil {
		t.Fatal("未初始化时应当报错，而不是 panic")
	}
}

// 按键是 base64 传的：解不开就要明确报错，而不是把垃圾字节送到远端。
func TestWriteTerminalRejectsBadBase64(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}

	err := a.WriteTerminal(hostID, "这不是 base64!!")
	if err == nil {
		t.Fatal("非法 base64 应当报错")
	}
	if !strings.Contains(err.Error(), "解码失败") {
		t.Errorf("错误信息应指出是解码问题，实得 %q", err.Error())
	}
}

// 终端没打开就写按键：必须明确拒绝，而不是静默吞掉 ——
// 静默吞掉的话用户会以为「终端开着但不响应」，那是另一类排查噩梦。
func TestWriteTerminalWithoutOpenTerminalFails(t *testing.T) {
	a, hostID := newTermTestApp(t)

	err := a.WriteTerminal(hostID, base64.StdEncoding.EncodeToString([]byte("ls\r")))
	if err == nil {
		t.Fatal("终端没打开时写入应当报错")
	}
	if !strings.Contains(err.Error(), "没有打开") {
		t.Errorf("错误信息应说明终端未打开，实得 %q", err.Error())
	}
}

// 尺寸上报在终端没开时静默忽略：布局变化随时会发生，
// 此时弹一条报错纯属噪音。
func TestResizeTerminalWithoutOpenTerminalIsNoop(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.ResizeTerminal(hostID, 120, 40); err != nil {
		t.Errorf("终端没打开时调整尺寸应当静默忽略，实得 %v", err)
	}
}

// 关一条没开的终端返回 false，不写审计：记一条「关闭了交互终端」
// 会让人以为真的关掉了什么。
func TestCloseTerminalWithoutOpenTerminalIsFalse(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if a.CloseTerminal(hostID) {
		t.Error("没开着的时候关闭应返回 false")
	}
	view := a.ListAudit(0)
	for _, e := range view.Entries {
		if e.Kind == audit.KindTerminal && strings.Contains(e.Note, "关闭了") {
			t.Errorf("没关掉任何东西时不该写审计，实得 %q", e.Note)
		}
	}
}

// 关闭之后确实关掉了（远端 shell 也结束），而不是只从注册表里摘掉。
func TestCloseTerminalActuallyEndsSession(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	p := a.ssh.PTY(hostID)
	if p == nil {
		t.Fatal("打开之后应当能查到终端")
	}

	if !a.CloseTerminal(hostID) {
		t.Fatal("关闭应当返回 true")
	}
	if a.ssh.PTY(hostID) != nil {
		t.Error("关闭之后注册表里不该还有这条终端")
	}

	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("关闭之后终端应当结束（远端 shell 收到挂断）")
	}
	if p.Resize(sshclient.TerminalSize{Cols: 80, Rows: 24}) == nil {
		t.Error("关闭之后调整尺寸应当报错")
	}
}

// 关掉之后可以重新打开一条新的（而不是被旧条目挡住）。
func TestReopenAfterCloseGivesFreshTerminal(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	first := a.ssh.PTY(hostID)
	a.CloseTerminal(hostID)

	if err := a.OpenTerminal(hostID, 80, 24); err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	second := a.ssh.PTY(hostID)
	if second == nil {
		t.Fatal("重新打开后应当有终端")
	}
	if second == first {
		t.Error("关掉之后重新打开应当是一条新终端")
	}
	waitForTerm(t, "新终端可用", func() bool { return second.HostID() == hostID })
}

// ---- 事件载荷编码（纯函数）----

// 载荷必须是标准 base64，且解回来与原始字节**逐字节相等**。
func TestTermDataPayloadRoundTrips(t *testing.T) {
	// 混入多字节字符和一个被切开的多字节序列：这正是当初选 base64 的理由。
	raw := []byte("中文输出 \xe4\xb8") // 最后两个字节是一个不完整的三字节字符
	got := termDataPayload("h1", raw)

	if got["hostId"] != "h1" {
		t.Errorf("hostId 应为 h1，实得 %q", got["hostId"])
	}
	back, err := base64.StdEncoding.DecodeString(got["data"])
	if err != nil {
		t.Fatalf("载荷不是合法的 base64: %v", err)
	}
	if string(back) != string(raw) {
		t.Errorf("解回来应与原始字节一致，实得 %q", back)
	}
}

func TestTermDataPayloadHandlesEmptyAndBinary(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {0x00, 0xff, 0x1b, 0x5b, 0x32, 0x4a}} {
		got := termDataPayload("h", raw)
		back, err := base64.StdEncoding.DecodeString(got["data"])
		if err != nil {
			t.Fatalf("载荷不是合法的 base64: %v", err)
		}
		if string(back) != string(raw) {
			t.Errorf("原始 %v 解回来应一致，实得 %v", raw, back)
		}
	}
}
