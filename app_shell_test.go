package main

import (
	"strings"
	"testing"

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
	// 人敲 shell：非硬拒绝命令自动放行，不依赖白名单/手动模式
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

func TestRunShell_HighRiskAutoExecutes(t *testing.T) {
	// 人工路径不再对高危命令挂审批：mkdir -p 直接执行完成（status=done）。
	a, hostID := newShellTestApp(t, vault.ModeWhitelist, vault.DefaultWhitelist())
	res := a.RunShell(hostID, "mkdir -p /tmp/aishell-hr-auto", "~", 10)
	if res.Status != "done" {
		t.Fatalf("status=%s error=%s decision=%s", res.Status, res.Error, res.Decision)
	}
	if res.Rule != "auto_safe" {
		t.Fatalf("rule=%s want auto_safe（人工路径不应再命中 high_risk）", res.Rule)
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
