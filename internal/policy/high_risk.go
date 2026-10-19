package policy

import (
	"regexp"
	"strings"
)

// highRisk 命中则需人工确认（不是硬拒绝）。仅作用于 LLM 路径（Evaluate）；
// 人工 shell 走 EvaluateHuman，不看高危。
//
// 覆盖「会改系统状态」的常见操作：删/建/改文件与权限、装包、启停服务、
// 杀进程、危险管道与重定向。极高危（mkfs、rm -rf /…）仍由 destructive 硬拒绝。
//
// 匹配前先走 normalizeForRisk（剥 VAR=/sudo/command|time|env、路径 basename、bash -c），
// 避免 `FOO=1 mount`、`command rm`、`/bin/rm` 绕过。
var highRisk = []struct {
	re   *regexp.Regexp
	rule string
}{
	// 删 / 建 / 改文件与目录
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?rm\b`), "删除文件/目录"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?rmdir\b`), "删除目录"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?unlink\b`), "删除文件"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?mkdir\b`), "创建目录"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?touch\b`), "创建/更新文件"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?truncate\b`), "截断文件"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:mv|cp)\b`), "移动/复制（可能覆写）"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:ln|install)\b`), "创建链接/安装文件"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:chmod|chown|chgrp)\b`), "修改权限/属主"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?tee\b`), "写入文件（tee）"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?dd\b`), "块设备/文件写入（dd）"},
	{regexp.MustCompile(`(?i)\bsed\s+[^\n]*-i`), "原地改文件（sed -i）"},

	// 重定向写
	{regexp.MustCompile(`(?:^|[^0-9])>{1,2}\s*\S+`), "输出重定向写文件"},

	// 管道进解释器 / 危险下载执行
	{regexp.MustCompile(`(?i)\|\s*(?:sudo\s+)?(?:sh|bash|zsh|dash|ksh|python\d*|perl|ruby|node)\b`), "管道进解释器"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:eval|exec|source|\.)\s`), "eval/source 执行"},
	{regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^|;]*\|\s*(?:sudo\s+)?(?:sh|bash)`), "下载并管道执行"},

	// 包管理 / 服务变更
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:apt|apt-get|yum|dnf|zypper|pacman|apk)\b[^|;]*\b(?:install|remove|erase|purge|upgrade|dist-upgrade)\b`), "包管理变更"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?systemctl\s+(?:start|stop|restart|reload|enable|disable|mask|unmask)\b`), "变更 systemd 服务"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?service\s+\S+\s+(?:start|stop|restart|reload)\b`), "变更 service"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:kill|pkill|killall)\b`), "结束进程"},

	// 账户 / 挂载 / 网络策略（未到硬拒绝阈值的）
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:useradd|adduser|usermod|passwd|visudo|groupadd)\b`), "账户/提权配置变更"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:mount|umount|fdisk|parted|pvcreate|lvcreate|vgcreate)\b`), "磁盘/挂载变更"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?(?:iptables|nft|ufw|firewall-cmd)\b`), "防火墙变更"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?docker\s+(?:rm|rmi|run|kill|stop|start|exec|commit|build)\b`), "Docker 变更类操作"},
	{regexp.MustCompile(`(?i)(?:^|[|;&]\s*)(?:sudo\s+)?kubectl\s+(?:apply|delete|create|replace|patch|scale|rollout|exec)\b`), "Kubernetes 变更类操作"},
}

// IsHighRisk 判断命令是否属于「变更类高危」—— 需要人工确认，但仍可被批准执行（仅 LLM 路径 Evaluate；人工 shell 不审批）。
func IsHighRisk(cmd string) bool {
	return HighRiskReason(cmd) != ""
}

// InteractiveBlockReason 判断命令会不会在「无 TTY、stdin 已关」的 Exec 上等待键盘。
//
// 典型是批准后的 `rm -i`：部分 rm 实现（busybox）在 EOF 上仍阻塞读确认，
// Agent 整轮挂死，界面只能靠「中断」解开。命中后调用方应直接失败返回，不要 Exec。
// `--interactive=never` 与不含 i 的短选项（`-fv`）不算。
func InteractiveBlockReason(cmd string) string {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return ""
	}
	for _, cand := range normalizeForRisk(c) {
		if why := interactiveOn(cand); why != "" {
			return why
		}
	}
	return ""
}

func interactiveOn(cmd string) string {
	s := commandAfterWrappers(cmd)
	word, rest := firstWord(s)
	name := strings.ToLower(BaseName(word))
	switch name {
	case "rm", "cp", "mv", "ln":
		if hasPromptFlag(rest) {
			return name + " 的 -i/--interactive 会等待键盘确认"
		}
	case "apt", "apt-get", "yum", "dnf":
		if packageChangePrompts(rest) {
			return name + " 未带 -y/--yes，会在无终端上等待确认"
		}
	case "passwd":
		if passwdPrompts(rest) {
			return "passwd 会等待输入口令"
		}
	}
	return ""
}

func packageChangePrompts(rest string) bool {
	change, yes := false, false
	s := rest
	for {
		w, r := firstWord(s)
		if w == "" {
			break
		}
		low := strings.ToLower(w)
		switch low {
		case "install", "remove", "erase", "purge", "upgrade", "dist-upgrade", "reinstall", "autoremove":
			change = true
		case "-y", "--yes", "--assumeyes":
			yes = true
		}
		if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w, "yY") {
			yes = true
		}
		s = r
	}
	return change && !yes
}

func passwdPrompts(rest string) bool {
	s := rest
	for {
		w, r := firstWord(s)
		if w == "" {
			return true
		}
		switch strings.ToLower(w) {
		case "-s", "--status", "-d", "--delete", "-l", "--lock", "-u", "--unlock", "-e", "--expire":
			return false
		}
		if !strings.HasPrefix(w, "-") {
			return true
		}
		s = r
	}
}

func hasPromptFlag(rest string) bool {
	s := rest
	for {
		w, r := firstWord(s)
		if w == "" || w == "--" || !strings.HasPrefix(w, "-") {
			return false
		}
		low := strings.ToLower(w)
		if low == "--interactive=never" {
			s = r
			continue
		}
		if low == "--interactive" || strings.HasPrefix(low, "--interactive=") {
			return true
		}
		if strings.HasPrefix(w, "--") {
			s = r
			continue
		}
		if strings.ContainsAny(w[1:], "iI") {
			return true
		}
		s = r
	}
}

// HighRiskReason 返回命中的高危规则说明；未命中返回空串。
// 对 normalizeForRisk 展开的每条候选匹配，避免 VAR=/wrapper/路径绕过。
//
// 匹配前先剥掉「丢弃输出」的重定向（> /dev/null、2>&1 等）：这类重定向不写任何
// 真实文件，不该被「输出重定向写文件」这条规则命中。不剥的话，
// `systemctl status nginx 2>/dev/null` 会被误判为高危、连白名单模式都要人工确认 ——
// 而它是运维里最常见的只读诊断写法。真正写文件的重定向（> /tmp/x、2> err.log）
// 不受影响，照旧命中。剥离用的规则与白名单放行同一套（stripDiscardRedirects），
// 保证两处判定一致、不会出现「放行算丢弃、高危又算写文件」的自相矛盾。
func HighRiskReason(cmd string) string {
	for _, cand := range normalizeForRisk(stripDiscardRedirects(cmd)) {
		for _, h := range highRisk {
			if h.re.MatchString(cand) {
				return h.rule
			}
		}
	}
	return ""
}

// EvaluateHuman 是「人在控制台直接敲的 shell」专用裁决：
//
//	硬拒绝 → 其余一律直接放行（不审批）。
//
// 为什么不再对高危命令要求确认：控制台是「人是所有者」的真终端，人对自己
// 敲的命令负责，审批自己没有意义（与 SSH 直连一致）。安全边界改成按**命令
// 来源**分层 —— 只有 AI 生成的命令才需人工确认（见 Evaluate），因为 AI 会
// 误判、会被远端输出里的提示注入诱导。
//
// 因此这里只保留硬拒绝：挡掉不可逆的破坏命令（rm -rf /、mkfs …）与凭据
// 读取（/etc/shadow、~/.ssh …）。定位是「防手滑」而非「防刻意」—— 它只在
// 「本地行编辑 → 回车提交」这条路上生效；用户若用原始按键直通（Tab /
// passthrough）把命令灌进 PTY 仍会绕过（那条路零策略，见 WriteTerminal）。
// 这是有意的取舍，用来换取 Tab 补全 / vim / top 等原生终端体验。
//
// 不走「手动模式条条确认」，也不要求命中只读白名单 —— 白名单是给 LLM 工具用的命令库。
// 未知第三方命令是否存在，由调用方在 Exec 前做 command -v 检查。
func EvaluateHuman(cmd string) Verdict {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return Verdict{Deny, "空命令", "empty"}
	}
	if v := EvaluateProtected(c); v.Decision == Deny {
		return v
	}
	return Verdict{Allow, "人工 shell 命令直接执行（仅硬拒绝不可逆/凭据操作）", "auto_safe"}
}
