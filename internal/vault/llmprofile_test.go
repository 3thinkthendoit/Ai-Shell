package vault

import (
	"testing"
)

// 本文件覆盖 LLM 多方案（profile）：新增/编辑、切换、删除，以及
// 老配置（单套 LLM 字段）到多方案的一次性迁移。
//
// 关键不变式：
//   - 永远至少有一套可用配置（读路径只认「激活方案」）；
//   - 迁移幂等，重复 Open 不产生重复方案；
//   - 密钥只出现在 LLMAPIKeys，视图（View）里绝不带 Key。

// TestFreshInstallMigratesToFirstProfile 锁定「全新安装也有第一套方案」：
// defaultStore 预置的是旧字段（llm / llmApiKey），由 Open() 的迁移统一
// 转成方案 —— 这是所有读路径（LLM / LLMSettingsView / LLMProfiles）
// 永远拿得到可用配置的前提。
func TestFreshInstallMigratesToFirstProfile(t *testing.T) {
	v := newTestVault(t)

	ps := v.LLMProfiles()
	if len(ps) != 1 {
		t.Fatalf("全新安装应经迁移得到一套方案，实得 %d 套", len(ps))
	}
	if !ps[0].Active {
		t.Fatal("预置方案应自动激活")
	}
	if ps[0].BaseURL == "" || ps[0].Model == "" {
		t.Fatalf("预置方案应带默认 Base URL 与模型名，实得 %+v", ps[0])
	}
	if ps[0].HasAPIKey {
		t.Fatal("预置方案不应有 API Key")
	}

	// LLM() 必须从激活方案取值 —— 这是 Agent 实际使用的出口。
	saved, key := v.LLM()
	if saved.BaseURL != ps[0].BaseURL || saved.Model != ps[0].Model || key != "" {
		t.Fatalf("LLM() 应返回激活方案，实得 %+v key=%q", saved, key)
	}
}

func TestSaveAndActivateProfile(t *testing.T) {
	v := newTestVault(t)

	key := "sk-second"
	if err := v.SaveLLMProfile(LLMProfile{
		ID: "p2", Name: " DeepSeek ", BaseURL: "https://api.deepseek.com/v1///", Model: "deepseek-chat",
	}, &key); err != nil {
		t.Fatal(err)
	}

	// 新增的第二套不应自动激活（第一套仍然在使用）。
	if got := v.activeProfileIDForTest(t); got != defaultLLMProfileID {
		t.Fatalf("新增方案不应自动激活，激活 ID 实得 %q", got)
	}

	// 名称去空格、Base URL 去尾部斜杠，都在保存时完成。
	p, ok := v.LLMProfile("p2")
	if !ok {
		t.Fatal("保存后的方案应能按 ID 取回")
	}
	if p.Name != "DeepSeek" || p.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("保存时未规范化：%+v", p)
	}
	if v.LLMKey("p2") != key {
		t.Fatal("API Key 未随方案保存")
	}

	// 视图里只应有 HasAPIKey 布尔，结构体本身就不存在 Key 字段（类型即防线）。
	for _, view := range v.LLMProfiles() {
		if view.ID == "p2" && !view.HasAPIKey {
			t.Fatal("p2 已配置 Key，视图应报告 HasAPIKey")
		}
	}

	if err := v.ActivateLLMProfile("p2"); err != nil {
		t.Fatal(err)
	}
	saved, gotKey := v.LLM()
	if saved.BaseURL != "https://api.deepseek.com/v1" || gotKey != key {
		t.Fatalf("切换后 LLM() 应返回新方案，实得 %+v key=%q", saved, gotKey)
	}
	view := v.LLMSettingsView()
	if !view.HasAPIKey {
		t.Fatal("视图应报告已配置 Key")
	}
}

func TestSaveProfileKeepsEmptyFieldsOnEdit(t *testing.T) {
	v := newTestVault(t)

	if err := v.SaveLLMProfile(LLMProfile{ID: "p1", Name: "a", BaseURL: "https://x/v1", Model: "m1"}, nil); err != nil {
		t.Fatal(err)
	}
	// 编辑时 Base URL / 模型名留空 = 保留原值（与 SetLLM 的语义一致）。
	if err := v.SaveLLMProfile(LLMProfile{ID: "p1", Name: "a2"}, nil); err != nil {
		t.Fatal(err)
	}
	p, _ := v.LLMProfile("p1")
	if p.BaseURL != "https://x/v1" || p.Model != "m1" || p.Name != "a2" {
		t.Fatalf("编辑留空的字段应保留原值，实得 %+v", p)
	}

	// 校验：名称与 ID 必填。
	if err := v.SaveLLMProfile(LLMProfile{ID: "", Name: "x"}, nil); err == nil {
		t.Fatal("空 ID 应被拒绝")
	}
	if err := v.SaveLLMProfile(LLMProfile{ID: "p9", Name: "  "}, nil); err == nil {
		t.Fatal("空名称应被拒绝")
	}
}

func TestDeleteProfileMaintainsActive(t *testing.T) {
	v := newTestVault(t)
	for _, p := range []LLMProfile{
		{ID: "p2", Name: "b", BaseURL: "https://b/v1", Model: "m"},
		{ID: "p3", Name: "c", BaseURL: "https://c/v1", Model: "m"},
	} {
		if err := v.SaveLLMProfile(p, nil); err != nil {
			t.Fatal(err)
		}
	}

	// 至少保留一套：只剩一套时删除必须被拒绝；删掉激活方案后
	// 应自动切到剩下的第一套。
	if err := v.ActivateLLMProfile("p2"); err != nil {
		t.Fatal(err)
	}
	if err := v.DeleteLLMProfile("nonexistent"); err == nil {
		t.Fatal("不存在的方案应被拒绝")
	}
	_ = v.DeleteLLMProfile("p3")
	if err := v.DeleteLLMProfile("p2"); err != nil {
		t.Fatal(err)
	}
	ps := v.LLMProfiles()
	if len(ps) != 1 {
		t.Fatalf("删到只剩一套，实得 %d 套", len(ps))
	}
	if !ps[0].Active {
		t.Fatal("删除激活方案后应自动切到剩下那套")
	}

	// 最后一套删不掉。
	if err := v.DeleteLLMProfile(defaultLLMProfileID); err == nil {
		t.Fatal("不应允许删掉最后一套方案")
	}
}

func TestLegacyLLMMigrationIsIdempotent(t *testing.T) {
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()

	// 先建库拿密钥，再手工写一份「老版本」store：只有 llm / llmApiKey 字段。
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	legacy := `{
		"version": 1,
		"hosts": [], "secrets": {}, "hostKeys": {},
		"llm": {"baseUrl": "https://api.deepseek.com/v1", "model": "deepseek-chat"},
		"llmApiKey": "sk-legacy-key-0123456789",
		"whitelist": ["ls"],
		"policy": {"mode": "manual", "whitelist": ["ls"], "redactOutput": true, "maxOutput": 32768, "maxSteps": 12}
	}`
	blob, err := encrypt(v.key, []byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(v.path(vaultFileName), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	// 第一次 Open：迁移应生成一套方案并搬走 Key。
	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	ps := v2.LLMProfiles()
	if len(ps) != 1 {
		t.Fatalf("老配置应迁移出一套方案，实得 %d 套", len(ps))
	}
	if ps[0].BaseURL != "https://api.deepseek.com/v1" || ps[0].Model != "deepseek-chat" {
		t.Fatalf("迁移丢失了配置：%+v", ps[0])
	}
	if !ps[0].HasAPIKey {
		t.Fatal("迁移应把旧 llmApiKey 搬进方案")
	}
	if got := v2.LLMKey(ps[0].ID); got != "sk-legacy-key-0123456789" {
		t.Fatalf("迁移后的 Key 不对：%q", got)
	}

	// 第二次 Open：迁移必须幂等，不能又长出一套。
	v3 := New(dir)
	if err := v3.Open(); err != nil {
		t.Fatal(err)
	}
	if n := len(v3.LLMProfiles()); n != 1 {
		t.Fatalf("重复 Open 不应产生重复方案，实得 %d 套", n)
	}

	// 旧 Key 也必须参与脱敏比对。
	found := false
	for _, s := range v3.SecretStrings() {
		if s == "sk-legacy-key-0123456789" {
			found = true
		}
	}
	if !found {
		t.Fatal("迁移后的 LLM Key 应参与 SecretStrings 脱敏比对")
	}
}

func (v *Vault) activeProfileIDForTest(t *testing.T) string {
	t.Helper()
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.activeProfileIDLocked()
}
