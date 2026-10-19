package agent

import (
	"context"
	"strings"
	"testing"

	"ai-shell/internal/policy"
)

// withActiveDiagnose 直接注入活动诊断项（绕过「单条 diagnose 琐碎计划」闸门），专供 allows/收尾单测。
func withActiveDiagnose(intent string) runPlan {
	p := newRunPlan(6)
	p.items = []planItem{{ID: "p1", Intent: intent, Class: classDiagnose, Status: planActive}}
	return p
}

func TestPlanAllowsSimpleToolsWithoutPlan(t *testing.T) {
	p := newRunPlan(6)
	wl := []string{"ls", "uptime", "df"}
	for _, c := range []struct{ name, cmd string }{
		{"run_command", "ls"},
		{"run_command", "uptime"},
		{"list_dir", ""},
		{"system_info", ""},
		{"read_file", ""},
	} {
		if ok, why := p.allows(c.name, c.cmd, wl); !ok {
			t.Fatalf("无计划应放行只读 %s %q: %s", c.name, c.cmd, why)
		}
	}
}

func TestPlanAllowsLsWithEmptyUserWhitelist(t *testing.T) {
	// 用户清空策略白名单时，内置 DefaultWhitelist 仍应让 ls 免计划。
	p := newRunPlan(6)
	if ok, why := p.allows("run_command", "ls", nil); !ok {
		t.Fatalf("空白名单时 ls 仍应免计划: %s", why)
	}
	if ok, why := p.allows("run_command", "ls", []string{}); !ok {
		t.Fatalf("空切片白名单时 ls 仍应免计划: %s", why)
	}
}

func TestPlanRequiresActiveBeforeChangeAndBaselineTools(t *testing.T) {
	p := newRunPlan(6)
	for _, name := range []string{"write_file", "restore_file_backup", "upload_local_file", "snapshot_state", "diff_state", "verify_checks"} {
		ok, why := p.allows(name, "", []string{"ls"})
		if ok {
			t.Fatalf("没有活动项时不应放行 %s", name)
		}
		if !strings.Contains(why, "revise_plan") && !strings.Contains(why, "执行计划") {
			t.Fatalf("%s 拒绝原因应提示先提交计划，实得 %q", name, why)
		}
	}
	ok, why := p.allows("run_command", "rm -rf /tmp/x", []string{"ls"})
	if ok {
		t.Fatal("没有活动项时不应放行非只读 run_command")
	}
	if !strings.Contains(why, "revise_plan") && !strings.Contains(why, "执行计划") {
		t.Fatalf("拒绝原因应提示先提交计划，实得 %q", why)
	}
}

func TestPlanRejectsTrivialReadonlyPlan(t *testing.T) {
	p := newRunPlan(6)
	out, ok := p.revise(`{"reason":"用户要 ls","items":[{"id":"p1","intent":"列目录","class":"diagnose","status":"active"}]}`)
	if ok {
		t.Fatalf("仅一条 diagnose 应被拒绝，实得 %s", out)
	}
	if !strings.Contains(out, "单步观测") && !strings.Contains(out, "不必提交") {
		t.Fatalf("拒绝说明应提示直接调工具，实得 %q", out)
	}
	if p.hasOpen() {
		t.Fatal("被拒的琐碎计划不应写入状态")
	}
}

func TestPlanAcceptsMultiStepDiagnose(t *testing.T) {
	p := newRunPlan(6)
	out, ok := p.revise(`{"reason":"多步诊断","items":[
		{"id":"p1","intent":"看负载","class":"diagnose","status":"active"},
		{"id":"p2","intent":"看磁盘","class":"diagnose","status":"pending"}
	]}`)
	if !ok {
		t.Fatalf("至少两条 diagnose 应通过: %s", out)
	}
}

func TestPlanClassGatesTools(t *testing.T) {
	p := withActiveDiagnose("看负载")
	wl := []string{"uptime", "cat"}
	if ok, why := p.allows("run_command", "uptime", wl); !ok {
		t.Fatalf("diagnose 应允许只读白名单命令: %s", why)
	}
	if ok, why := p.allows("write_file", "", wl); ok {
		t.Fatalf("diagnose 不应允许 write_file: %s", why)
	}
	if ok, why := p.allows("run_command", "rm -rf /tmp/x", wl); ok {
		t.Fatalf("diagnose 不应允许高危 run_command: %s", why)
	}
	if ok, why := p.allows("run_command", `python3 -c 'open("/tmp/x","w").write("a")'`, wl); ok {
		t.Fatalf("diagnose 不应放行非白名单命令: %s", why)
	}
}

func TestPlanDoneRequiresToolRan(t *testing.T) {
	p := withActiveDiagnose("看")
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
	p := withActiveDiagnose("看")
	p.noteToolRan()
	if _, ok := p.revise(`{"reason":"完成","items":[{"id":"p1","intent":"看","class":"diagnose","status":"done"}]}`); !ok {
		t.Fatal("标 done 失败")
	}
	out, ok := p.revise(`{"reason":"改写已完成","items":[{"id":"p1","intent":"别的事","class":"diagnose","status":"done"}]}`)
	if ok {
		t.Fatalf("已完成项不应被改写: %s", out)
	}
}

func TestPlanRewriteClearsToolRan(t *testing.T) {
	p := withActiveDiagnose("看")
	p.noteToolRan()
	out, ok := p.revise(`{"reason":"改类","items":[{"id":"p1","intent":"看","class":"change","status":"active"}]}`)
	if !ok {
		t.Fatalf("应允许改写未完成项为 change: %s", out)
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

func TestAgentPlanRejectsChangeToolWithoutPlan(t *testing.T) {
	h := newHarness(t, []string{
		respToolCall("c1", "write_file", `{"host_id":"h-test","path":"/tmp/aishell-plan-test","content":"x"}`),
		respPlanChange("plan1"),
		respToolCall("c2", "write_file", `{"host_id":"h-test","path":"/tmp/aishell-plan-test","content":"x"}`),
		respPlanChangeDone("plan2"),
		respContent("完成"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "写个文件"); err != nil {
		t.Fatal(err)
	}
	if !h.hasEvent(EvPlan) {
		t.Fatal("成功提交计划后应发出 agent:plan")
	}
	foundReject := false
	for i := 0; i < h.fake.requestCount(); i++ {
		for _, m := range h.fake.messagesAt(i) {
			if m.Role == "tool" && (strings.Contains(m.Content, "执行计划") || strings.Contains(m.Content, "revise_plan")) {
				foundReject = true
			}
		}
	}
	if !foundReject {
		t.Fatal("无计划的 write_file 应回喂计划拒绝说明")
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
		if e.Kind == "tool" && e.Rule == "plan" && e.Decision == "deny" && e.Tool == "write_file" {
			found = true
		}
	}
	if !found {
		t.Fatal("计划拒绝应写入审计，规则 plan")
	}
}

func TestAgentSimpleReadonlySkipsPlan(t *testing.T) {
	h := newHarness(t, []string{
		respToolCall("c1", "run_command", `{"host_id":"h-test","command":"uptime"}`),
		respContent("负载正常"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "ls"); err != nil {
		t.Fatal(err)
	}
	if h.hasEvent(EvPlan) {
		t.Fatal("单步只读不应发出执行计划事件")
	}
	if got := h.lastAnswer(); !strings.Contains(got, "负载正常") {
		t.Fatalf("应收尾，实得 %q", got)
	}
}

func TestAgentTrivialPlanRejectedNoUIEvent(t *testing.T) {
	// 模型仍试图为 ls 开单条 diagnose 计划 → 运行时拒绝，不推 EvPlan，随后直接跑命令。
	h := newHarness(t, []string{
		respToolCall("bad", "revise_plan", `{"reason":"用户要 ls","items":[{"id":"p1","intent":"列目录","class":"diagnose","status":"active"}]}`),
		respToolCall("c1", "run_command", `{"host_id":"h-test","command":"ls"}`),
		respContent("目录如下"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "ls"); err != nil {
		t.Fatal(err)
	}
	if h.hasEvent(EvPlan) {
		t.Fatal("被拒的琐碎计划不应发出 agent:plan")
	}
	found := false
	for i := 0; i < h.fake.requestCount(); i++ {
		for _, m := range h.fake.messagesAt(i) {
			if m.Role == "tool" && strings.Contains(m.Content, "单步观测") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("应回喂「单步观测不必提交计划」")
	}
}

func TestSystemPromptPlanIsOptionalForReadonly(t *testing.T) {
	h := newHarness(t, []string{respContent("好的")}, policy.ModeWhitelist, true)
	if err := runDefault(h.ag, context.Background(), h.hostID, "hi"); err != nil {
		t.Fatal(err)
	}
	sys := h.fake.messagesAt(0)[0].Content
	for _, want := range []string{"简单查询直接做", "多步只读可直接连调", "变更/回滚/基线必须先计划"} {
		if !strings.Contains(sys, want) {
			t.Errorf("系统提示缺少 %q", want)
		}
	}
	if strings.Contains(sys, "计划（强制）") {
		t.Error("不应再写「计划（强制）」")
	}
}

func TestAgentInteractiveRmSkippedAndAudited(t *testing.T) {
	h := newHarness(t, []string{
		respPlanChange("plan1"),
		respToolCall("c1", "run_command", `{"host_id":"h-test","command":"rm -i /tmp/x"}`),
		respContent("已改为非交互"),
	}, policy.ModeWhitelist, true)
	if err := runDefault(h.ag, context.Background(), h.hostID, "删掉临时文件"); err != nil {
		t.Fatal(err)
	}
	foundMsg := false
	for i := 0; i < h.fake.requestCount(); i++ {
		for _, m := range h.fake.messagesAt(i) {
			if m.Role == "tool" && strings.Contains(m.Content, "未执行") && strings.Contains(m.Content, "键盘确认") {
				foundMsg = true
			}
		}
	}
	if !foundMsg {
		t.Fatal("rm -i 应立刻退回，不能进入 Exec")
	}
	entries, err := h.auditor.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Rule == "interactive" && e.Decision == "deny" && e.Tool == "run_command" {
			found = true
		}
	}
	if !found {
		t.Fatal("交互命令被拦住时应写入审计，规则 interactive")
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
