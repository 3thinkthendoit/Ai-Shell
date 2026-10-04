package main

import (
	"strings"
	"testing"
	"time"

	"ai-shell/internal/audit"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/sshtest"
	"ai-shell/internal/vault"
)

func newShellTestApp(t *testing.T, mode vault.PolicyMode, whitelist []string) (*App, string) {
	t.Helper()
	a, hostID, _ := newShellTestAppWithServer(t, mode, whitelist)
	return a, hostID
}

// newShellTestAppWithServer 与 newShellTestApp 相同，但把假远端服务器也交出来。
//
// 有些用例需要**改造远端的行为**才能复现真实环境的问题 —— 最典型的是
// 「远端 stdout 不干净」（登录脚本打欢迎语、busybox 的 base64 打用法提示），
// 而这个条件在默认的假远端里不存在，于是那类 bug 永远测不到。
func newShellTestAppWithServer(t *testing.T, mode vault.PolicyMode, whitelist []string) (*App, string, *sshtest.Server) {
	t.Helper()
	srv := sshtest.Start(t)

	a := newTestApp(t)
	a.ssh = sshclient.New(a.v)
	a.ctx = nil

	if err := a.v.SaveHost(vault.Host{
		ID: "h1", Name: "h1", Addr: srv.Addr,
		User: sshtest.User, AuthMethod: vault.AuthPassword,
	}, &vault.HostSecret{Password: sshtest.Password}); err != nil {
		t.Fatal(err)
	}
	pol := a.v.Policy()
	pol.Mode = mode
	if whitelist != nil {
		pol.Whitelist = whitelist
	}
	if err := a.v.SetPolicy(pol); err != nil {
		t.Fatal(err)
	}

	lg, err := audit.New(a.v.Dir(), a.v)
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	a.audit = lg
	return a, "h1", srv
}

func TestRunShell_SafeAutoAllows(t *testing.T) {
	// 人敲 shell：非高危自动放行，不依赖白名单/手动模式
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)
	res := a.RunShell(hostID, "echo hi", "~", 10)
	if res.Status != "done" {
		t.Fatalf("status=%s error=%s decision=%s", res.Status, res.Error, res.Decision)
	}
	if res.Decision != "allow" {
		t.Fatalf("decision=%s want allow", res.Decision)
	}
	if !strings.Contains(res.Stdout, "hi") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}

func TestRunShell_DenyProtected(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeWhitelist, []string{"cat"})
	res := a.RunShell(hostID, "cat /etc/shadow", "~", 10)
	if res.Status != "denied" {
		t.Fatalf("期望 denied，得 %s (%s)", res.Status, res.Error)
	}
	if res.Decision != "deny" {
		t.Fatalf("decision=%s", res.Decision)
	}
}

func TestRunShell_HighRiskConfirmThenApprove(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeWhitelist, vault.DefaultWhitelist())

	done := make(chan ShellResult, 1)
	go func() {
		done <- a.RunShell(hostID, "mkdir -p /tmp/aishell-hr-test", "~", 10)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		a.shell.mu.Lock()
		for k := range a.shell.approvals {
			id = k
		}
		a.shell.mu.Unlock()
		if id != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("高危命令应挂起审批")
	}
	if !a.Approve(id, true) {
		t.Fatal("Approve 应成功")
	}

	res := <-done
	if res.Status != "done" {
		t.Fatalf("status=%s error=%s", res.Status, res.Error)
	}
	if res.Rule != "high_risk" {
		t.Fatalf("rule=%s want high_risk", res.Rule)
	}
}

func TestRunShell_HighRiskConfirmThenReject(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	done := make(chan ShellResult, 1)
	go func() {
		done <- a.RunShell(hostID, "rm -f /tmp/aishell-nope", "~", 10)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		a.shell.mu.Lock()
		for k := range a.shell.approvals {
			id = k
		}
		a.shell.mu.Unlock()
		if id != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("审批没有挂起")
	}
	a.Approve(id, false)

	res := <-done
	if res.Status != "denied" {
		t.Fatalf("期望 denied，得 %s", res.Status)
	}
}

func TestRunShell_WrapUsesCwd(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeWhitelist, []string{"echo"})
	// 包装后应为 cd '/tmp' && echo WRAPOK；假远端剥前缀后回显 WRAPOK。
	res := a.RunShell(hostID, "echo WRAPOK", "/tmp", 10)
	if res.Status != "done" {
		t.Fatalf("status=%s error=%s stderr=%s", res.Status, res.Error, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "WRAPOK") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	wrapped := wrapShellCommand("/tmp", "echo WRAPOK")
	if !strings.HasPrefix(wrapped, "cd '/tmp' && ") {
		t.Fatalf("wrap=%q", wrapped)
	}
}

func TestRunShell_AskBlockedWhileShellPending(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	done := make(chan ShellResult, 1)
	go func() {
		done <- a.RunShell(hostID, "touch /tmp/aishell-busy", "~", 10)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.lane.shellBusy() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !a.lane.shellBusy() {
		t.Fatal("shell 应占用中")
	}
	if id := a.lane.shellHostID(); id != hostID {
		t.Fatalf("shellHostID=%q want %q", id, hostID)
	}
	err := a.Ask(hostID, "", "hello")
	if err == nil {
		t.Fatal("Ask 应被 shell 占用拒绝")
	}

	a.cancelShellApprovals()
	<-done
}

func TestRunShell_EmptyHost(t *testing.T) {
	a, _ := newShellTestApp(t, vault.ModeWhitelist, []string{"echo"})
	res := a.RunShell("", "echo x", "~", 5)
	if res.Status != "error" {
		t.Fatalf("status=%s", res.Status)
	}
}

func TestRunShell_MissingThirdParty(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeWhitelist, vault.DefaultWhitelist())
	res := a.RunShell(hostID, "nosuch-aishell-bin-xyz --help", "~", 10)
	if res.Status != "missing" {
		t.Fatalf("期望 missing，得 %s error=%s", res.Status, res.Error)
	}
	if res.Hint == "" {
		t.Fatal("应提示改走 LLM")
	}
}

func TestWrapShellCommand(t *testing.T) {
	got := wrapShellCommand("~", "uname -a")
	if got != "cd ~ && uname -a" {
		t.Fatalf("got %q", got)
	}
	got = wrapShellCommand("/etc", "ls")
	if got != "cd '/etc' && ls" {
		t.Fatalf("got %q", got)
	}
	got = wrapShellCommand("~", "cd /var")
	if !strings.Contains(got, cwdMarker) || !strings.Contains(got, "cd '/var'") {
		t.Fatalf("bare cd wrap=%q", got)
	}
}
