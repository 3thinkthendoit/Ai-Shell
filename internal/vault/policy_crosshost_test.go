package vault

import "testing"

// AllowCrossHost 是安全开关：默认必须为禁止，且要能落盘、重开、读回。
// 若默认值哪天翻转（或落盘丢失），这条用例会在合并前报警。
func TestAllowCrossHostRoundTripsAndDefaultsToDeny(t *testing.T) {
	v := newTestVault(t)

	// 零值即禁止 —— 老配置反序列化后天然安全。
	if got := v.Policy().AllowCrossHost; got {
		t.Fatal("AllowCrossHost 默认必须是 false（禁止跨主机）")
	}

	// 开启 → 落盘 → 重新打开 → 读回仍是开启。
	p := v.Policy()
	p.AllowCrossHost = true
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}

	v2 := New(v.Dir())
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	if !v2.Policy().AllowCrossHost {
		t.Fatal("开启跨主机后重新打开凭证库，开关应保持为 true")
	}

	// 再关掉 → 重开 → 仍是关闭（双向都要成立，防止「只能开不能关」）。
	p.AllowCrossHost = false
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	v3 := New(v.Dir())
	if err := v3.Open(); err != nil {
		t.Fatal(err)
	}
	if v3.Policy().AllowCrossHost {
		t.Fatal("关闭跨主机后重新打开凭证库，开关应保持为 false")
	}
}
