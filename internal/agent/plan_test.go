package agent

import (
	"context"
	"strings"
	"testing"

	"ai-shell/internal/policy"
)

func TestPlanRequiresActiveBeforeTools(t *testing.T) {
	p := newRunPlan(6)
	ok, why := p.allows("run_command", "uptime", []string{"uptime"})
	if ok {
		t.Fatal("没有活动项时不应放行工具")
	}
	if !strings.Contains(why, "revise_plan") {
		t.Fatalf("拒绝原因应提示先修订计划，实得 %q", why)
	}
}

func TestPlanClassGatesTools(t *testing.T) {
	p := newRunPlan(6)
	wl := []string{"uptime", "cat"}
	out, ok := p.revise(`{"reason":"诊断","items":[{"id":"p1","intent":"看负载","class":"diagnose","status":"active"}]}`)
	if !ok {
		t.Fatalf("提交计划失败: %s", out)
	}
	if ok, why := p.allows("run_command", "uptime", wl); !ok {
		t.Fatalf("diagnose 应允许只读白名单命令: %s", why)
	}
	if ok, why := p.allows("write_file", "", wl); ok {
		t.Fatalf("diagnose 不应允许 write_file: %s", why)
	}
	if ok, why := p.allows("run_command", "rm -rf /tmp/x", wl); ok {
		t.Fatalf("diagnose 不应允许高危 run_command: %s", why)
	}
	// 高危正则盖不住的写操作：不在只读白名单里就必须换 change。
	if ok, why := p.allows("run_command", `python3 -c 'open("/tmp/x","w").write("a")'`, wl); ok {
		t.Fatalf("diagnose 不应放行非白名单命令: %s", why)
	}
}

func TestPlanDoneRequiresToolRan(t *testing.T) {
	p := newRunPlan(6)
	if _, ok := p.revise(`{"reason":"开始","items":[{"id":"p1","intent":"看","class":"diagnose","status":"active"}]}`); !ok {
		t.Fatal("提交失败")
	}
	out, ok := p.revise(`{"reason":"收尾","items":[{"id":"p1","intent":"看","class":"diagnose","status":"done"}]}`)
	if ok {
		t.Fatalf("未跑过工具不应标 done，实得 %s", out)
	}
	p.noteToolRan()
	out, ok = p.revise(`{"reason":"收尾","items":[{"id":"p1","intent":"看","class":"diagnose","status":"done"}]}`)
	if !ok {
		t.Fatalf("跑过工具后应能标 done: %s", out)
	}
	if p.hasOpen() {
		t.Fatal("全部完成后不应还有未完成项")
	}
}

func TestPlanFrozenItemsCannotChange(t *testing.T) {
	p := newRunPlan(6)
	p.revise(`{"reason":"开始","items":[{"id":"p1","intent":"看","class":"diagnose","status":"active"}]}`)
	p.noteToolRan()
	p.revise(`{"reason":"完成","items":[{"id":"p1","intent":"看","class":"diagnose","status":"done"}]}`)
	out, ok := p.revise(`{"reason":"改写已完成","items":[{"id":"p1","intent":"别的事","class":"diagnose","status":"done"}]}`)
	if ok {
		t.Fatalf("已完成项不应被改写: %s", out)
	}
}

func TestPlanRewriteClearsToolRan(t *testing.T) {
	p := newRunPlan(6)
	p.revise(`{"reason":"开始","items":[{"id":"p1","intent":"看","class":"diagnose","status":"active"}]}`)
	p.noteToolRan()
	out, ok := p.revise(`{"reason":"改类","items":[{"id":"p1","intent":"看","class":"change","status":"active"}]}`)
	if !ok {
		t.Fatalf("应允许改写未完成项: %s", out)
	}
	out, ok = p.revise(`{"reason":"冒充完成","items":[{"id":"p1","intent":"看","class":"change","status":"done"}]}`)
	if ok {
		t.Fatalf("改过类别后不应凭旧的执行记录标 done: %s", out)
	}
}

func TestPlanRewriteOpenItems(t *testing.T) {
	p := newRunPlan(6)
	p.revise(`{"reason":"开始","items":[
		{"id":"p1","intent":"诊断","class":"diagnose","status":"active"},
		{"id":"p2","intent":"变更","class":"change","status":"pending"}
	]}`)
	out, ok := p.revise(`{"reason":"证据表明要先回滚","items":[
		{"id":"p1","intent":"诊断","class":"diagnose","status":"dropped","reason":"方向错了"},
		{"id":"p2","intent":"回滚配置","class":"rollback","status":"active"}
	]}`)
	if !ok {
		t.Fatalf("应允许改写未完成项: %s", out)
	}
	if act := p.active(); act == nil || act.Class != classRollback {
		t.Fatalf("活动项应变为 rollback，实得 %#v", act)
	}
}

func TestAgentPlanRejectsToolWithoutPlan(t *testing.T) {
	h := newHarness(t, []string{
		respToolCall("c1", "run_command", `{"host_id":"h-test","command":"uptime"}`),
		respPlanDiagnose("plan1"),
		respToolCall("c2", "run_command", `{"host_id":"h-test","command":"uptime"}`),
		respPlanDone("plan2"),
		respContent("完成"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "看负载"); err != nil {
		t.Fatal(err)
	}
	if !h.hasEvent(EvPlan) {
		t.Fatal("成功提交计划后应发出 agent:plan")
	}
	if msg := firstToolContent(h); !strings.Contains(msg, "计划拒绝") && !strings.Contains(msg, "没有活动计划项") {
		// 第一次 run_command 的拒绝会出现在后续某次请求的 tool 消息里；
		// 也可能被后一次成功的 uptime 结果盖过 firstToolContent —— 所以再扫一遍。
		found := false
		for i := 0; i < h.fake.requestCount(); i++ {
			for _, m := range h.fake.messagesAt(i) {
				if m.Role == "tool" && strings.Contains(m.Content, "活动计划项") {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("应回喂计划拒绝说明，首条工具结果: %q", msg)
		}
	}
	if got := h.lastAnswer(); !strings.Contains(got, "完成") {
		t.Fatalf("应能在补交计划后收尾，实得 %q", got)
	}
	entries, err := h.auditor.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "tool" && e.Rule == "plan" && e.Decision == "deny" && e.Tool == "run_command" {
			found = true
			if e.Command != "uptime" {
				t.Fatalf("计划拒绝应记下被拦的命令，实得 %q", e.Command)
			}
		}
	}
	if !found {
		t.Fatal("计划拒绝应写入审计，规则 plan")
	}
}

func TestAgentOpenPlanNoteIsNotRemembered(t *testing.T) {
	h := newHarness(t, []string{
		respPlanDiagnose("plan1"),
		respContent("先这样吧"),
		respPlanDrop("plan2"),
		respContent("最终结论"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "看一下"); err != nil {
		t.Fatal(err)
	}
	if got := h.lastAnswer(); !strings.Contains(got, "最终结论") {
		t.Fatalf("应收尾为最终结论，实得 %q", got)
	}
	for _, m := range histDefault(h.ag, h.hostID) {
		if strings.Contains(m.Content, "计划未收口") {
			t.Fatalf("未收口提示不应进入会话历史: %s", m.Content)
		}
	}
}
