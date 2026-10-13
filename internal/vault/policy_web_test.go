package vault

// 出网相关策略字段的持久化与默认值测试。

import (
	"testing"
)

// 老配置（落盘的 store 里没有这三个字段）反序列化后必须默认关闭。
//
// 这是安全默认值：出网工具让 LLM 能把内容送去公网，而一台被入侵的
// 主机可以借提示注入诱导它这么做。零值即禁止，所以老配置天然安全 ——
// 不需要任何迁移代码。
//
// 做法是**先存一份新配置、再手工改掉落盘文件**（而不是直接写一段
// 明文 JSON）：vault.enc 是加密后的 blob，写明文进去只会得到
// 「解密失败」，测的就不是默认值了。
func TestWebAccessDefaultsOffForOldConfig(t *testing.T) {
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	// 存一份**不带**出网字段的旧形状配置。
	if err := v.SetPolicy(PolicySettings{Mode: ModeManual, Whitelist: DefaultWhitelist()}); err != nil {
		t.Fatal(err)
	}

	// 直接改内存里的 store 再落盘，模拟「旧版本写下的文件」。
	v.mu.Lock()
	v.st.Policy.AllowWeb = false
	v.st.Policy.SearchProvider = ""
	v.st.Policy.SearchAPIKey = ""
	err := v.saveLocked()
	v.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	p := v2.Policy()
	if p.AllowWeb {
		t.Error("老配置的 AllowWeb 应为 false（零值即禁止）")
	}
	if p.SearchProvider != "" {
		t.Errorf("老配置的 SearchProvider 应为空，实得 %q", p.SearchProvider)
	}
}

// 三个字段必须能完整落盘并读回。
func TestWebSettingsPersist(t *testing.T) {
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}

	want := PolicySettings{
		Mode:           ModeWhitelist,
		Whitelist:      DefaultWhitelist(),
		RedactOutput:   true,
		MaxOutput:      32 * 1024,
		MaxSteps:       12,
		AllowWeb:       true,
		SearchProvider: "duckduckgo",
	}
	if err := v.SetPolicy(want); err != nil {
		t.Fatal(err)
	}

	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	got := v2.Policy()
	if !got.AllowWeb {
		t.Error("AllowWeb 应落盘为 true")
	}
	if got.SearchProvider != "duckduckgo" {
		t.Errorf("SearchProvider 应落盘，实得 %q", got.SearchProvider)
	}
}

// SetPolicy 是**整份替换**而不是按字段合并。
//
// 这条守住的是「调用方必须发全量」这个前提：前端 PolicyPanel 的 save()
// 发的是整份 payload，所以缺字段的调用（比如新后端接到旧前端的请求）
// 会把会话上限写成 0，再被 Policy() 补成默认值 —— 现象是
// 「用户设的 20 轮突然变回 8 轮」，而不是报错。
//
// 写成测试是因为这个语义将来很可能被误改：有人看到「只改一个开关却把
// 别的设置清了」会想把它改成合并，而那会让「用户清空白名单」变成
// 「改不动白名单」。所以这里把它钉住。
func TestSetPolicyReplacesWholeObject(t *testing.T) {
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}

	if err := v.SetPolicy(PolicySettings{
		Mode:               ModeManual,
		Whitelist:          DefaultWhitelist(),
		MaxSessionTurns:    20,
		MaxStoredToolBytes: 16 * 1024,
		MaxSessionBytes:    512 * 1024,
	}); err != nil {
		t.Fatal(err)
	}

	// 第二次带上新增字段，也显式带上上面那三项。
	if err := v.SetPolicy(PolicySettings{
		Mode:               ModeManual,
		Whitelist:          DefaultWhitelist(),
		AllowWeb:           true,
		SearchProvider:     "duckduckgo",
		MaxSessionTurns:    20,
		MaxStoredToolBytes: 16 * 1024,
		MaxSessionBytes:    512 * 1024,
	}); err != nil {
		t.Fatal(err)
	}

	got := v.Policy()
	if !got.AllowWeb || got.SearchProvider != "duckduckgo" {
		t.Errorf("新增字段应生效，实得 AllowWeb=%v provider=%q", got.AllowWeb, got.SearchProvider)
	}
	// 同一份 payload 里显式带上的字段必须保住 —— 这条确认「替换」
	// 丢的只是没带的字段，而不是把带上的一起弄坏。
	if got.MaxSessionTurns != 20 {
		t.Errorf("MaxSessionTurns 应保留 20，实得 %d", got.MaxSessionTurns)
	}
	if got.MaxStoredToolBytes != 16*1024 {
		t.Errorf("MaxStoredToolBytes 应保留 16KiB，实得 %d", got.MaxStoredToolBytes)
	}
}

// 没带上的字段会被替换成零值，再由 Policy() 补默认值。
//
// 与上一条是同一件事的两面：那条证明「带上的不丢」，这条证明
// 「没带的确实会丢」。两面都钉住，才不会被将来某次「顺手改成合并」
// 悄悄破坏「用户能清空白名单」这个既有行为。
func TestSetPolicyDropsFieldsNotSent(t *testing.T) {
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}

	// 自定义白名单：只有一条，与默认值明显不同。
	custom := []string{"ls", "systemctl status nginx"}
	if err := v.SetPolicy(PolicySettings{Mode: ModeManual, Whitelist: custom}); err != nil {
		t.Fatal(err)
	}
	if got := v.Policy(); len(got.Whitelist) != len(custom) {
		t.Fatalf("夹具坏了：自定义白名单应生效，实得 %d 条", len(got.Whitelist))
	}

	// 这次不带白名单。
	if err := v.SetPolicy(PolicySettings{Mode: ModeManual, AllowWeb: true}); err != nil {
		t.Fatal(err)
	}

	// 落回默认白名单（Policy() 对空名单的兜底），而不是保留自定义那两条。
	got := v.Policy()
	if len(got.Whitelist) <= len(custom) {
		t.Errorf("白名单应被替换掉（落回默认 %d 条），实得 %d 条: %v",
			len(DefaultWhitelist()), len(got.Whitelist), got.Whitelist)
	}
}
