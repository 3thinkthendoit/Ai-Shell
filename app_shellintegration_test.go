package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shell integration 注入脚本的**真 bash** 回归测试。
//
// 为什么需要它（血泪教训）：注入脚本是喂给远端 bash 的**语义程序**，而所有
// 既有测试都跑在假远端上 —— sshtest 的假 shell 自己发干净的 133;C/D，
// **从不执行这段脚本**。于是「bash 的 DEBUG trap 会为 PROMPT_COMMAND 里的
// 每条命令各补一个 133;C」「promptRestore 的『一次性』其实不一次性」这类
// 纯 bash 语义的问题，全量测试完全看不见，后果却是灾难性的：
// 提示符前的最后一个标记停在 C → 客户端把「命令运行中」永久当成 true →
// 所有输入（包括自然语言）都被透传给 shell，Agent 入口整个失效。
//
// 环境依赖：需要 bash（Git for Windows / WSL / Linux 上都有）。找不到时
// **skip 并把原因说出来** —— skip 不等于通过，只是这台机器没验到。
func TestShellIntegrationLeavesPromptAtD(t *testing.T) {
	bashPath := findBashForTest()
	if bashPath == "" {
		t.Skip("本机找不到 bash（Git for Windows / WSL / Linux 上应有）—— " +
			"注入脚本未被验证，不要把这条 skip 当成通过")
	}

	dir := t.TempDir()
	rcPath := filepath.Join(dir, "rc")
	if err := os.WriteFile(rcPath, []byte("PS1=''\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 输入流 = 生产里注入的三步（两个常量逐字使用，不转抄）+ 后续命令。
	// 提示符换成哨兵串，便于在输出里定位「提示符出现在哪」。
	input := strings.Join([]string{
		"PS1=",                      // 第一步：静默
		shellIntegration,            // 第二步：装 integration
		promptRestore,               // 第三步：恢复提示符
		"stty echo",                 //     （管道里会失败报错，无妨）
		"PS1='AISHELL_PROMPT_MARK'", // 提示符哨兵
		"echo AAA-OUT",
		"echo BBB-OUT",
		"exit",
	}, "\n") + "\n"
	inputPath := filepath.Join(dir, "input")
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bashPath, "--noprofile", "--rcfile", filepath.ToSlash(rcPath), "-i")
	in, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	cmd.Stdin = in
	// stdout/stderr 合并收集：标记（stdout）与提示符（stderr）的相对顺序
	// 就是断言对象，不能分开读。bash 的 job control 告警也一起收着，无害。
	out, _ := cmd.CombinedOutput()
	text := string(out)
	if ctx.Err() != nil {
		t.Fatalf("bash 没有在超时前结束：%v\n输出:\n%s", ctx.Err(), text)
	}

	// 哨兵必须真的出现过（≥2 次）。没出现说明 promptRestore 没有把
	// PROMPT_COMMAND 装回正式值 —— PS1 被那个钩子反复重置，哨兵上不了屏。
	const sentinel = "AISHELL_PROMPT_MARK"
	n := strings.Count(text, sentinel)
	if n < 2 {
		t.Fatalf("提示符哨兵只出现 %d 次（至少应有 2 次）——\n"+
			"promptRestore 没有把 PROMPT_COMMAND 装回正式值：它必须是真·一次性，\n"+
			"跑完这一轮要把 PROMPT_COMMAND 恢复成 shellIntegration 装的那条。\n"+
			"输出:\n%s", n, text)
	}

	// 核心断言：**每个**提示符之前，最后一个 OSC 133 标记必须是 D。
	// 是 C 的话客户端会把「命令运行中」永久卡在 true —— 所有输入都被当
	// 程序输入透传给 PTY，自然语言再也进不了 Agent（真实主机踩到的 bug）。
	for i := 0; ; {
		j := strings.Index(text[i:], sentinel)
		if j < 0 {
			break
		}
		pos := i + j
		const prefix = "\x1b]133;"
		marker := strings.LastIndex(text[:pos], prefix)
		if marker < 0 {
			t.Fatalf("提示符（第 %d 字节处）之前没有任何 133 标记，序列不完整\n输出:\n%s", pos, text)
		}
		tail := text[marker+len(prefix):]
		if tail == "" || (tail[0] != 'C' && tail[0] != 'D') {
			t.Fatalf("提示符前的 133 标记形态异常：%q\n输出:\n%s", tail, text)
		}
		if tail[0] != 'D' {
			t.Fatalf("提示符前最后一个 133 标记应为 D（命令已结束），实得 %q。\n"+
				"最常见原因：PROMPT_COMMAND 里写了多条命令 —— bash 的 DEBUG trap\n"+
				"会为每条命令各补一个 133;C，最后那个 C 没有配对的 D，客户端会把\n"+
				"「命令运行中」永久卡住（所有输入都被透传，自然语言进不了 Agent）。\n"+
				"输出:\n%s", tail[0], text)
		}
		i = pos + len(sentinel)
	}

	// 形态检查：标记序列里不允许出现连续的 [D, C, 7] 三元组。
	// 正常的命令周期是 …C(命令) …C(PROMPT_COMMAND 的单条 printf) D 7：
	// D 之后紧跟的是同一个 printf 输出的 7，然后才是下一条命令的 C。
	// 「D 之后来了个 C、紧接着又来了个 7」= 有一发 133;C 夹在 D 和 7 之间
	// 没被配对（典型来源：PROMPT_COMMAND 写成两条命令时，DEBUG trap 为
	// 第二条命令补的那个 C）—— 客户端会把它当成「新命令开始」而永久卡住。
	markers := scanMarkers(text)
	for i := 0; i+2 < len(markers); i++ {
		if markers[i] == 'D' && markers[i+1] == 'C' && markers[i+2] == '7' {
			lo, hi := i-3, i+6
			if lo < 0 {
				lo = 0
			}
			if hi > len(markers) {
				hi = len(markers)
			}
			t.Fatalf("标记序列里出现 [D,C,7] 三元组（第 %d 个标记起）：…%s…\n"+
				"说明有一发 133;C 没有配对的 D 就迎来了 OSC 7 —— 命令运行态会被卡住。\n输出:\n%s",
				i, markers[lo:hi], text)
		}
	}

	// OSC 7（当前目录上报）也必须活着：启动期 1 次 + 之后每个命令周期各 1 次。
	if k := strings.Count(text, "\x1b]7;"); k < 2 {
		t.Fatalf("OSC 7 只出现 %d 次 —— 目录上报在启动后失效（promptRestore 覆盖问题的另一面）\n输出:\n%s", k, text)
	}

	t.Logf("通过：检查 %d 个提示符，最后一个标记均为 D；OSC 7 共 %d 次",
		n, strings.Count(text, "\x1b]7;"))
}

// scanMarkers 按出现顺序提取 OSC 133;C / 133;D / OSC 7 三类标记（只留类型字符）。
// 只认真正的 ESC 字节序列 —— 输入回显里出现的 `\033]133;...` 是字面文本
// （反斜杠加数字），不会误匹配。
func scanMarkers(s string) []byte {
	var out []byte
	for i := 0; i < len(s); {
		if i+7 <= len(s) && strings.HasPrefix(s[i:], "\x1b]133;") {
			if c := s[i+6]; c == 'C' || c == 'D' {
				out = append(out, c)
				i += 7
				continue
			}
		}
		if strings.HasPrefix(s[i:], "\x1b]7;") {
			out = append(out, '7')
			i += 4
			continue
		}
		i++
	}
	return out
}

// findBashForTest 找一台可用的 bash；找不到返回空串（调用方 skip）。
func findBashForTest() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	for _, p := range []string{
		`D:\soft\Git\bin\bash.exe`,
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		`C:\msys64\usr\bin\bash.exe`,
		`/bin/bash`,
		`/usr/bin/bash`,
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
