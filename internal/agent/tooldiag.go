package agent

// 运维诊断工具：采集包 + 结构化只读探针。
//
// 目标不是再给模型一把更自由的 shell，而是用固定脚本把「打开到定位」
// 所需的信号一次采齐，并让 journal / systemd / 监听端口走结构化出口，
// 减少对裸 run_command 的依赖（解释器类与拼接命令更容易撞策略）。

import (
	"fmt"
	"strings"
	"time"

	"ai-shell/internal/sshclient"
)

// ---- collect_diagnostics ----

var toolCollectDiagnostics = Tool{
	Name: "collect_diagnostics",
	Def: def(
		"collect_diagnostics",
		"按范围采集故障诊断包并返回结构化分段输出。"+
			"比 system_info 更深：默认含失败服务、近期 journal 错误、负载/内存/磁盘/监听端口。"+
			"排查故障时优先用它建立证据，再针对性深入。"+
			"scope 可选：default（默认）、network、disk、systemd、journal、full。",
		map[string]any{
			"host_id": hostIDSchema(),
			"scope":   strSchema("采集范围：default / network / disk / systemd / journal / full，默认 default"),
			"unit":    strSchema("可选：限定 journal/systemd 到某个 unit 名，如 nginx"),
			"since":   strSchema("可选：journal 时间窗，如 1h、2h、today，默认 1h"),
		},
		"host_id",
	),
	Evaluate: evalReadTool,
	Run:      runCollectDiagnostics,
}

func runCollectDiagnostics(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	scope := strings.ToLower(strings.TrimSpace(strArg(args, "scope")))
	if scope == "" {
		scope = "default"
	}
	unit, err := optionalUnit(strArg(args, "unit"))
	if err != nil {
		return sshclient.Result{}, err
	}
	since := sanitizeSince(strArg(args, "since"))
	script, err := diagnosticsScript(scope, unit, since)
	if err != nil {
		return sshclient.Result{}, err
	}
	return ctx.ssh.Exec(hostID, script, 90*time.Second)
}

func diagnosticsScript(scope, unit, since string) (string, error) {
	var parts []string
	parts = append(parts, `echo "== meta =="; date -Is 2>/dev/null || date; hostname 2>/dev/null; echo "scope=`+scope+`"`)
	add := func(name, body string) {
		parts = append(parts, `echo "== `+name+` =="; `+body)
	}
	switch scope {
	case "default":
		add("os", `cat /etc/os-release 2>/dev/null | head -6; uname -a`)
		add("load", `uptime; nproc 2>/dev/null`)
		add("memory", `free -h 2>/dev/null`)
		add("disk", `df -hT 2>/dev/null | head -16`)
		add("failed_units", `systemctl --failed --no-pager 2>/dev/null | head -30`)
		add("top_procs", `ps -eo pid,comm,%cpu,%mem --sort=-%mem 2>/dev/null | head -12`)
		add("listening", `(ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | head -30`)
		add("journal_err", journalCmd(unit, since, 80))
	case "network":
		add("listening", `(ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | head -40`)
		add("routes", `ip -brief addr 2>/dev/null; echo ---; ip route 2>/dev/null | head -20`)
		add("dns", `cat /etc/resolv.conf 2>/dev/null | head -20`)
		add("journal_net", journalCmd(unit, since, 60))
	case "disk":
		add("df", `df -hT 2>/dev/null`)
		add("inodes", `df -i 2>/dev/null | head -16`)
		add("mounts", `findmnt -rno TARGET,SOURCE,FSTYPE,OPTIONS 2>/dev/null | head -40 || mount 2>/dev/null | head -40`)
	case "systemd":
		add("failed_units", `systemctl --failed --no-pager 2>/dev/null | head -40`)
		if unit != "" {
			add("unit_status", `systemctl status --no-pager -l `+shellQuote(unit)+` 2>&1 | head -60`)
			add("unit_journal", journalCmd(unit, since, 80))
		} else {
			add("recent_starts", `systemctl list-units --type=service --state=running --no-pager 2>/dev/null | head -40`)
		}
	case "journal":
		add("journal_err", journalCmd(unit, since, 120))
	case "full":
		add("os", `cat /etc/os-release 2>/dev/null | head -6; uname -a`)
		add("load", `uptime; nproc 2>/dev/null`)
		add("memory", `free -h 2>/dev/null`)
		add("disk", `df -hT 2>/dev/null | head -16`)
		add("inodes", `df -i 2>/dev/null | head -12`)
		add("failed_units", `systemctl --failed --no-pager 2>/dev/null | head -40`)
		add("top_cpu", `ps -eo pid,comm,%cpu,%mem --sort=-%cpu 2>/dev/null | head -12`)
		add("top_mem", `ps -eo pid,comm,%cpu,%mem --sort=-%mem 2>/dev/null | head -12`)
		add("listening", `(ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | head -40`)
		add("routes", `ip -brief addr 2>/dev/null | head -20`)
		add("journal_err", journalCmd(unit, since, 100))
	default:
		return "", fmt.Errorf("未知 scope %q，可选：default / network / disk / systemd / journal / full", scope)
	}
	return strings.Join(parts, "; "), nil
}

func journalCmd(unit, since string, lines int) string {
	if lines <= 0 {
		lines = 80
	}
	cmd := fmt.Sprintf("journalctl -p err..alert --no-pager -n %d --since %s 2>/dev/null", lines, shellQuote(since))
	if unit != "" {
		cmd = fmt.Sprintf("journalctl -u %s -p err..alert --no-pager -n %d --since %s 2>/dev/null || journalctl -u %s --no-pager -n %d --since %s 2>/dev/null",
			shellQuote(unit), lines, shellQuote(since), shellQuote(unit), lines, shellQuote(since))
	}
	return cmd
}

// ---- journal_query ----

var toolJournalQuery = Tool{
	Name: "journal_query",
	Def: def(
		"journal_query",
		"查询 systemd journal（只读）。用于按 unit / 优先级 / 时间窗拉取日志，比自己拼 journalctl 更稳。",
		map[string]any{
			"host_id":   hostIDSchema(),
			"unit":      strSchema("可选：unit 名，如 nginx.service 或 nginx"),
			"priority":  strSchema("可选：优先级上限，如 err、warning、info，默认 err"),
			"since":     strSchema("可选：时间窗，如 1h、2h、today，默认 1h"),
			"lines":     intSchema("最多返回行数，默认 80，上限 300"),
			"grep":      strSchema("可选：固定字符串过滤（grep -F），不要用正则"),
		},
		"host_id",
	),
	Evaluate: evalReadTool,
	Run:      runJournalQuery,
}

func runJournalQuery(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	unit, err := optionalUnit(strArg(args, "unit"))
	if err != nil {
		return sshclient.Result{}, err
	}
	prio := sanitizePriority(strArg(args, "priority"))
	since := sanitizeSince(strArg(args, "since"))
	lines := numArg(args, "lines", 80)
	if lines > 300 {
		lines = 300
	}
	grep := strArg(args, "grep")
	if strings.ContainsAny(grep, "\n\r;|&`$") {
		return sshclient.Result{}, fmt.Errorf("grep 含非法字符，只允许普通固定字符串")
	}

	var b strings.Builder
	b.WriteString("journalctl --no-pager")
	if unit != "" {
		b.WriteString(" -u ")
		b.WriteString(shellQuote(unit))
	}
	b.WriteString(" -p ")
	b.WriteString(shellQuote(prio))
	b.WriteString(fmt.Sprintf(" -n %d --since %s 2>/dev/null", lines, shellQuote(since)))
	if grep != "" {
		b.WriteString(" | grep -F -- ")
		b.WriteString(shellQuote(grep))
		b.WriteString(" | head -n ")
		b.WriteString(fmt.Sprintf("%d", lines))
	}
	return ctx.ssh.Exec(hostID, b.String(), 60*time.Second)
}

// ---- systemd_status ----

var toolSystemdStatus = Tool{
	Name: "systemd_status",
	Def: def(
		"systemd_status",
		"查看一个或多个 systemd unit 的状态（systemctl status / is-active / is-enabled）。只读。",
		map[string]any{
			"host_id": hostIDSchema(),
			"unit":    strSchema("unit 名，如 nginx 或 nginx.service；多个用逗号分隔，最多 8 个"),
		},
		"host_id", "unit",
	),
	Evaluate: evalReadTool,
	Run:      runSystemdStatus,
}

func runSystemdStatus(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	raw := strArg(args, "unit")
	if hostID == "" || strings.TrimSpace(raw) == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 与 unit 都不能为空")
	}
	// 任一片段非法则整体失败，避免「nginx;rm」被静默丢掉后只查到空结果。
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if sanitizeUnit(part) == "" {
			return sshclient.Result{}, fmt.Errorf("unit 非法: %q", part)
		}
	}
	units := splitUnits(raw)
	if len(units) == 0 {
		return sshclient.Result{}, fmt.Errorf("unit 无效")
	}
	if len(units) > 8 {
		return sshclient.Result{}, fmt.Errorf("一次最多查询 8 个 unit")
	}
	var parts []string
	for _, u := range units {
		q := shellQuote(u)
		parts = append(parts,
			fmt.Sprintf(`echo "== %s =="; systemctl is-active %s 2>&1; systemctl is-enabled %s 2>&1; systemctl status --no-pager -l %s 2>&1 | head -50`,
				u, q, q, q))
	}
	return ctx.ssh.Exec(hostID, strings.Join(parts, "; "), 60*time.Second)
}

// ---- net_listen ----

var toolNetListen = Tool{
	Name: "net_listen",
	Def: def(
		"net_listen",
		"列出本机监听端口（ss/netstat）及对应进程。只读。可按端口号过滤。",
		map[string]any{
			"host_id": hostIDSchema(),
			"port":    intSchema("可选：只看某个端口号"),
		},
		"host_id",
	),
	Evaluate: evalReadTool,
	Run:      runNetListen,
}

func runNetListen(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	port := numArg(args, "port", 0)
	base := `(ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null)`
	cmd := base + ` | head -80`
	if port > 0 && port < 65536 {
		cmd = fmt.Sprintf(`%s | grep -E ':%d\\s|:%d$' | head -40 || true`, base, port, port)
	}
	return ctx.ssh.Exec(hostID, `echo "== listening =="; `+cmd, 30*time.Second)
}

// ---- 参数清洗（防命令注入）----

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sanitizeUnit(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// unit 名只允许常见字符，拒绝空格与 shell 元字符。
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == '@' || r == ':' {
			continue
		}
		return ""
	}
	if len(s) > 128 {
		return s[:128]
	}
	return s
}

func sanitizeSince(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "1h"
	}
	switch s {
	case "today", "yesterday", "now":
		return s
	}
	// 允许 30m / 1h / 2h / 1d 这类简写，或 journalctl 能吃的短时间串。
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == ':' || r == ' ' || r == 'T' || r == '+' {
			continue
		}
		return "1h"
	}
	if len(s) > 64 {
		return "1h"
	}
	return s
}

func sanitizePriority(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "err", "error":
		return "err"
	case "emerg", "alert", "crit", "warning", "notice", "info", "debug":
		return s
	default:
		return "err"
	}
}

func splitUnits(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		u := sanitizeUnit(p)
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}
