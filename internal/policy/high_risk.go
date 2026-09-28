package policy

import (
	"regexp"
	"strings"
)

// highRisk 命中则需人工确认（不是硬拒绝）。
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

// IsHighRisk 判断命令是否属于「变更类高危」—— 需要人工确认，但仍可被批准执行。
func IsHighRisk(cmd string) bool {
	return HighRiskReason(cmd) != ""
}

// HighRiskReason 返回命中的高危规则说明；未命中返回空串。
// 对 normalizeForRisk 展开的每条候选匹配，避免 VAR=/wrapper/路径绕过。
func HighRiskReason(cmd string) string {
	for _, cand := range normalizeForRisk(cmd) {
		for _, h := range highRisk {
			if h.re.MatchString(cand) {
				return h.rule
			}
		}
	}
	return ""
}

// EvaluateHuman 是 Agent 会话里「人直接敲的 shell」专用裁决：
//
//	硬拒绝 → 高危需确认 → 其余自动放行。
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
	if reason := HighRiskReason(c); reason != "" {
		return Verdict{Confirm, "高危变更（" + reason + "）需人工确认", "high_risk"}
	}
	return Verdict{Allow, "非高危命令，自动执行", "auto_safe"}
}
