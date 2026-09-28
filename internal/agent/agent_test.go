package agent

import (
	"errors"
	"strings"
	"testing"

	"ai-shell/internal/audit"
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
)

func TestProtectedProbeForReadTools(t *testing.T) {
	cases := []struct {
		tool, path, want string
	}{
		{"read_file", "/etc/shadow", "cat /etc/shadow"},
		{"list_dir", "/root/.ssh", "ls /root/.ssh"},
		{"system_info", "", ""}, // 没有路径参数 → 空探针
		{"read_file", "", ""},   // 缺参数 → 空探针
		{"unknown_tool", "/x", ""},
	}
	for _, c := range cases {
		if got := protectedProbe(c.tool, c.path); got != c.want {
			t.Fatalf("protectedProbe(%q, %q) = %q，期望 %q", c.tool, c.path, got, c.want)
		}
	}
}

// 回归测试：读类工具在手动模式下需确认、白名单模式下自动放行，
// 且无论如何都不能绕过凭据路径的硬拒绝。
func TestReadToolVerdicts(t *testing.T) {
	// 无路径（system_info）：手动 → 确认
	v := policy.EvaluateProtected(protectedProbe("system_info", ""))
	if v.Decision != policy.Allow {
		t.Fatalf("system_info 的路径检查应放行，实际 %s", v.Decision)
	}

	// 凭据路径：无论什么模式都拒绝
	v = policy.EvaluateProtected(protectedProbe("read_file", "~/.ssh/id_rsa"))
	if v.Decision != policy.Deny {
		t.Fatalf("读取 ~/.ssh/id_rsa 应硬拒绝，实际 %s", v.Decision)
	}

	// 普通路径：路径检查放行，由模式决定
	v = policy.EvaluateProtected(protectedProbe("read_file", "/etc/nginx/nginx.conf"))
	if v.Decision != policy.Allow {
		t.Fatalf("读取普通配置应通过路径检查，实际 %s", v.Decision)
	}
}

func TestFormatResultIncludesExitCode(t *testing.T) {
	out := formatResult(sshclient.Result{Stdout: "hello\n", ExitCode: 0, DurationMs: 12})
	if out == "" {
		t.Fatal("formatResult 不应返回空串")
	}
	if !strings.Contains(out, "exit=0") || !strings.Contains(out, "hello") {
		t.Fatalf("formatResult 内容不完整: %q", out)
	}

	empty := formatResult(sshclient.Result{ExitCode: 1})
	if !strings.Contains(empty, "(无输出)") {
		t.Fatalf("无输出时应给出占位提示: %q", empty)
	}
}

// 交互式命令在无 PTY 下必然失败。LLM 若不知道这是环境限制，
// 会一遍遍重试同一个 `top`，把整轮排查耗光 —— 所以要把出路写进它看到的结果里。
func TestFormatResultTellsLLMAboutTTYLimitation(t *testing.T) {
	out := formatResult(sshclient.Result{
		Stderr:   "TERM environment variable not set.\n",
		ExitCode: 1,
	})

	for _, want := range []string{"TTY", "top -b -n 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("应告诉 LLM 改用非交互方式（缺 %q）: %q", want, out)
		}
	}
	// 必须标明这段是客户端加的：远端输出一律不可信，而这段是可信的说明，
	// 不标清楚就会被混进同一种语义里。
	if !strings.Contains(out, "added by the client") {
		t.Errorf("应标明这段不是远端输出: %q", out)
	}
}

// 正常结果不该带这段说明 —— 每轮都塞会浪费 token，也会稀释真正要看的内容。
func TestFormatResultNoTTYNoteForNormalResult(t *testing.T) {
	out := formatResult(sshclient.Result{Stdout: "hi\n", ExitCode: 0, DurationMs: 1})
	if strings.Contains(out, "added by the client") {
		t.Fatalf("正常结果不该带 TTY 说明: %q", out)
	}
}

// 普通失败（非 TTY 问题）也不该带 —— 误报会把 LLM 带偏。
func TestFormatResultNoTTYNoteForOrdinaryFailure(t *testing.T) {
	out := formatResult(sshclient.Result{Stderr: "bash: x: command not found\n", ExitCode: 127})
	if strings.Contains(out, "added by the client") {
		t.Fatalf("普通失败不该带 TTY 说明: %q", out)
	}
}

// ---- 审计写入失败必须可见 ----

// 永远写入失败的审计器，模拟磁盘满 / 密钥临时不可用。
type failingAuditor struct{ err error }

func (f failingAuditor) Append(audit.Entry) error { return f.err }

// 永远成功的审计器。
type okAuditor struct{}

func (okAuditor) Append(audit.Entry) error { return nil }

// 审计写入失败不能静默 —— 必须发 audit:error，让界面给出持久告警。
//
// 这是「审计日志最不该有的失败模式」可见性的一半。另一半在 audit 包里：
// 失败时故意推进 Seq，在链上留下序号空洞，使 Verify() 能报出不完整
// （见 audit.TestAppendFailureLeavesDetectableGap）。
// 两者缺一不可 —— 链上有空洞但没人看到，用户照样以为轨迹完整。
func TestAuditFailureEmitsEvent(t *testing.T) {
	var events []string
	var payloads []any
	a := New(nil, nil, func(ev string, pay any) {
		events = append(events, ev)
		payloads = append(payloads, pay)
	})
	a.SetAuditor(failingAuditor{err: errors.New("磁盘已满")})

	a.log(audit.Entry{Kind: audit.KindTool, Tool: "run_command"})

	if len(events) != 1 {
		t.Fatalf("审计失败应恰好发一个事件，实得 %d 个: %v", len(events), events)
	}
	if events[0] != EvAuditError {
		t.Fatalf("事件名应为 %q，实得 %q", EvAuditError, events[0])
	}
	m, ok := payloads[0].(map[string]string)
	if !ok {
		t.Fatalf("载荷应为 map[string]string，实得 %T", payloads[0])
	}
	if !strings.Contains(m["message"], "磁盘已满") {
		t.Fatalf("提示应带上底层错误，实得 %q", m["message"])
	}
	if !strings.Contains(m["message"], "审计轨迹可能已不完整") {
		t.Fatalf("提示应说明后果，而不只是报错，实得 %q", m["message"])
	}
}

// 审计成功时不该发事件 —— 否则正常运行时全是噪音，真告警反而被淹没。
func TestAuditSuccessEmitsNothing(t *testing.T) {
	n := 0
	a := New(nil, nil, func(string, any) { n++ })
	a.SetAuditor(okAuditor{})
	a.log(audit.Entry{Kind: audit.KindTool})
	if n != 0 {
		t.Fatalf("审计成功时不应发事件，实得 %d 个", n)
	}
}

// 未注入审计器时不能 panic、也不能发事件。
func TestAuditWithoutAuditorIsNoop(t *testing.T) {
	a := New(nil, nil, func(string, any) { t.Fatal("没有审计器时不应发事件") })
	a.log(audit.Entry{Kind: audit.KindTool})
}
