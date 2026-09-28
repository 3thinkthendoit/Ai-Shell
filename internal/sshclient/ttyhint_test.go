package sshclient

import (
	"strings"
	"testing"
)

// 用户在直连终端里敲 `top`，看到的是：
//
//	root@8.163.114.84:~# top
//	TERM environment variable not set.
//	退出码 1 · 18ms
//
// 报错本身完全没提「我们没给你分配终端」，用户只会以为 top 装坏了。
// 这个函数负责把这类失败翻译成一句能照做的话。
func TestTTYHintRecognizesRealFailures(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		code   int
	}{
		{"top：TERM 未设置（用户实际遇到的）", "TERM environment variable not set.\n", 1},
		{"top：procps 拿不到 tty", "top: failed tty get\n", 1},
		{"通用：not a tty", "foo: not a tty\n", 1},
		{"通用：must be a tty", "bar: must be a tty\n", 2},
		{"ioctl 失败", "tcgetattr: Inappropriate ioctl for device\n", 1},
		{"sudo：需要终端读密码", "sudo: a terminal is required to read the password\n", 1},
		{"大小写不敏感", "term environment variable not set.\n", 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TTYHint(c.stderr, c.code)
			if got == "" {
				t.Fatalf("应识别为「需要 TTY」，实得空串（stderr=%q）", c.stderr)
			}
			// 提示必须可照做，而不是复述一遍「需要终端」
			for _, want := range []string{"TTY", "top -b -n 1", "ps aux"} {
				if !strings.Contains(got, want) {
					t.Errorf("提示里应包含 %q，实得: %s", want, got)
				}
			}
		})
	}
}

// 误报比漏报更糟：一条与真实原因无关的提示会把用户带偏。
func TestTTYHintDoesNotFireOnUnrelatedFailures(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		code   int
	}{
		{"普通命令不存在", "bash: frobnicate: command not found\n", 127},
		{"文件不存在（含 tty 字样但不是签名）", "grep: /etc/tty.conf: No such file or directory\n", 2},
		{"权限不足", "ls: cannot open directory '/root': Permission denied\n", 2},
		{"空 stderr", "", 1},
		{"只有空白", "  \n\t\n", 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TTYHint(c.stderr, c.code); got != "" {
				t.Fatalf("不该触发提示，实得: %s", got)
			}
		})
	}
}

// 退出码为 0 时一律不提示。
//
// 有些程序会打这类警告但依然正常工作（例如某些 wrapper 脚本），
// 这时候弹一段「这个程序需要终端」会让人以为刚才那条命令其实失败了。
func TestTTYHintSilentOnSuccess(t *testing.T) {
	if got := TTYHint("TERM environment variable not set.\n", 0); got != "" {
		t.Fatalf("退出码 0 时不该提示，实得: %s", got)
	}
}
