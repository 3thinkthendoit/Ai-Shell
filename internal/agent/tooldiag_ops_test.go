package agent

import (
	"strings"
	"testing"

	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

func TestDiagnosticsScriptRejectsUnknownScope(t *testing.T) {
	if _, err := diagnosticsScript("weird", "", "1h"); err == nil {
		t.Fatal("unknown scope should error")
	}
}

func TestDiagnosticsScriptDefaultContainsJournal(t *testing.T) {
	s, err := diagnosticsScript("default", "", "1h")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== os ==", "== journal_err ==", "journalctl"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q:\n%s", want, s)
		}
	}
}

func TestSanitizeUnitRejectsMetacharacters(t *testing.T) {
	if got := sanitizeUnit("nginx;rm -rf /"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := sanitizeUnit("nginx.service"); got != "nginx.service" {
		t.Fatalf("got %q", got)
	}
}

func TestParseVerifyChecks(t *testing.T) {
	checks, err := parseVerifyChecks(`[{"type":"service","unit":"nginx"},{"type":"port","port":80},{"type":"file_contains","path":"/etc/nginx/nginx.conf","text":"worker"}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 3 {
		t.Fatalf("len=%d", len(checks))
	}
}

func TestParseVerifyChecksRejectsRemoteHTTP(t *testing.T) {
	_, err := parseVerifyChecks(`[{"type":"http","url":"http://evil.example/x"}]`)
	if err == nil {
		t.Fatal("remote http should be rejected")
	}
}

func TestIsLocalHTTPURLRejectsPrefixBypass(t *testing.T) {
	bad := []string{
		"http://127.0.0.1.attacker/",
		"http://localhost.evil/",
		"http://127.0.0.1@evil/",
		"https://127.0.0.1.evil.com/x",
		"http://evil.com/",
		"ftp://127.0.0.1/",
	}
	for _, u := range bad {
		if isLocalHTTPURL(u) {
			t.Fatalf("should reject %q", u)
		}
	}
	good := []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/health",
		"https://localhost/status",
		"http://[::1]/",
	}
	for _, u := range good {
		if !isLocalHTTPURL(u) {
			t.Fatalf("should allow %q", u)
		}
	}
}

func TestParseVerifyChecksRejectsCredentialPath(t *testing.T) {
	// 策略层也会拦；这里保证路径形状至少是绝对路径。
	_, err := parseVerifyChecks(`[{"type":"file_exists","path":"../etc/shadow"}]`)
	if err == nil {
		t.Fatal("relative path should be rejected")
	}
}

func TestResolveSnapshotPath(t *testing.T) {
	p, err := resolveSnapshotPath("12345")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "/12345.snap") {
		t.Fatalf("got %s", p)
	}
	if _, err := resolveSnapshotPath("/tmp/aishell-snapshots/../etc/shadow"); err == nil {
		t.Fatal("path with .. should fail")
	}
	if _, err := resolveSnapshotPath("/etc/shadow"); err == nil {
		t.Fatal("arbitrary absolute path outside snapshotDir must fail")
	}
	if _, err := resolveSnapshotPath("/tmp/aishell-snapshots/ok.snap"); err != nil {
		t.Fatalf("in-dir snap should work: %v", err)
	}
	if _, err := resolveSnapshotPath("/tmp/aishell-snapshots/ok.txt"); err == nil {
		t.Fatal("non-.snap must fail")
	}
}

func TestSanitizePathsRejectsMetacharacters(t *testing.T) {
	bad := []string{
		`/tmp/x"; reboot; echo "`,
		"/tmp/x;rm",
		"/tmp/$HOME",
		"/tmp/x`id`",
		"/tmp/x with space",
	}
	for _, p := range bad {
		if _, err := sanitizePaths(p, 5); err == nil {
			t.Fatalf("should reject %q", p)
		}
	}
	ok, err := sanitizePaths("/etc/nginx/nginx.conf,/var/log/app.log", 5)
	if err != nil || len(ok) != 2 {
		t.Fatalf("got %v err=%v", ok, err)
	}
}

func TestOptionalUnitRejectsInvalid(t *testing.T) {
	if _, err := optionalUnit("nginx;rm -rf /"); err == nil {
		t.Fatal("expected error")
	}
	u, err := optionalUnit("nginx.service")
	if err != nil || u != "nginx.service" {
		t.Fatalf("got %q %v", u, err)
	}
	u, err = optionalUnit("")
	if err != nil || u != "" {
		t.Fatalf("empty should be ok: %q %v", u, err)
	}
}

func TestEvalDiffStateDeniesOutsideSnapshotDir(t *testing.T) {
	tool, ok := lookupTool("diff_state")
	if !ok {
		t.Fatal("missing tool")
	}
	view := &ToolCallView{
		Name:    "diff_state",
		Args:    `{"before":"/etc/shadow"}`,
		Command: "/etc/shadow",
	}
	v := tool.Evaluate(view, vault.PolicySettings{Mode: vault.ModeWhitelist})
	if v.Decision != policy.Deny {
		t.Fatalf("want deny for /etc/shadow, got %s (%s)", v.Decision, v.Reason)
	}
}

func TestProtectedProbeCoversDiffState(t *testing.T) {
	got := protectedProbe("diff_state", "/etc/shadow")
	if got != "cat /etc/shadow" {
		t.Fatalf("got %q", got)
	}
	v := policy.EvaluateProtected(got)
	if v.Decision != policy.Deny {
		t.Fatalf("want deny, got %s", v.Decision)
	}
}

func TestSnapshotCaptureQuotesPaths(t *testing.T) {
	script := snapshotCaptureScript("1", []string{"os"}, "", []string{"/etc/nginx/nginx.conf"})
	if !strings.Contains(script, "path='/etc/nginx/nginx.conf'") {
		t.Fatalf("path label must be shell-quoted:\n%s", script)
	}
	if strings.Contains(script, `path=/etc/nginx/nginx.conf";`) {
		t.Fatal("unquoted path injection surface")
	}
}

func TestVerifyScriptMarksFailExit(t *testing.T) {
	s, err := verifyScript([]verifyCheck{{Type: "port", Port: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "VERIFY_RESULT=FAIL") || !strings.Contains(s, "exit 1") {
		t.Fatalf("script should fail closed:\n%s", s)
	}
}

func TestCollectDiagnosticsRunsOnHarness(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	res, err := runCollectDiagnostics(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "scope": "disk",
	})
	if err != nil {
		t.Fatalf("collect_diagnostics: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "== df ==") {
		t.Fatalf("want df section, got: %s", res.Stdout)
	}
}

func TestSnapshotAndDiffRoundTrip(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()
	ctx := newToolCtx(t, h)

	snap, err := runSnapshotState(ctx, map[string]any{
		"host_id": h.hostID, "scopes": "os,load",
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !strings.Contains(snap.Stdout, "snapshot_id=") {
		t.Fatalf("missing snapshot_id: %s", snap.Stdout)
	}
	idLine := ""
	for _, ln := range strings.Split(snap.Stdout, "\n") {
		if strings.HasPrefix(ln, "snapshot_id=") {
			idLine = strings.TrimPrefix(ln, "snapshot_id=")
			break
		}
	}
	if idLine == "" {
		t.Fatal("no snapshot_id line")
	}

	diff, err := runDiffState(ctx, map[string]any{
		"host_id": h.hostID, "before": idLine,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff.Stdout, "before=") || !strings.Contains(diff.Stdout, "after=") {
		t.Fatalf("diff output unexpected: %s", diff.Stdout)
	}
}

func TestVerifyChecksPort(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	// 端口 1 几乎必然未监听 → FAIL，但工具本身应跑通并返回非 0。
	res, err := runVerifyChecks(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID,
		"checks":  `[{"type":"port","port":1}]`,
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected non-zero exit for missing port")
	}
	if !strings.Contains(res.Stdout, "FAIL") || !strings.Contains(res.Stdout, "VERIFY_RESULT=FAIL") {
		t.Fatalf("want FAIL markers, got: %s", res.Stdout)
	}
}

func TestListAndRestoreFileBackup(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()
	ctx := newToolCtx(t, h)

	const path = "/tmp/aishell-restore-demo.conf"
	h.srv.SetRemoteFile(path, "v1\n")
	if _, err := h.ssh.WriteFile(h.hostID, path, "v2\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	listed, err := runListFileBackups(ctx, map[string]any{"host_id": h.hostID, "path": path})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listed.Stdout, path+".bak.") {
		t.Fatalf("expected backup listing, got: %s", listed.Stdout)
	}

	restored, err := runRestoreFileBackup(ctx, map[string]any{"host_id": h.hostID, "path": path})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.ExitCode != 0 {
		t.Fatalf("restore exit=%d out=%s err=%s", restored.ExitCode, restored.Stdout, restored.Stderr)
	}
	got, err := h.ssh.ReadFile(h.hostID, path, 1024)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got.Stdout, "v1") {
		t.Fatalf("want restored v1, got: %s", got.Stdout)
	}
}

func TestRestoreFileBackupRejectsForeignBackup(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	_, err := runRestoreFileBackup(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID,
		"path":    "/tmp/a.conf",
		"backup":  "/tmp/other.bak.1",
	})
	if err == nil {
		t.Fatal("foreign backup should be rejected")
	}
}

func TestEvalVerifyChecksDeniesProtectedPath(t *testing.T) {
	tool, ok := lookupTool("verify_checks")
	if !ok {
		t.Fatal("missing tool")
	}
	view := &ToolCallView{
		Name:    "verify_checks",
		Args:    `{"checks":[{"type":"file_exists","path":"/etc/shadow"}]}`,
		Command: "verify",
	}
	v := tool.Evaluate(view, vault.PolicySettings{Mode: vault.ModeWhitelist})
	if v.Decision != policy.Deny {
		t.Fatalf("want deny, got %s (%s)", v.Decision, v.Reason)
	}
}

func TestNetListenAndSystemdStatus(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()
	ctx := newToolCtx(t, h)

	netRes, err := runNetListen(ctx, map[string]any{"host_id": h.hostID})
	if err != nil {
		t.Fatalf("net_listen: %v", err)
	}
	if !strings.Contains(netRes.Stdout, "== listening ==") {
		t.Fatalf("got: %s", netRes.Stdout)
	}

	// 测试夹具未必有 nginx；工具应仍返回可解析输出。
	st, err := runSystemdStatus(ctx, map[string]any{"host_id": h.hostID, "unit": "sshd"})
	if err != nil {
		t.Fatalf("systemd_status: %v", err)
	}
	if !strings.Contains(st.Stdout, "== sshd ==") && !strings.Contains(st.Stdout, "sshd") {
		// 某些极简环境可能没有 systemctl；至少不应崩溃。
		t.Logf("systemd_status output: %s", st.Stdout)
	}
}
