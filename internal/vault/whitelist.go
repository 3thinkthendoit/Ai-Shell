package vault

// DefaultWhitelist 返回内置的只读诊断白名单。
func DefaultWhitelist() []string {
	return append([]string(nil), defaultWhitelist()...)
}

// defaultWhitelist 是白名单模式的初始值 —— 只放「只读诊断」类命令。
// 注意刻意不含 env / printenv / cat 敏感路径，这些即使在白名单模式下也要人工确认。
//
// 判定「只读」的标准不是「看起来是查询命令」，而是**它有没有执行任意子进程的出口**。
// 按此标准刻意排除（不要重新加回来）：
//
//	awk   —— BEGIN{system("...")} 与 `cmd` | getline 两条执行出口
//	find  —— -exec / -execdir / -ok 可直接执行任意命令
//	sed   —— GNU sed 的 e 命令（本就不在清单里）
//	xargs / eval / sh / bash / perl / python —— 同上
//
// 这类命令的危险在于**不含任何分隔符**：`awk 'BEGIN{system("id")}'` 段首就是
// 白名单词，命令替换闸门也拦不住（不带引号时靠 (){} 拦得住，写成 -f script.awk 就漏了），
// 因此在白名单里出现即等于一条无人确认的任意命令执行通道。
//
// 确实需要「结构化文本处理」时，正确做法是在 Go 侧实现对应工具（例如 json_query），
// 而不是把解释器交给远端执行。
func defaultWhitelist() []string {
	return []string{
		"ls", "ll", "cat", "head", "tail", "less", "more", "wc",
		"grep", "egrep", "fgrep", "rg", "cut", "sort", "uniq", "tr",
		"stat", "file", "readlink", "realpath",
		"df", "du", "free", "uptime", "vmstat", "iostat", "sar", "nproc", "lscpu",
		"uname", "hostname", "whoami", "id", "groups", "pwd", "date", "locale",
		"ps", "top -b", "pidstat", "lsof", "last", "who", "w",
		"journalctl", "dmesg", "systemctl status", "systemctl is-active",
		"systemctl list-units", "systemctl list-unit-files", "systemctl show",
		"ip addr", "ip route", "ip link", "ss", "netstat", "ping", "traceroute",
		"dig", "nslookup", "host",
		"lsblk", "mount", "findmnt", "blkid", "lspci", "lsusb", "lsmod",
		"docker ps", "docker logs", "docker inspect", "docker images",
		"docker stats", "docker version", "docker info",
		"kubectl get", "kubectl describe", "kubectl logs", "kubectl top",
		"podman ps",
		"git status", "git log", "git diff", "git show", "git branch",
		"cat /etc/os-release", "cat /etc/hostname", "cat /etc/hosts",
		"cat /etc/resolv.conf", "cat /proc/loadavg", "cat /proc/meminfo",
		"cat /proc/cpuinfo", "cat /proc/uptime", "cat /proc/version",
		"tar -t", "tar -tf", "unzip -l", "md5sum", "sha256sum",
		"echo", "printf", "which", "type", "command -v", "basename", "dirname",
	}
}
