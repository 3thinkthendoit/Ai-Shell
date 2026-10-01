package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"ai-shell/internal/agent"
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
	// 快照管线的终点是 agent 的瞬态上下文 + EvSnapshot 事件：
	// 不给它真实例，deliverSnapshot 会在 nil 上炸。
	a.ag = agent.New(a.v, a.ssh, a.emit)
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

// 反复打开同一任务（同 host+session）必须复用同一条终端。
//
// 界面在切模式、切回同一任务时会反复调 OpenTerminal。每次都新开一条的话，
// 用户 cd 过去的目录、跑着的进程会被反复丢掉，而他只是切了个标签。
func TestOpenTerminalReusesExistingSession(t *testing.T) {
	a, hostID := newTermTestApp(t)
	const sid = "s1"

	if err := a.OpenTerminal(hostID, sid, 80, 24); err != nil {
		t.Fatalf("第一次打开失败: %v", err)
	}
	first := a.ssh.PTY(termKey(hostID, sid))
	if first == nil {
		t.Fatal("打开之后应当能查到终端")
	}

	if err := a.OpenTerminal(hostID, sid, 100, 30); err != nil {
		t.Fatalf("第二次打开失败: %v", err)
	}
	if a.ssh.PTY(termKey(hostID, sid)) != first {
		t.Error("同任务再次打开应复用同一条终端，而不是新开")
	}
	if n := a.ssh.PTYCount(); n != 1 {
		t.Errorf("应只有 1 条终端，实得 %d", n)
	}
}

// 每任务独立 shell、只留当前任务：切到同主机的另一条任务时，关掉上一条的
// PTY、为目标任务开一条全新 PTY（旧 PTY 结束，注册表只剩当前这一条）。
func TestOpenTerminalPerSessionReopensAndClosesOthers(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if err := a.OpenTerminal(hostID, "A", 80, 24); err != nil {
		t.Fatalf("开任务 A 失败: %v", err)
	}
	firstA := a.ssh.PTY(termKey(hostID, "A"))
	if firstA == nil {
		t.Fatal("任务 A 应有终端")
	}

	if err := a.OpenTerminal(hostID, "B", 80, 24); err != nil {
		t.Fatalf("开任务 B 失败: %v", err)
	}
	// 只留当前任务：A 被关掉、B 新开一条。
	if a.ssh.PTY(termKey(hostID, "A")) != nil {
		t.Error("切到 B 后 A 的终端应已关闭")
	}
	if n := a.ssh.PTYCount(); n != 1 {
		t.Errorf("只留当前任务应只剩 1 条，实得 %d", n)
	}
	b := a.ssh.PTY(termKey(hostID, "B"))
	if b == nil || b == firstA {
		t.Error("B 应是一条全新终端，而不是复用 A")
	}
	select {
	case <-firstA.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("A 的 PTY 关闭后应结束")
	}
}

// 默认会话的空串与 "default" 必须合成同一个注册表键。
//
// 钉住那个静默失败：agent.Run 把空 sessionID 归一成 "default" 后再发出 toolResult
// 的 sessionId，前端据此调 WriteTerminal(h, "default")；而终端可能是在会话列表还没
// 拉回来的窗口里以 raw "" 挂上的。若 termKey 不归一，两边键差一个空段，TTY 交接
// 会查不到 PTY 报「没有打开」，并被前端 catch 静默吞掉 —— 核心功能凭空失效。
func TestTermKeyTreatsBlankAndDefaultAsSameSession(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, "", 80, 24); err != nil {
		t.Fatalf("以空串打开默认会话终端失败: %v", err)
	}
	// 用归一后的 "default" 写入必须命中同一条 PTY（而不是报「没有打开」）。
	if err := a.WriteTerminal(hostID, "default", base64.StdEncoding.EncodeToString([]byte("ls\r"))); err != nil {
		t.Errorf("空串注册的终端应能被归一后的 \"default\" 命中，实得: %v", err)
	}
	// 反向亦然：以 "default" 打开、以空串写入。
	if !a.CloseTerminal(hostID, "") {
		t.Fatalf("关闭默认会话终端应返回 true")
	}
	if err := a.OpenTerminal(hostID, "default", 80, 24); err != nil {
		t.Fatalf("以 \"default\" 打开失败: %v", err)
	}
	if err := a.WriteTerminal(hostID, "", base64.StdEncoding.EncodeToString([]byte("ls\r"))); err != nil {
		t.Errorf("\"default\" 注册的终端应能被空串命中，实得: %v", err)
	}
}

// 打开与关闭都要留审计；重复打开（同任务复用）不该重复留痕。
func TestTerminalOpenCloseIsAuditedOnce(t *testing.T) {
	a, hostID := newTermTestApp(t)
	const sid = "s1"

	if err := a.OpenTerminal(hostID, sid, 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	// 复用不该再记一条：否则用户每切一次标签就多一条「打开了终端」，
	// 真正的那一次反而被淹没。
	if err := a.OpenTerminal(hostID, sid, 80, 24); err != nil {
		t.Fatalf("再次打开失败: %v", err)
	}

	if !a.CloseTerminal(hostID, sid) {
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
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
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
	err := a.OpenTerminal("  ", "s1", 80, 24)
	if err == nil {
		t.Fatal("没选主机时应当报错")
	}
	if !strings.Contains(err.Error(), "请先选择一台主机") {
		t.Errorf("错误措辞应告诉用户该做什么，实得 %q", err.Error())
	}
}

func TestOpenTerminalRequiresVault(t *testing.T) {
	a := &App{} // 未初始化
	if err := a.OpenTerminal("h1", "s1", 80, 24); err == nil {
		t.Fatal("未初始化时应当报错，而不是 panic")
	}
}

// 按键是 base64 传的：解不开就要明确报错，而不是把垃圾字节送到远端。
func TestWriteTerminalRejectsBadBase64(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}

	err := a.WriteTerminal(hostID, "s1", "这不是 base64!!")
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

	err := a.WriteTerminal(hostID, "s1", base64.StdEncoding.EncodeToString([]byte("ls\r")))
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
	if err := a.ResizeTerminal(hostID, "s1", 120, 40); err != nil {
		t.Errorf("终端没打开时调整尺寸应当静默忽略，实得 %v", err)
	}
}

// 关一条没开的终端返回 false，不写审计：记一条「关闭了交互终端」
// 会让人以为真的关掉了什么。
func TestCloseTerminalWithoutOpenTerminalIsFalse(t *testing.T) {
	a, hostID := newTermTestApp(t)

	if a.CloseTerminal(hostID, "s1") {
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
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	p := a.ssh.PTY(termKey(hostID, "s1"))
	if p == nil {
		t.Fatal("打开之后应当能查到终端")
	}

	if !a.CloseTerminal(hostID, "s1") {
		t.Fatal("关闭应当返回 true")
	}
	if a.ssh.PTY(termKey(hostID, "s1")) != nil {
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

	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	first := a.ssh.PTY(termKey(hostID, "s1"))
	a.CloseTerminal(hostID, "s1")

	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	second := a.ssh.PTY(termKey(hostID, "s1"))
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
	got := termDataPayload("h1", "s1", raw)

	if got["hostId"] != "h1" {
		t.Errorf("hostId 应为 h1，实得 %q", got["hostId"])
	}
	if got["sessionId"] != "s1" {
		t.Errorf("sessionId 应为 s1，实得 %q", got["sessionId"])
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
		got := termDataPayload("h", "s", raw)
		back, err := base64.StdEncoding.DecodeString(got["data"])
		if err != nil {
			t.Fatalf("载荷不是合法的 base64: %v", err)
		}
		if string(back) != string(raw) {
			t.Errorf("原始 %v 解回来应一致，实得 %v", raw, back)
		}
	}
}

// ---- 常驻终端的定格快照（单表面控制台）----

// ReportActiveSession 报的会话就是定格快照的归属：切会话/切主机时
// 前端报一次，后端 per-host 记住，PTY 快照用它路由（否则快照会落
// 进一个谁也看不到的默认桶）。
func TestReportActiveSessionRoutesSnapshot(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if got := a.activeSession(hostID); got != "" {
		t.Errorf("未报告时应为空，实得 %q", got)
	}
	if err := a.ReportActiveSession(hostID, "s1"); err != nil {
		t.Fatalf("报告失败: %v", err)
	}
	if got := a.activeSession(hostID); got != "s1" {
		t.Errorf("应记住 s1，实得 %q", got)
	}
	// 切到另一条会话：覆盖而不是累加。
	_ = a.ReportActiveSession(hostID, "s2")
	if got := a.activeSession(hostID); got != "s2" {
		t.Errorf("应覆盖为 s2，实得 %q", got)
	}
}

// 全屏重绘型命令（tuispin：CUP+ED 重绘三帧后退出，不发备用屏幕）在
// OSC 133;D 命令结束时必须定格最后一帧：走完快照管线、审计出现
// 「终端结束画面进入模型上下文」。top 这类不发 ?1049h 的程序全靠
// 「命令期间全屏重绘过」这个特征识别 —— 早先只认备用屏幕会漏掉它们。
func TestPersistentTerminalFinalSnapshotOnTUICommandEnd(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	// 报告快照归属的会话：定格快照落进本 PTY 自己的 sessionID（s1）。
	if err := a.ReportActiveSession(hostID, "s1"); err != nil {
		t.Fatalf("报告会话失败: %v", err)
	}

	if err := a.WriteTerminal(hostID, "s1", base64.StdEncoding.EncodeToString([]byte("tuispin\n"))); err != nil {
		t.Fatalf("写入命令失败: %v", err)
	}

	waitForTerm(t, "TUI 命令结束的定格快照审计落盘", func() bool {
		for _, e := range a.ListAudit(0).Entries {
			if e.Kind == audit.KindTerminal && strings.Contains(e.Note, "终端结束画面进入模型上下文") {
				return true
			}
		}
		return false
	})
}

// PTY 退出（人手敲 exit，shell 结束）是定格的第三个触发点：哪怕这条
// 命令没有全屏重绘，退出时的最后一屏也要留给模型 —— 「死亡现场」
// 往往就是结论本身。
func TestPersistentTerminalFinalSnapshotOnPTYExit(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := a.ReportActiveSession(hostID, "s1"); err != nil {
		t.Fatalf("报告会话失败: %v", err)
	}

	if err := a.WriteTerminal(hostID, "s1", base64.StdEncoding.EncodeToString([]byte("exit\n"))); err != nil {
		t.Fatalf("写入 exit 失败: %v", err)
	}

	waitForTerm(t, "PTY 退出的定格快照审计落盘", func() bool {
		var ended, snapped bool
		for _, e := range a.ListAudit(0).Entries {
			if e.Kind != audit.KindTerminal {
				continue
			}
			if strings.Contains(e.Note, "交互终端已结束") {
				ended = true
			}
			if strings.Contains(e.Note, "终端结束画面进入模型上下文") {
				snapped = true
			}
		}
		return ended && snapped
	})
}

// 流式输出命令（ls 这类，命令期间没有全屏重绘）结束时**不该**定格：
// 它的结尾画面没有信息量，进模型上下文纯属噪音。只有全屏重绘过的
// 命令才值得留最后一帧。
func TestPersistentTerminalNoSnapshotForPlainCommand(t *testing.T) {
	a, hostID := newTermTestApp(t)
	if err := a.OpenTerminal(hostID, "s1", 80, 24); err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if err := a.ReportActiveSession(hostID, "s1"); err != nil {
		t.Fatalf("报告会话失败: %v", err)
	}
	if err := a.WriteTerminal(hostID, "s1", base64.StdEncoding.EncodeToString([]byte("ls\n"))); err != nil {
		t.Fatalf("写入命令失败: %v", err)
	}

	// 等 ls 的输出确实到了（命令已执行完），再确认没有定格快照。
	waitForTerm(t, "ls 输出到达", func() bool {
		for _, e := range a.ListAudit(0).Entries {
			_ = e
		}
		// ls 本身不写审计，这里靠「等一会」让 133;D 处理完：
		// 用 PTY 仍活着 + 给足时间的方式间接等待。
		return a.ssh.PTY(termKey(hostID, "s1")) != nil
	})
	time.Sleep(200 * time.Millisecond)
	for _, e := range a.ListAudit(0).Entries {
		if e.Kind == audit.KindTerminal && strings.Contains(e.Note, "终端结束画面进入模型上下文") {
			t.Errorf("流式命令结束不该定格快照，实得 %q", e.Note)
		}
	}
}
