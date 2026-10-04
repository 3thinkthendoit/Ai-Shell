package sshclient

import (
	"fmt"
	"strings"
	"testing"
)

// hasUnquotedComment 检查命令文本里是否存在「未加引号的、位于词首的 #」。
//
// # 在 shell 里开启注释，直到行尾 —— 词首的裸 # 会把它后面**整行**的
// 内容全部吞掉。下载命令曾用没加引号的 ###B64-BEGIN### 做标记，结果远端
// 真正执行的只剩一个零参数 printf，报
//
//	printf: usage: printf [-v var] format [arguments]
//
// 而整条 head|base64 管道被当成了注释，一个字节都没传回来。
//
// 实现是按 shell 的分词规则模拟：跟踪单/双引号状态；# 只有在「词首」
// （行首、空白/分隔符之后）才开注释，词中（如 foo#bar）不算。
func hasUnquotedComment(s string) bool {
	var quote byte
	wordStart := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '#':
			if wordStart {
				return true
			}
		case ' ', '\t', '\n', ';', '&', '|':
			wordStart = true
		default:
			wordStart = false
		}
	}
	return false
}

// 先自证这个检查器本身是对的 —— 它必须能抓住那次的原始写法，
// 也不能把引号里的 # 误报成注释。
func TestHasUnquotedComment(t *testing.T) {
	// 那次事故的原始写法：# 未加引号，管道被吞。
	if !hasUnquotedComment(`printf 'SIZE:%s\n' 1; printf ###B64-BEGIN###; head -c 5 | base64`) {
		t.Fatal("必须检出未加引号的 # 注释")
	}
	// 引号里的 # 是字面量，不是注释。
	if hasUnquotedComment(`printf '###B64-BEGIN###'; head -c 5 -- '/a#b'`) {
		t.Fatal("引号内的 # 不应误报")
	}
	// 词中的 # 也不是注释。
	if hasUnquotedComment(`echo foo#bar`) {
		t.Fatal("词中的 # 不应误报")
	}
	// 分隔符后的裸 # 是注释（这是真实 shell 语义）。
	if !hasUnquotedComment(`head -c 5; # tail is comment`) {
		t.Fatal("必须检出分隔符后的注释")
	}
}

// 下载相关命令文本的 shell 语法契约。
//
// 为什么不信任假远端：sshtest 的假服务器只做**模式匹配**、按协议应答，
// 从不真的执行命令 —— 命令文本里就算有注释吞行这种致命语法错误，测试
// 照样全绿（### 标记事故正是这样漏上线的）。所以这里直接对生成的命令
// 文本做分词级检查，把「命令必须真的能被 shell 正确执行」钉死。
func TestDownloadCmds_ShellSafe(t *testing.T) {
	// 路径故意带上 ; # $ 空格：它们必须全部被引号安全地隔离。
	const p = `/srv/weird; name#x$y`

	t.Run("statSizeCmd", func(t *testing.T) {
		cmd := statSizeCmd(p)
		if hasUnquotedComment(cmd) {
			t.Fatalf("存在未加引号的 #:\n%s", cmd)
		}
		if !strings.Contains(cmd, `wc -c < '`+p+`'`) {
			t.Fatalf("路径必须带引号出现在重定向里:\n%s", cmd)
		}
	})

	t.Run("readChunkCmd", func(t *testing.T) {
		cmd := readChunkCmd(p, 2*ChunkSize, 1024)

		if hasUnquotedComment(cmd) {
			t.Fatalf("存在未加引号的 #:\n%s", cmd)
		}
		// 哨兵必须带引号出现（END 标记的 \n 在引号内一起传）。
		if !strings.Contains(cmd, `'`+b64Begin+`'`) || !strings.Contains(cmd, `'`+b64End+`\n'`) {
			t.Fatalf("哨兵必须带引号出现:\n%s", cmd)
		}
		// tail 的偏移是 **1 起**：offset+1 差一位就是整体错一个字节。
		if !strings.Contains(cmd, fmt.Sprintf("tail -c +%d -- '%s'", 2*ChunkSize+1, p)) {
			t.Fatalf("tail 偏移应为 offset+1:\n%s", cmd)
		}
		if !strings.Contains(cmd, "| head -c 1024 | base64 || exit $?") {
			t.Fatalf("缺少 head 截断或 base64 失败传播:\n%s", cmd)
		}
	})
}
