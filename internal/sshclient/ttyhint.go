package sshclient

import "strings"

// ttySignatures 是「程序需要交互式终端」在**不分配伪终端**执行时的典型报错。
//
// 刻意不去维护「哪些命令是交互式的」清单：那种清单永远不全
// （top / htop / vim / nano / less / man / tmux / screen / sudo / …），
// 而且会误伤同名程序。直接识别程序自己吐出来的报错，覆盖更全，也不会冤枉普通命令。
//
// 全部小写，匹配前会把 stderr 转小写。
var ttySignatures = []string{
	"term environment variable not set",         // top 及部分 ncurses 程序
	"failed tty get",                            // procps 的 top 在无 tty 时
	"not a tty",                                 // 通用写法
	"must be a tty",                             // 通用写法
	"inappropriate ioctl for device",            // tcgetattr/ioctl 失败
	"terminal is required to read the password", // sudo
	"a tty is required",
}

// TTYHint 判断这次失败是不是「程序需要交互式终端」造成的。
//
// 命中时返回一句用户能照做的说明；否则返回空串 —— 调用方**不该**显示任何额外提示。
// 宁可漏报也不要误报：一条与真实原因无关的提示会把用户带偏，比不提示更糟。
//
// 只看 stderr、且要求退出码非 0：
//   - 只看 stderr 是因为 stdout 里出现 "not a tty" 完全可能是命令的正常输出
//     （比如 `grep "not a tty" /var/log/x`）；
//   - 要求非 0 是因为有些程序会打这种警告但依然正常工作。
func TTYHint(stderr string, exitCode int) string {
	if exitCode == 0 {
		return ""
	}
	low := strings.ToLower(stderr)
	for _, sig := range ttySignatures {
		if !strings.Contains(low, sig) {
			continue
		}
		return "这个程序需要交互式终端（TTY）。当前命令不分配伪终端，拿不到键盘输入，" +
			"所以 top、htop、vim、less、sudo 这类程序不能这样运行。" +
			"可改用非交互方式，例如 top -b -n 1 取一次快照、ps aux 看进程。"
	}
	return ""
}
