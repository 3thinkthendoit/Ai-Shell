package main

import (
	"testing"

	"ai-shell/internal/audit"
	"ai-shell/internal/vault"
)

// TestResolveUnknownInput 钉住「首词不在命令库的一行该交给谁」的裁决语义。
//
// 这个函数是自动转 LLM 的**唯一入口**，也是那条安全闸门（Missing）的唯一
// 产生点 —— 它若判错，要么把危险命令静默转成提问，要么把用户想跑的脚本
// 误当提问。所以每条分支都要钉死。
//
// 假远端对 `command -v -- <词>` 的行为：`nosuch-aishell-bin-xyz` 返回 1
// （不存在），其余一律返回 0（存在）—— 见 internal/sshtest 的 RunCommand。
func TestResolveUnknownInput(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	t.Run("路径形命令按命令走且不探测", func(t *testing.T) {
		// ./deploy.sh、/opt/app/bin/run：人明确在指定可执行文件，
		// 即使不在命令库里也算命令意图，不该发远端探测。
		// 用 Missing 必须为 false 且 Kind=shell 锁住「不转 LLM」。
		for _, in := range []string{"./deploy.sh --prod", "/opt/app/bin/run"} {
			d := a.ResolveUnknownInput(hostID, in)
			if d.Kind != "shell" {
				t.Fatalf("%q: kind=%s want shell", in, d.Kind)
			}
			if d.Missing {
				t.Fatalf("%q: 路径形命令不应标记 missing", in)
			}
		}
	})

	t.Run("抽不出首词按命令走", func(t *testing.T) {
		// 纯标点/空白抽不出主程序：交给 shell 自己报错，比猜「是不是提问」诚实。
		d := a.ResolveUnknownInput(hostID, "!!!")
		if d.Kind != "shell" || d.Missing {
			t.Fatalf("kind=%s missing=%v want shell/false", d.Kind, d.Missing)
		}
	})

	t.Run("远端存在则当命令跑", func(t *testing.T) {
		// 用户自己的脚本：远端装了 → 当命令走，绝不转 LLM。
		d := a.ResolveUnknownInput(hostID, "customscript --flag")
		if d.Kind != "shell" {
			t.Fatalf("kind=%s want shell", d.Kind)
		}
		if d.Missing {
			t.Fatalf("存在的命令不应标记 missing")
		}
		if d.Binary != "customscript" {
			t.Fatalf("binary=%q want customscript", d.Binary)
		}
	})

	t.Run("远端不存在则转 LLM 且留痕审计", func(t *testing.T) {
		// 安全闸门打开的**唯一**场景：探测证明不存在。
		d := a.ResolveUnknownInput(hostID, "nosuch-aishell-bin-xyz")
		if d.Kind != "agent" {
			t.Fatalf("kind=%s want agent", d.Kind)
		}
		if !d.Missing {
			t.Fatalf("不存在的命令必须标记 missing（这是自动转 LLM 的安全闸门）")
		}
		if d.Binary != "nosuch-aishell-bin-xyz" {
			t.Fatalf("binary=%q", d.Binary)
		}

		// 去向改变必须留痕：翻日志要能看出这条 agent 会话起源于
		// 一条被判为「不存在」的用户命令。
		entries, err := a.audit.Recent(0)
		if err != nil {
			t.Fatalf("读取审计失败: %v", err)
		}
		found := false
		for _, e := range entries {
			if e.Rule == "not_found_reroute" && e.Kind == audit.KindDirect {
				found = true
				if e.Decision != "reroute" {
					t.Fatalf("decision=%q want reroute", e.Decision)
				}
			}
		}
		if !found {
			t.Fatal("自动转 LLM 应写一条 rule=not_found_reroute 的审计记录")
		}
	})

	t.Run("无 hostID 时保守按命令走", func(t *testing.T) {
		// 没有主机就无法探测 —— 宁可让 shell 报 command not found，
		// 也不要因为探测不了就擅自转 LLM。
		d := a.ResolveUnknownInput("", "nosuch-aishell-bin-xyz")
		if d.Kind != "shell" {
			t.Fatalf("kind=%s want shell（无 hostID 保守）", d.Kind)
		}
		if d.Missing {
			t.Fatalf("无 hostID 不应标记 missing")
		}
	})
}
