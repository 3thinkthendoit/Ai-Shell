package main

import (
	"strings"
	"testing"

	"ai-shell/internal/audit"
	"ai-shell/internal/vault"
)

// 本文件锁住 SavePolicy 的审计记录。
//
// 为什么要专门测这个：策略变更里包含「模型能记住多少对话」这类
// 影响数据外发体积的设置。事后要能回答「当时是什么值」，
// 所以审计里必须记全，且必须记**实际生效**的那一份。
//
// 真实陷阱：入参里传 0 表示「未设置」，SetPolicy 会回填成默认值。
// 如果拿入参去写审计，日志会记成 0，而实际跑的是 8 ——
// 事后翻日志的人会得出完全相反的结论。

// newPolicyTestApp 装配一个带审计日志的 App。
func newPolicyTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")

	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开临时凭证库失败: %v", err)
	}
	au, err := audit.New(v.Dir(), v)
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	return &App{v: v, audit: au}
}

// lastPolicyNote 取最近一条策略审计记录的 Note。
func lastPolicyNote(t *testing.T, a *App) string {
	t.Helper()
	view := a.ListAudit(50)
	if view.Error != "" {
		t.Fatalf("读取审计日志失败: %s", view.Error)
	}
	for i := len(view.Entries) - 1; i >= 0; i-- {
		if view.Entries[i].Kind == audit.KindPolicy {
			return view.Entries[i].Note
		}
	}
	t.Fatal("审计日志里没有策略变更记录")
	return ""
}

// 审计记录里必须出现会话上下文的三个上限 —— 只记白名单和步数是不够的，
// 「模型记得多少」直接决定有多少远端数据会被发给模型。
func TestSavePolicyAuditsSessionLimits(t *testing.T) {
	a := newPolicyTestApp(t)

	p := a.v.Policy()
	p.Whitelist = []string{"ls"}
	p.MaxSessionTurns = 20
	p.MaxStoredToolBytes = 64 * 1024
	p.MaxSessionBytes = 1 << 20
	if err := a.SavePolicy(p); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}

	note := lastPolicyNote(t, a)
	// 20 轮 / 64KiB 单条 / 1024KiB 总量
	for _, want := range []string{"20", "64KiB", "1024KiB"} {
		if !strings.Contains(note, want) {
			t.Errorf("审计记录应包含 %q，实得: %s", want, note)
		}
	}
}

// 审计里要记**实际生效**的覆盖台数，而不是入参里的条目数。
//
// 入参里可能存在三项全空的条目（界面把某台主机那一行清空后，
// 或旧前端把整表原样回传）—— 它们会被 SetPolicy 规整掉。
// 记入参的话日志会显示「2 台主机有独立上限」而实际生效 1 台，
// 事后按日志排查「这台机器的记性为什么和别人不一样」会被带偏。
func TestSavePolicyAuditsEffectiveOverrideCount(t *testing.T) {
	a := newPolicyTestApp(t)

	p := a.v.Policy()
	p.SessionOverrides = map[string]vault.SessionLimits{
		"h-real":  {MaxSessionTurns: 30},
		"h-empty": {}, // 三项全空，会被规整掉
	}
	if err := a.SavePolicy(p); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}

	note := lastPolicyNote(t, a)
	if !strings.Contains(note, "1 台主机有独立上限") {
		t.Errorf("审计应记实际生效的覆盖台数 1，实得: %s", note)
	}
}

// 入参里的非正值会被回填成默认值，审计必须记**回填后**的值。
//
// 若记成入参的 0，事后翻日志的人会以为「当时上下文是关着的」，
// 而实际上模型一直在带历史 —— 日志与事实相反比没有日志更危险。
func TestSavePolicyAuditsEffectiveNotRequestedValues(t *testing.T) {
	a := newPolicyTestApp(t)

	// 三个会话上限全传 0（模拟前端清空输入框 / 老前端不传该字段）。
	p := vault.PolicySettings{
		Mode:               vault.ModeManual,
		Whitelist:          []string{"ls"},
		RedactOutput:       true,
		MaxOutput:          32 * 1024,
		MaxSteps:           12,
		MaxSessionTurns:    0,
		MaxStoredToolBytes: 0,
		MaxSessionBytes:    0,
	}
	if err := a.SavePolicy(p); err != nil {
		t.Fatalf("保存策略失败: %v", err)
	}

	note := lastPolicyNote(t, a)
	// 实际生效的是默认值：8 轮 / 8KiB / 256KiB
	if !strings.Contains(note, "8 轮") {
		t.Errorf("审计应记回填后的轮数 8，实得: %s", note)
	}
	if !strings.Contains(note, "8KiB") {
		t.Errorf("审计应记回填后的单条上限 8KiB，实得: %s", note)
	}
	if !strings.Contains(note, "256KiB") {
		t.Errorf("审计应记回填后的总量 256KiB，实得: %s", note)
	}
	// 不能出现「0 轮」这种与实际相反的记录。
	if strings.Contains(note, "0 轮") {
		t.Errorf("审计不该记入参的 0（与实际生效值相反）: %s", note)
	}

	// 顺带确认落库的也确实是回填后的值。
	got := a.v.Policy()
	if got.MaxSessionTurns != 8 || got.MaxStoredToolBytes != 8192 || got.MaxSessionBytes != 262144 {
		t.Fatalf("实际生效值应为默认值，实得 %d / %d / %d",
			got.MaxSessionTurns, got.MaxStoredToolBytes, got.MaxSessionBytes)
	}
}
