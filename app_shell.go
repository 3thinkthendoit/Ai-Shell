package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ai-shell/internal/agent"
	"ai-shell/internal/audit"
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
)

// 人工 shell 审批等待上限。与 agent 工具审批同量级：用户可能离开屏幕去核对命令。
const shellApprovalTimeout = 5 * time.Minute

// cwd 哨兵：与 frontend/src/term.js 的 CWD_MARKER 保持一致。
const cwdMarker = "__AISHELL_CWD__"

// ShellResult 是交互式会话里「人直接敲的一条 shell 命令」的结果。
//
// 这条路径**经过策略引擎**（与绕过策略的交互终端相对），再决定是否 Exec。
// Status:
//   - done      —— 已执行（含非零退出码）
//   - denied    —— 策略硬拒绝，或用户在审批里点了拒绝
//   - cancelled —— 审批超时 / 被 Stop 打断
//   - missing   —— 未知第三方命令在远端不存在；提示改走 LLM
//   - error     —— 本机侧失败（未选主机、SSH 不可达等）
type ShellResult struct {
	Status     string `json:"status"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
	Rule       string `json:"rule"`
	Risk       string `json:"risk"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Hint       string `json:"hint"`
	Truncated  bool   `json:"truncated"`
	Error      string `json:"error"`
}

// shellGate 是人工 shell 命令的审批闸门，与 Agent.approvals 分开：
// Agent 在跑一轮时也可以有工具审批，两者 ID 空间不能混用，
// 否则一边 Resolve 成功、另一边永远挂起。
type shellGate struct {
	mu        sync.Mutex
	approvals map[string]chan bool
}

func (g *shellGate) register(id string) chan bool {
	ch := make(chan bool, 1)
	g.mu.Lock()
	if g.approvals == nil {
		g.approvals = map[string]chan bool{}
	}
	g.approvals[id] = ch
	g.mu.Unlock()
	return ch
}

func (g *shellGate) resolve(id string, approved bool) bool {
	g.mu.Lock()
	ch, ok := g.approvals[id]
	if ok {
		delete(g.approvals, id)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- approved:
	default:
	}
	return true
}

func (g *shellGate) drop(id string) {
	g.mu.Lock()
	delete(g.approvals, id)
	g.mu.Unlock()
}

func (g *shellGate) cancelAll() {
	g.mu.Lock()
	ids := make([]string, 0, len(g.approvals))
	for id := range g.approvals {
		ids = append(ids, id)
	}
	g.mu.Unlock()
	for _, id := range ids {
		g.resolve(id, false)
	}
}

// wait 等到批准/拒绝，或超时/ctx 取消。超时与取消时自行从 map 清掉登记。
func (g *shellGate) wait(id string, ch <-chan bool, done <-chan struct{}) (approved bool, cancelled bool) {
	select {
	case ok := <-ch:
		return ok, false
	case <-time.After(shellApprovalTimeout):
		g.drop(id)
		return false, true
	case <-done:
		g.drop(id)
		return false, true
	}
}

func newShellID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "shell-" + hex.EncodeToString(b[:])
}

// shellQuote 用单引号包裹，供远端 bash 安全嵌入路径。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wrapShellCommand 把用户原文包成「先切到跟踪目录再执行」。
// 纯 cd 时追加哨兵打印新目录（与 frontend term.js 语义一致）。
func wrapShellCommand(cwd, cmd string) string {
	c := strings.TrimSpace(cmd)
	dir := "~"
	if cwd != "" && cwd != "~" {
		dir = shellQuote(cwd)
	}
	bare := parseBareCd(c)
	if bare != nil {
		target := "~"
		if *bare != "" && *bare != "~" {
			target = shellQuote(*bare)
		}
		return fmt.Sprintf("cd %s && cd %s && printf '%%s\\n' %s$(pwd -P)", dir, target, cwdMarker)
	}
	return fmt.Sprintf("cd %s && %s", dir, c)
}

// parseBareCd 识别「整行只是一次 cd」。返回目标；不是纯 cd 则 nil。
func parseBareCd(cmd string) *string {
	c := strings.TrimSpace(cmd)
	if c == "cd" {
		empty := ""
		return &empty
	}
	if !strings.HasPrefix(c, "cd ") && !strings.HasPrefix(c, "cd\t") {
		return nil
	}
	arg := strings.TrimSpace(c[2:])
	if arg == "" {
		empty := ""
		return &empty
	}
	if strings.ContainsAny(arg, ";&|<>()$`") {
		return nil
	}
	arg = strings.Trim(arg, `"'`)
	return &arg
}

// RunShell 在 Agent 会话里执行一条人敲的 shell 命令。
//
// 裁决用**用户原文**（EvaluateHuman：仅高危需确认）；Exec 用包装后命令（带 cwd）。
// 未知第三方二进制先 command -v；不存在则返回 missing，提示改问 LLM。
// 高危批准后会再测一次存在性，缩小 check→approve→exec 窗口。
//
// cwd 由前端跟踪；空或 "~" 表示家目录。
//
// 与 Ask 互斥：经 sessionLane 占坑，避免与 Agent 并发抢主机。
func (a *App) RunShell(hostID, command, cwd string, timeoutSec int) ShellResult {
	if err := a.ready(); err != nil {
		return ShellResult{Status: "error", Error: err.Error()}
	}
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ShellResult{Status: "error", Error: "命令为空"}
	}
	if strings.TrimSpace(hostID) == "" {
		return ShellResult{Status: "error", Error: "请先选择一台主机"}
	}
	if ok, reason := a.lane.tryEnterShell(hostID); !ok {
		return ShellResult{Status: "error", Error: reason}
	}
	defer a.lane.leaveShell()

	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	pol := a.v.Policy()
	verdict := policy.EvaluateHuman(cmd)
	risk := policy.Risk(cmd)

	base := ShellResult{
		Decision: string(verdict.Decision),
		Reason:   verdict.Reason,
		Rule:     verdict.Rule,
		Risk:     risk,
	}

	if verdict.Decision == policy.Deny {
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: string(verdict.Decision),
			Rule:     verdict.Rule,
			Note:     "人工 shell 硬拒绝：" + verdict.Reason,
		})
		base.Status = "denied"
		return base
	}

	// 未知第三方：先查远端是否存在。不在命令库里、且 command -v 失败 → 提示走 LLM。
	if miss := a.probeMissingBinary(hostID, cmd, pol.Whitelist); miss != nil {
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: string(verdict.Decision),
			Rule:     "not_found",
			Note:     "远端不存在该命令，提示改走 LLM",
		})
		base.Status = "missing"
		base.Reason = miss.Reason
		base.Hint = miss.Hint
		base.Error = miss.Reason
		return base
	}

	if verdict.Decision == policy.Confirm {
		id := newShellID()
		ch := a.shell.register(id)
		view := agent.ToolCallView{
			ID:       id,
			Name:     "shell",
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: verdict.Decision,
			Reason:   verdict.Reason,
			Rule:     verdict.Rule,
			Risk:     risk,
			Status:   "pending",
		}
		a.emit(agent.EvApproval, view)

		var done <-chan struct{}
		if a.ctx != nil {
			done = a.ctx.Done()
		}
		approved, cancelled := a.shell.wait(id, ch, done)
		if cancelled {
			a.auditLog(audit.Entry{
				Kind:     audit.KindDirect,
				HostID:   hostID,
				HostName: hostNameOf(a.v, hostID),
				Command:  cmd,
				Decision: string(verdict.Decision),
				Rule:     verdict.Rule,
				Note:     "人工 shell 审批超时或已中断",
			})
			base.Status = "cancelled"
			base.Error = "审批超时或已中断"
			return base
		}
		if !approved {
			denied := false
			a.auditLog(audit.Entry{
				Kind:     audit.KindDirect,
				HostID:   hostID,
				HostName: hostNameOf(a.v, hostID),
				Command:  cmd,
				Decision: string(verdict.Decision),
				Rule:     verdict.Rule,
				Approved: &denied,
				Note:     "用户拒绝执行人工 shell 命令",
			})
			base.Status = "denied"
			base.Reason = "用户拒绝执行"
			return base
		}
		ok := true
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: string(verdict.Decision),
			Rule:     verdict.Rule,
			Approved: &ok,
			Note:     "用户批准执行人工 shell 命令",
		})
		// 批准后到 Exec 之间再测一次，缩小窗口。
		if miss := a.probeMissingBinary(hostID, cmd, pol.Whitelist); miss != nil {
			base.Status = "missing"
			base.Reason = miss.Reason
			base.Hint = miss.Hint
			base.Error = miss.Reason
			return base
		}
	}

	execCmd := wrapShellCommand(cwd, cmd)
	res, err := a.ssh.Exec(hostID, execCmd, time.Duration(timeoutSec)*time.Second)
	hint := sshclient.TTYHint(res.Stderr, res.ExitCode)

	entry := audit.Entry{
		Kind:       audit.KindDirect,
		HostID:     hostID,
		HostName:   hostNameOf(a.v, hostID),
		Command:    cmd,
		Decision:   string(verdict.Decision),
		Rule:       verdict.Rule,
		ExitCode:   &res.ExitCode,
		DurationMs: res.DurationMs,
		Note:       "人工 shell 执行（经策略引擎）",
	}
	if err != nil {
		entry.Note = "人工 shell 执行失败：" + err.Error()
	}
	a.auditLog(entry)

	base.Stdout = res.Stdout
	base.Stderr = res.Stderr
	base.ExitCode = res.ExitCode
	base.DurationMs = res.DurationMs
	base.Hint = hint
	base.Truncated = res.Truncated
	if err != nil {
		base.Status = "error"
		base.Error = err.Error()
		return base
	}
	base.Status = "done"
	return base
}

// RunShellInTerminal 把一条人敲的 shell 命令送进常驻终端执行：裁决仍走
// 策略引擎（高危需确认），但执行路径改成把这一行写进常驻 PTY ——
// 提示符回显、输出、top/vim 接管全发生在同一块表面上，与人亲手
// 在键盘上敲完全同形。与 RunShell 的差别是没有有界捕获：输出属于
// 终端流，模型看到的是命令退出时定格的最后一帧（见 OpenTerminal 的 VT 管线）。
//
// 不做存在性探测：命令不存在时 shell 自己会报 command not found，
// 那比后端代猜一句「远端未找到」更自然，也少一次往返。
func (a *App) RunShellInTerminal(hostID, sessionID, command string) ShellResult {
	if err := a.ready(); err != nil {
		return ShellResult{Status: "error", Error: err.Error()}
	}
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ShellResult{Status: "error", Error: "命令为空"}
	}
	if strings.TrimSpace(hostID) == "" {
		return ShellResult{Status: "error", Error: "请先选择一台主机"}
	}

	verdict := policy.EvaluateHuman(cmd)
	base := ShellResult{
		Decision: string(verdict.Decision),
		Reason:   verdict.Reason,
		Rule:     verdict.Rule,
		Risk:     policy.Risk(cmd),
	}

	if verdict.Decision == policy.Deny {
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: string(verdict.Decision),
			Rule:     verdict.Rule,
			Note:     "人工 shell 硬拒绝（常驻终端）：" + verdict.Reason,
		})
		base.Status = "denied"
		return base
	}

	if verdict.Decision == policy.Confirm {
		id := newShellID()
		ch := a.shell.register(id)
		a.emit(agent.EvApproval, agent.ToolCallView{
			ID:       id,
			Name:     "shell",
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: verdict.Decision,
			Reason:   verdict.Reason,
			Rule:     verdict.Rule,
			Risk:     base.Risk,
			Status:   "pending",
		})

		var done <-chan struct{}
		if a.ctx != nil {
			done = a.ctx.Done()
		}
		approved, cancelled := a.shell.wait(id, ch, done)
		if cancelled {
			a.auditLog(audit.Entry{
				Kind:     audit.KindDirect,
				HostID:   hostID,
				HostName: hostNameOf(a.v, hostID),
				Command:  cmd,
				Decision: string(verdict.Decision),
				Rule:     verdict.Rule,
				Note:     "人工 shell 审批超时或已中断（常驻终端）",
			})
			base.Status = "cancelled"
			base.Error = "审批超时或已中断"
			return base
		}
		if !approved {
			denied := false
			a.auditLog(audit.Entry{
				Kind:     audit.KindDirect,
				HostID:   hostID,
				HostName: hostNameOf(a.v, hostID),
				Command:  cmd,
				Decision: string(verdict.Decision),
				Rule:     verdict.Rule,
				Approved: &denied,
				Note:     "用户拒绝执行人工 shell 命令（常驻终端）",
			})
			base.Status = "denied"
			base.Reason = "用户拒绝执行"
			return base
		}
		ok := true
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  cmd,
			Decision: string(verdict.Decision),
			Rule:     verdict.Rule,
			Approved: &ok,
			Note:     "用户批准执行人工 shell 命令（常驻终端）",
		})
	}

	p := a.ssh.PTY(termKey(hostID, sessionID))
	if p == nil {
		base.Status = "error"
		base.Error = "常驻终端没有打开"
		return base
	}
	a.auditLog(audit.Entry{
		Kind:     audit.KindDirect,
		HostID:   hostID,
		HostName: hostNameOf(a.v, hostID),
		Command:  cmd,
		Decision: string(verdict.Decision),
		Rule:     verdict.Rule,
		Note:     "人工 shell 在常驻终端执行（经策略引擎）",
	})
	if err := p.Write([]byte(cmd + "\n")); err != nil && !errors.Is(err, sshclient.ErrPTYClosed) {
		base.Status = "error"
		base.Error = err.Error()
		return base
	}
	base.Status = "done"
	return base
}

type missingProbe struct {
	Reason string
	Hint   string
}

// probeMissingBinary 对未知第三方做 command -v；已知库/内建返回 nil。
// 探测本身失败时返回带 Reason 的 missing（调用方当 missing 处理），
// 网络错误则 Reason 以「探测失败」开头。
func (a *App) probeMissingBinary(hostID, cmd string, whitelist []string) *missingProbe {
	bin := policy.PrimaryBinary(cmd)
	if bin == "" || policy.IsKnownBinary(bin, whitelist) {
		return nil
	}
	check := fmt.Sprintf("command -v -- %s >/dev/null 2>&1", shellQuote(bin))
	res, err := a.ssh.Exec(hostID, check, 15*time.Second)
	if err != nil {
		return &missingProbe{
			Reason: "探测命令是否存在失败：" + err.Error(),
			Hint:   "请检查 SSH 连接后重试，或用中文/? 交给 Agent。",
		}
	}
	if res.ExitCode != 0 {
		return &missingProbe{
			Reason: "远端未找到命令「" + policy.BaseName(bin) + "」",
			Hint:   "这不像内置 Linux 命令库里的工具。可用中文描述需求，或加 ? 前缀交给 Agent；若已安装请确认 PATH。",
		}
	}
	return nil
}

func (a *App) resolveShell(id string, approved bool) bool {
	return a.shell.resolve(id, approved)
}

func (a *App) cancelShellApprovals() {
	a.shell.cancelAll()
}
