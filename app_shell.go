package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-shell/internal/audit"
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// cwd 哨兵：与 frontend/src/term.js 的 CWD_MARKER 保持一致。
const cwdMarker = "__AISHELL_CWD__"

// ShellResult 是交互式会话里「人直接敲的一条 shell 命令」的结果。
//
// 这条路径**经过策略引擎**（仅硬拒绝不可逆/凭据命令；与绕过策略的
// 交互终端原始按键相对），再决定是否 Exec。人是命令的所有者，故不弹审批。
// Status:
//   - done      —— 已执行（含非零退出码）
//   - denied    —— 策略硬拒绝（不可逆破坏 / 凭据读取）
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
// 裁决用**用户原文**（EvaluateHuman：仅硬拒绝不可逆/凭据命令，其余直接执行、
// 不审批）；Exec 用包装后命令（带 cwd）。未知第三方二进制先 command -v；
// 不存在则返回 missing，提示改问 LLM。
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
		Note:       "人工 shell 执行（未触发硬拒绝）",
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

// RunShellInTerminal 把一条人敲的 shell 命令送进常驻终端执行：裁决仅做
// 硬拒绝（不可逆/凭据命令，见 EvaluateHuman），普通与高危命令一律直接执行、
// 不审批；执行路径是把这一行写进常驻 PTY —— 提示符回显、输出、top/vim 接管
// 全发生在同一块表面上，与人亲手在键盘上敲完全同形。与 RunShell 的差别是
// 没有有界捕获：输出属于终端流，模型看到的是命令退出时定格的最后一帧
// （见 OpenTerminal 的 VT 管线）。
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
		Note:     "人工 shell 在常驻终端执行（未触发硬拒绝）",
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

// ClassifyShellInput 判断一行输入**像不像** shell 命令，供前端做输入分流。
//
// 为什么把这个判断放在后端：命令识别需要「Linux 命令库」这份知识，
// 而它已经在 policy 包里（PrimaryBinary / IsKnownBinary / shellBuiltins）
// 加上 vault.DefaultWhitelist（内置只读诊断清单）。前端再养一份必然漂移。
//
// 分流用的命令库**不等于**用户当前策略白名单：
// 用户把白名单清空（一切走确认）时，`ls` 仍是 Linux 命令，必须走 shell，
// 不能因为「不在白名单」就被当成自然语言交给 Agent。
// 用户白名单只作为扩展（自定义只读前缀也算命令意图）。
//
// 只做**本地**判断，不发远端探测：分流发生在用户敲回车的那一刻，
// 不能因为查远端而卡住输入。未知第三方（可能是自定义脚本）一律返回 false，
// 交给 ResolveUnknownInput（见下）做一次远端探测再定去向。
//
// 返回值刻意用「是否是命令」而不是 kind 字符串：前端的既有规则
// （? / ! 前缀、中文提问、英文问句、多行）保持不变，只在
// 「首词像不像命令」这一处改成问后端，改动面最小。
func (a *App) ClassifyShellInput(text string) bool {
	bin := policy.PrimaryBinary(text)
	if bin == "" {
		return false
	}
	// 路径形（./deploy.sh、/usr/local/bin/x）：人明确在指定一个可执行文件，
	// 即使不在命令库里也算命令意图。
	if strings.Contains(bin, "/") {
		return true
	}
	wl := vault.DefaultWhitelist()
	if a.v != nil {
		wl = append(wl, a.v.Policy().Whitelist...)
	}
	return policy.IsKnownBinary(bin, wl)
}

// RouteDecision 是「首词不认识的一行」该交给谁的裁决结果。
//
// 与 ClassifyShellInput 的分工：那个是**纯本地**判断（敲回车当场就要答案，
// 不能等远端）；这个是**补一次远端探测**后的最终去向 —— 只在
// ClassifyShellInput 已经说了「不认识」时才需要它。因此它带网络往返，
// 只在前端确认「需要这一步」时才调用。
type RouteDecision struct {
	// Kind 是最终去向："shell"（当命令跑）或 "agent"（交给 LLM 理解）。
	Kind string `json:"kind"`
	// Binary 是抽出的首词（命令名），供前端在「未找到」提示里回显。
	Binary string `json:"binary"`
	// Missing 仅在探测证明「远端不存在该命令」时为 true。
	//
	// 它是整个自动转 LLM 的安全闸门：只有它为 true 时前端才会把这一行
	// 交给 LLM。之所以这个闸门成立 —— 一个**不存在**的命令，执行它没有
	// 任何副作用，因此「交给 LLM 理解」最坏只是得到一句无用的话，
	// 而不会删/改任何东西。反之，探测**存在**的命令一律当 shell 交策略引擎，
	// 绝不自动转 LLM（那会把「执行」静默变成「提问」，违背本项目的诚实原则）。
	Missing bool `json:"missing"`
}

// ResolveUnknownInput 对「首词不在本地命令库」的一行做远端探测，决定去向。
//
// 只应在 ClassifyShellInput 返回 false 后调用。逻辑：
//
//  1. 抽不出首词（纯标点/空白）→ 没什么可执行的，按命令走（让 shell 自己报错）；
//  2. 路径形命令（./x、/opt/x）→ 人明确在指定可执行文件，按命令走，不探测；
//  3. 远端 `command -v <词>` 成功 → 该命令存在（可能是自定义脚本），按命令走；
//  4. 探测失败（不存在）→ Missing=true，Kind="agent"，交给 LLM 理解意图。
//
// 探测是**零副作用**的纯查询（`command -v` 只查 PATH，不执行）。这正是
// 「直接执行、失败才介入」得以安全成立的前提：危险命令（rm 等）的首词在
// 本地命令库里，根本走不到这里；即便走到，`command -v` 也只是查一下是否
// 存在，不会真的运行它。
func (a *App) ResolveUnknownInput(hostID, text string) RouteDecision {
	bin := policy.PrimaryBinary(text)
	if bin == "" {
		// 抽不出首词：当作命令交给 shell，让它自己报语法错误，
		// 比在这里猜「是不是提问」更诚实。
		return RouteDecision{Kind: "shell"}
	}
	base := policy.BaseName(bin)
	if strings.Contains(bin, "/") {
		// 路径形：人已明确指定可执行文件，按命令走（是否存在交给 shell）。
		return RouteDecision{Kind: "shell", Binary: base}
	}
	// 远端探测。hostID 为空或 SSH 不可达时，退回「当命令跑」——
	// 宁可让 shell 报 command not found，也不要因为探测失败就擅自转 LLM。
	if strings.TrimSpace(hostID) == "" || a.ssh == nil {
		return RouteDecision{Kind: "shell", Binary: base}
	}
	check := fmt.Sprintf("command -v -- %s >/dev/null 2>&1", shellQuote(bin))
	res, err := a.ssh.Exec(hostID, check, 15*time.Second)
	if err != nil {
		// 探测本身失败（网络等）：保守按命令走，把判断权还给 shell 与用户。
		return RouteDecision{Kind: "shell", Binary: base}
	}
	if res.ExitCode != 0 {
		// 远端确实没有这个命令 —— 安全闸门打开，交给 LLM 理解意图。
		//
		// 必须留痕：这是一次「去向/性质发生变化」的事件 —— 用户敲的是命令，
		// 系统把它改成了提问。项目里每条人工 shell 命令都写审计，这条
		// 特殊的改道若不记，事后翻日志只会看到凭空多出的一条 agent_run，
		// 无从知道它起源于一条被判为「不存在」的用户命令。
		a.auditLog(audit.Entry{
			Kind:     audit.KindDirect,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Command:  text,
			Decision: "reroute",
			Rule:     "not_found_reroute",
			Note:     "未知命令「" + base + "」远端不存在，已自动转交 LLM 理解用户意图",
		})
		return RouteDecision{Kind: "agent", Binary: base, Missing: true}
	}
	// 命令存在（可能是用户自己的脚本）：按命令走，过硬拒绝过滤。
	return RouteDecision{Kind: "shell", Binary: base}
}
