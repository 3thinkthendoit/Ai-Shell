package agent

// 工具注册表的测试。
//
// 重点保护两件事：
//   1. 注册表里的每个工具都同时具备三份能力（Schema / 策略 / 执行）——
//      缺任何一份都会让该工具静默失效，而这是重构前「三处 switch」
//      最常出的错。
//   2. 新增工具必须同步更新系统提示里的说明，否则模型知道有工具
//      却不知道怎么用。

import (
	"strings"
	"testing"

	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

// 注册表里不得有重复名字。
//
// 重复的后果不是编译错误，而是 lookupTool 永远命中第一个，
// 第二个工具**完全无法被调用** —— 而它的 Schema 照样会发给 LLM，
// 于是模型会反复尝试一个必然失败的工具。
func TestToolRegistryNoDuplicateNames(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range toolRegistry() {
		if seen[tool.Name] {
			t.Errorf("工具 %q 在注册表里重复出现", tool.Name)
		}
		seen[tool.Name] = true
	}
}

// 每个工具都必须三份能力齐全。
func TestToolRegistryEntriesAreComplete(t *testing.T) {
	for _, tool := range toolRegistry() {
		if tool.Name == "" {
			t.Errorf("注册表里有一个工具没有名字")
		}
		if tool.Def.Function.Name == "" {
			t.Errorf("工具 %q 缺少 Def.Function.Name", tool.Name)
		}
		// Def 的名字必须与注册键一致：不一致时 LLM 按 Schema 里的
		// 名字调用，而 lookupTool 按注册键查找，两边对不上就是
		// 「模型调了一个不存在的工具」。
		if tool.Def.Function.Name != tool.Name {
			t.Errorf("工具注册键 %q 与 Def.Function.Name %q 不一致",
				tool.Name, tool.Def.Function.Name)
		}
		if tool.Def.Function.Description == "" {
			t.Errorf("工具 %q 缺少描述", tool.Name)
		}
		if tool.Def.Type != "function" {
			t.Errorf("工具 %q 的 Type 应为 function，实得 %q", tool.Name, tool.Def.Type)
		}
		if tool.Evaluate == nil {
			t.Errorf("工具 %q 缺少 Evaluate（策略裁决）—— 它会被当成无策略工具", tool.Name)
		}
		if tool.Run == nil {
			t.Errorf("工具 %q 缺少 Run（执行体）—— 调用它会静默失败", tool.Name)
		}
	}
}

// lookupTool 必须能按名字找到每个注册的工具。
func TestLookupToolFindsEveryRegisteredTool(t *testing.T) {
	for _, tool := range toolRegistry() {
		got, ok := lookupTool(tool.Name)
		if !ok {
			t.Errorf("lookupTool(%q) 找不到，但它在注册表里", tool.Name)
			continue
		}
		if got.Name != tool.Name {
			t.Errorf("lookupTool(%q) 返回了 %q", tool.Name, got.Name)
		}
	}
}

// 查不到的名字必须返回 false，而不是一个零值 Tool。
//
// 零值 Tool 的 Evaluate/Run 都是 nil，调用方若不加判断就会 panic。
func TestLookupToolUnknownReturnsFalse(t *testing.T) {
	if _, ok := lookupTool("no_such_tool"); ok {
		t.Error("不存在的工具应返回 false")
	}
	if _, ok := lookupTool(""); ok {
		t.Error("空名字应返回 false")
	}
}

// 禁止跨主机时，所有带 host_id 的工具描述里都必须带上那条约束。
//
// 这是「工具描述与系统提示说同一套规则」的保障：两处不一致时
// 部分模型会优先信工具描述，于是用户在策略里禁掉跨主机，
// 模型却照样往别的主机发命令。
func TestToolDefsForMarksCrossHostRestriction(t *testing.T) {
	for _, cross := range []bool{true, false} {
		for _, d := range toolDefsFor(cross) {
			props, _ := d.Function.Parameters["properties"].(map[string]any)
			if props == nil {
				continue
			}
			h, ok := props["host_id"].(map[string]any)
			if !ok {
				// 没有 host_id 的工具（本地工具）不参与这条检查。
				continue
			}
			desc, _ := h["description"].(string)
			if cross {
				if strings.Contains(desc, hostIDCrossHostSuffix) {
					t.Errorf("允许跨主机时 %q 的 host_id 描述不应含限制说明", d.Function.Name)
				}
			} else {
				if !strings.Contains(desc, hostIDCrossHostSuffix) {
					t.Errorf("禁止跨主机时 %q 的 host_id 描述缺少限制说明", d.Function.Name)
				}
			}
		}
	}
}

// 工具定义的总量必须受控。
//
// 每加一个工具，tokenbudget 里那个固定的 requestOverhead 就不再够用 ——
// 它会持续低估上下文占用，直到用户在真实的 400 面前撞墙。这条测试
// 把「加了工具却忘了调 requestOverhead」变成一个显式失败。
func TestToolDefsOverheadWithinBudget(t *testing.T) {
	const budget = requestOverhead
	total := 0
	for _, d := range toolDefsFor(false) {
		total += EstimateTokens(d.Function.Name) + EstimateTokens(d.Function.Description)
	}
	if total > budget {
		t.Errorf("工具定义的 token 开销 %d 已超过 requestOverhead(%d)。\n"+
			"新增工具会持续低估每一次请求的上下文占用，直到用户撞上模型窗口上限。\n"+
			"请把 internal/agent/tokenbudget.go 里的 requestOverhead 调到 %d 以上。",
			total, budget, total)
	}
}

// 本地工具必须在跨主机闸门之前短路。
//
// 这条不是测实现而是测**标注**：Local 标错了，list_hosts 会被
// 当成远端工具去要 host_id，而它根本没有这个参数。
func TestLocalToolsHaveNoHostID(t *testing.T) {
	for _, tool := range toolRegistry() {
		if !tool.Local {
			continue
		}
		props, _ := tool.Def.Function.Parameters["properties"].(map[string]any)
		if props == nil {
			continue
		}
		if _, ok := props["host_id"]; ok {
			t.Errorf("本地工具 %q 标了 Local，却带 host_id 参数", tool.Name)
		}
	}
}

// 系统提示必须提到每一个非本地工具，否则模型知道有工具却不会用。
func TestSystemPromptMentionsEveryRemoteTool(t *testing.T) {
	v := newTestVault(t)
	sys := systemPrompt(v, true, "")
	for _, tool := range toolRegistry() {
		if tool.Local {
			continue
		}
		if !strings.Contains(sys, tool.Name) {
			t.Errorf("系统提示里没有提到工具 %q —— 模型看不到它的用法说明", tool.Name)
		}
	}
}

// newTestVault 造一个带一台主机的空 vault，供系统提示类测试使用。
func newTestVault(t *testing.T) *vault.Vault {
	t.Helper()
	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开 vault 失败: %v", err)
	}
	if err := v.SaveHost(vault.Host{ID: "h1", Name: "web-01", Addr: "10.0.0.1:22", User: "root"}, &vault.HostSecret{}); err != nil {
		t.Fatalf("保存主机失败: %v", err)
	}
	return v
}

// 默认策略下，写类工具必须给 Confirm，读类工具必须给 Allow。
//
// 这条守住重构后最容易出错的地方：策略评估函数挂错工具上，
// 会让写操作免审批（最危险）或读操作每次弹窗（最难用）。
func TestToolVerdictsMatchReadWriteIntent(t *testing.T) {
	pol := vault.PolicySettings{Mode: vault.ModeWhitelist}
	writeTools := map[string]bool{
		"write_file": true, "upload_local_file": true, "restore_file_backup": true,
	}
	for _, tool := range toolRegistry() {
		if tool.Local {
			continue
		}
		view := &ToolCallView{Name: tool.Name, Command: "/tmp/x"}
		// verify_checks 的策略要从 checks JSON 里取路径，测试里补一份无害清单。
		if tool.Name == "verify_checks" {
			view.Args = `{"checks":[{"type":"port","port":80}]}`
		}
		got := tool.Evaluate(view, pol)
		if writeTools[tool.Name] {
			if got.Decision != policy.Confirm {
				t.Errorf("写类工具 %q 默认应需人工确认，实得 %s", tool.Name, got.Decision)
			}
			continue
		}
		// 读类：普通路径在白名单模式下应自动放行。
		if got.Decision == policy.Deny {
			t.Errorf("读类工具 %q 对普通路径被硬拒绝：%s（%s）", tool.Name, got.Reason, got.Rule)
		}
	}
}
