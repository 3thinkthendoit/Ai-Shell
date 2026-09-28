package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestVault 在临时目录创建一个走密钥文件模式的凭证库，避免污染真实钥匙串。
func newTestVault(t *testing.T) *Vault {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	v := New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}
	return v
}

func TestEncryptedAtRest(t *testing.T) {
	const password = "S3cr3t-P@ssw0rd-xyz"
	const privKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAB3NzaC1yc2E\n-----END OPENSSH PRIVATE KEY-----"

	v := newTestVault(t)
	err := v.SaveHost(Host{
		ID: "h1", Name: "prod", Addr: "10.0.0.1:22", User: "root", AuthMethod: AuthPassword,
	}, &HostSecret{Password: password, PrivateKey: privKey})
	if err != nil {
		t.Fatalf("保存主机失败: %v", err)
	}

	// 核心断言：磁盘上的密文里不得出现任何明文密钥
	raw, err := os.ReadFile(filepath.Join(v.dir, vaultFileName))
	if err != nil {
		t.Fatalf("读取凭证库文件失败: %v", err)
	}
	for _, secret := range []string{password, "AAAAB3NzaC1yc2E", "PRIVATE KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("密文中出现了明文密钥片段: %q", secret)
		}
	}

	// 重新打开应能解出同样的密钥
	v2 := New(v.dir)
	if err := v2.Open(); err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	sec, ok := v2.Secret("h1")
	if !ok || sec.Password != password || sec.PrivateKey != privKey {
		t.Fatalf("解密结果不一致: %+v", sec)
	}
}

func TestHostViewNeverCarriesSecrets(t *testing.T) {
	v := newTestVault(t)
	_ = v.SaveHost(Host{
		ID: "h1", Name: "prod", Addr: "10.0.0.1:22", User: "root", AuthMethod: AuthPassword,
	}, &HostSecret{Password: "topsecret12345"})

	// Host 视图序列化后绝不能含密钥字段（注意 authMethod 的值就是 "password"，故须匹配 JSON 键名）
	hosts := v.ListHosts()
	blob, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "topsecret12345") {
		t.Fatalf("Host 视图泄露了密钥值: %s", string(blob))
	}
	for _, key := range []string{`"password":`, `"privateKey":`, `"passphrase":`} {
		if strings.Contains(string(blob), key) {
			t.Fatalf("Host 视图泄露了密钥字段 %s: %s", key, string(blob))
		}
	}
	if !hosts[0].HasSecret {
		t.Fatal("HasSecret 应为 true")
	}
}

func TestSecretStringsIncludesStoredSecrets(t *testing.T) {
	v := newTestVault(t)
	_ = v.SaveHost(Host{ID: "h1", Name: "a", Addr: "1.1.1.1:22", User: "u", AuthMethod: AuthPassword},
		&HostSecret{Password: "another-secret-value"})
	_ = v.SetLLM("https://api.example.com/v1", "m", strPtr("sk-llm-key-abcdefghij"))

	got := v.SecretStrings()
	joined := strings.Join(got, "\n")
	for _, want := range []string{"another-secret-value", "sk-llm-key-abcdefghij"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("SecretStrings 缺少 %q，实际: %v", want, got)
		}
	}
}

func TestSaveHostKeepsSecretWhenNil(t *testing.T) {
	v := newTestVault(t)
	_ = v.SaveHost(Host{ID: "h1", Name: "a", Addr: "1.1.1.1:22", User: "u", AuthMethod: AuthPassword},
		&HostSecret{Password: "keepme"})

	// sec 为 nil 表示只改元数据
	_ = v.SaveHost(Host{ID: "h1", Name: "renamed", Addr: "1.1.1.1:22", User: "u", AuthMethod: AuthPassword}, nil)

	sec, _ := v.Secret("h1")
	if sec.Password != "keepme" {
		t.Fatalf("密钥不应被覆盖，实际: %q", sec.Password)
	}
	h, _ := v.GetHost("h1")
	if h.Name != "renamed" || !h.HasSecret {
		t.Fatalf("元数据更新失败: %+v", h)
	}
}

func TestDeleteHostRemovesSecret(t *testing.T) {
	v := newTestVault(t)
	_ = v.SaveHost(Host{ID: "h1", Name: "a", Addr: "1.1.1.1:22", User: "u", AuthMethod: AuthPassword},
		&HostSecret{Password: "bye"})
	if err := v.DeleteHost("h1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Secret("h1"); ok {
		t.Fatal("删除主机后密钥材料仍然存在")
	}
	if len(v.ListHosts()) != 0 {
		t.Fatal("删除主机后列表不为空")
	}
}

func TestPolicyDefaults(t *testing.T) {
	v := newTestVault(t)
	p := v.Policy()
	if p.Mode != ModeManual {
		t.Fatalf("默认应为手动模式，实际 %s", p.Mode)
	}
	if !p.RedactOutput {
		t.Fatal("默认应开启输出脱敏")
	}
	if len(p.Whitelist) == 0 {
		t.Fatal("默认白名单不应为空")
	}
	if p.MaxSessionTurns != defaultMaxSessionTurns ||
		p.MaxStoredToolBytes != defaultMaxStoredToolBytes ||
		p.MaxSessionBytes != defaultMaxSessionBytes {
		t.Fatalf("会话上下文上限应有默认值，实得 %d / %d / %d",
			p.MaxSessionTurns, p.MaxStoredToolBytes, p.MaxSessionBytes)
	}
}

// 会话上下文上限必须能存下来再读回来 —— 否则用户改了设置，
// 重启应用后悄悄退回默认值，而界面上显示的还是改过的数字。
func TestSessionLimitsPersist(t *testing.T) {
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	p := v.Policy()
	p.MaxSessionTurns = 20
	p.MaxStoredToolBytes = 32 << 10
	p.MaxSessionBytes = 1 << 20
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}

	// 重新打开（模拟重启应用），读到必须是刚才存的那组值。
	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	got := v2.Policy()
	if got.MaxSessionTurns != 20 || got.MaxStoredToolBytes != 32<<10 || got.MaxSessionBytes != 1<<20 {
		t.Fatalf("会话上限未持久化，实得 %d / %d / %d",
			got.MaxSessionTurns, got.MaxStoredToolBytes, got.MaxSessionBytes)
	}
}

// 早期的配置文件里没有这三个字段，反序列化后是 0。
// 必须补成默认值 —— 否则升级后上下文功能会静默失效（表现为「0 轮」，
// 即模型每轮都失忆），而用户什么都没改过，根本不会怀疑到设置上。
//
// 关键：必须**真的造一份老磁盘文件**来测，而不是调 SetPolicy 传零值。
// SetPolicy 自己就会回填，走它等于绕过了「读时回填」这条路径 ——
// 那样即使把回填删掉，用例照样是绿的（本用例初版就踩了这个坑，
// 靠变异测试才发现）。
//
// 另一条实测结论：这条路径目前有**两层独立防护**，只拆一层测不出来 ——
//  1. Open() 从 defaultStore() 起步再 unmarshal，缺失字段保留默认值；
//  2. Policy() 里的 applySessionDefaults 再兜一次底。
//
// 实测：只去掉 1（改成 store{} 起步）用例仍绿（被 2 兜住）；
// 只去掉 2 也仍绿（被 1 兜住）；两层同时去掉才变红并打印 0 / 0 / 0。
// 这正是分层防御该有的样子，所以用例只在两层都失效时才报警。
func TestSessionLimitsBackfilledForLegacyConfig(t *testing.T) {
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()

	// 第一步：先正常建库，拿到 key 与磁盘格式。
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}

	// 第二步：手工写一份「老版本」store —— 三个会话字段在 JSON 里根本不存在。
	// 这是升级场景的真实形态：老代码没有这些字段，落盘的 JSON 里也就没有它们。
	legacy := `{
		"version": 1,
		"hosts": [],
		"secrets": {},
		"hostKeys": {},
		"llm": {"baseUrl": "https://api.openai.com/v1", "model": "gpt-4o-mini"},
		"llmApiKey": "",
		"whitelist": ["ls"],
		"policy": {
			"mode": "manual",
			"whitelist": ["ls"],
			"redactOutput": true,
			"maxOutput": 32768,
			"maxSteps": 12
		}
	}`
	blob, err := encrypt(v.key, []byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(v.path(vaultFileName), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	// 第三步：重新打开这份老配置，读到的会话上限必须是默认值而不是 0。
	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	got := v2.Policy()
	if got.MaxSessionTurns <= 0 || got.MaxStoredToolBytes <= 0 || got.MaxSessionBytes <= 0 {
		t.Fatalf("老配置里的零值必须被补成默认值，实得 %d / %d / %d",
			got.MaxSessionTurns, got.MaxStoredToolBytes, got.MaxSessionBytes)
	}
	if got.MaxSessionTurns != defaultMaxSessionTurns {
		t.Fatalf("补出来的轮数上限应是默认值 %d，实得 %d", defaultMaxSessionTurns, got.MaxSessionTurns)
	}
	// 顺带确认老配置的其他内容没被读丢。
	if got.Mode != ModeManual || got.MaxSteps != 12 {
		t.Fatalf("老配置的其他字段应原样读出，实得 mode=%s maxSteps=%d", got.Mode, got.MaxSteps)
	}
}

// 负数与零同等对待：都不该把上下文窗口意外关掉。
func TestSessionLimitsRejectNonPositive(t *testing.T) {
	v := newTestVault(t)
	if err := v.SetPolicy(PolicySettings{
		Mode:               ModeManual,
		MaxOutput:          32 * 1024,
		MaxSteps:           12,
		MaxSessionTurns:    -5,
		MaxStoredToolBytes: -1,
		MaxSessionBytes:    0,
	}); err != nil {
		t.Fatal(err)
	}
	got := v.Policy()
	if got.MaxSessionTurns <= 0 || got.MaxStoredToolBytes <= 0 || got.MaxSessionBytes <= 0 {
		t.Fatalf("负值/零值必须被补成默认值，实得 %d / %d / %d",
			got.MaxSessionTurns, got.MaxStoredToolBytes, got.MaxSessionBytes)
	}
}

// ---- 主机级会话上限覆盖 ----

// 覆盖表必须能存下来再读回来。
func TestSessionOverridesPersist(t *testing.T) {
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()
	v := New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}

	p := v.Policy()
	p.SessionOverrides = map[string]SessionLimits{
		"host-a": {MaxSessionTurns: 30},
		"host-b": {MaxStoredToolBytes: 64 << 10, MaxSessionBytes: 2 << 20},
	}
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}

	v2 := New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	got := v2.Policy().SessionOverrides
	if len(got) != 2 {
		t.Fatalf("应有 2 条覆盖，实得 %d 条: %+v", len(got), got)
	}
	if got["host-a"].MaxSessionTurns != 30 {
		t.Errorf("host-a 的轮数覆盖应为 30，实得 %d", got["host-a"].MaxSessionTurns)
	}
	// 未被覆盖的项必须原样留 0（= 继承），不能被回填成默认值。
	if got["host-a"].MaxStoredToolBytes != 0 || got["host-a"].MaxSessionBytes != 0 {
		t.Errorf("host-a 未设的两项应保持 0（继承），实得 %+v", got["host-a"])
	}
	if got["host-b"].MaxStoredToolBytes != 64<<10 || got["host-b"].MaxSessionBytes != 2<<20 {
		t.Errorf("host-b 的覆盖未完整读出，实得 %+v", got["host-b"])
	}
}

// 覆盖项里**不能被回填默认值** —— 那会把「继承」这个语义弄丢。
//
// 这是本功能最容易写错的一处：applySessionDefaults 天然会对三个字段
// 逐个补默认值，顺手对覆盖项也跑一遍的话，每个字段都变成非零，
// 于是「只调了轮数」的主机被悄悄钉死在当时的单条工具输出上限上，
// 之后改全局它再也不跟 —— 而界面上它只显示了一个轮数。
func TestSessionOverrideFieldsAreNotBackfilled(t *testing.T) {
	v := newTestVault(t)

	if err := v.SetPolicy(PolicySettings{
		Mode:      ModeManual,
		MaxOutput: 32 * 1024,
		MaxSteps:  12,
		// 全局三项全部留 0（会被回填成默认值）。
		SessionOverrides: map[string]SessionLimits{
			"host-a": {MaxSessionTurns: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got := v.Policy()
	if got.MaxSessionTurns != defaultMaxSessionTurns {
		t.Fatalf("全局轮数应被回填成默认值，实得 %d", got.MaxSessionTurns)
	}
	ov := got.SessionOverrides["host-a"]
	if ov.MaxSessionTurns != 30 {
		t.Errorf("覆盖的轮数应为 30，实得 %d", ov.MaxSessionTurns)
	}
	if ov.MaxStoredToolBytes != 0 {
		t.Errorf("未被覆盖的单条上限必须保持 0（继承全局），被回填成了 %d", ov.MaxStoredToolBytes)
	}
	if ov.MaxSessionBytes != 0 {
		t.Errorf("未被覆盖的总量上限必须保持 0（继承全局），被回填成了 %d", ov.MaxSessionBytes)
	}
}

// 负数归零、三项全空的条目删除。
//
// 空条目留着不是洁癖问题：界面按「哪些主机有覆盖」呈现状态，
// 一堆空条目会让用户看到「3 台主机设了独立上限」而实际一个都没设。
func TestSessionOverridesSanitized(t *testing.T) {
	v := newTestVault(t)

	if err := v.SetPolicy(PolicySettings{
		Mode:      ModeManual,
		MaxOutput: 32 * 1024,
		MaxSteps:  12,
		SessionOverrides: map[string]SessionLimits{
			"negative": {MaxSessionTurns: -5, MaxStoredToolBytes: -1, MaxSessionBytes: -7},
			"empty":    {},
			"mixed":    {MaxSessionTurns: -3, MaxSessionBytes: 1 << 20},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got := v.Policy().SessionOverrides
	if _, ok := got["empty"]; ok {
		t.Errorf("三项全空的条目应被删掉，实得 %+v", got["empty"])
	}
	if _, ok := got["negative"]; ok {
		t.Errorf("规整后变成全空的条目也应被删掉，实得 %+v", got["negative"])
	}
	m, ok := got["mixed"]
	if !ok {
		t.Fatalf("有效条目不该被删，实得 %+v", got)
	}
	if m.MaxSessionTurns != 0 {
		t.Errorf("负数轮数应归零（= 继承），实得 %d", m.MaxSessionTurns)
	}
	if m.MaxSessionBytes != 1<<20 {
		t.Errorf("同一台主机里有效的项应保留，实得 %d", m.MaxSessionBytes)
	}
}

// Policy() 返回的必须是**副本**，不能把库里的 map / slice 交出去。
//
// 交出去的后果：调用方随手改一下就绕过了 SetPolicy —— 既不落盘也不留审计，
// 而之后每次 Policy() 都读到那个脏值，直到重启才恢复。
// 这种「改了设置、当时生效、重启后变回去」极难归因，所以专门锁住。
func TestPolicyReturnsDetachedCopy(t *testing.T) {
	v := newTestVault(t)

	p := v.Policy()
	p.SessionOverrides = map[string]SessionLimits{"host-a": {MaxSessionTurns: 99}}
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}

	view := v.Policy()
	view.SessionOverrides["host-a"] = SessionLimits{MaxSessionTurns: 12345}
	view.SessionOverrides["host-injected"] = SessionLimits{MaxSessionTurns: 1}
	view.Whitelist[0] = "definitely-not-whitelisted"

	again := v.Policy()
	if got := again.SessionOverrides["host-a"].MaxSessionTurns; got != 99 {
		t.Errorf("改动返回值不该影响库里的状态：应仍为 99，实得 %d", got)
	}
	if _, ok := again.SessionOverrides["host-injected"]; ok {
		t.Error("往返回值里塞的条目不该出现在库里")
	}
	if again.Whitelist[0] == "definitely-not-whitelisted" {
		t.Error("改动返回的白名单不该影响库里的状态")
	}
}

// SetPolicy 不该改动调用方持有的那份 map。
func TestSetPolicyDoesNotMutateCallerMap(t *testing.T) {
	v := newTestVault(t)

	ov := map[string]SessionLimits{
		"empty":  {}, // 会被规整掉
		"host-a": {MaxSessionTurns: 30},
	}
	if err := v.SetPolicy(PolicySettings{
		Mode: ModeManual, MaxOutput: 32 * 1024, MaxSteps: 12,
		SessionOverrides: ov,
	}); err != nil {
		t.Fatal(err)
	}

	if len(ov) != 2 {
		t.Errorf("规整不该删掉调用方那份 map 里的条目，实得 %d 条: %+v", len(ov), ov)
	}
}

// 删主机时必须顺手清掉它的覆盖。
//
// 主机 ID 是随机生成的、不会复用，所以留下的覆盖再也不可能生效；
// 但它会被原样下发到前端，让「N 台主机设了独立上限」永远比实际多，
// 而用户在主机的列表里根本找不到多出来的那一台。
func TestDeleteHostRemovesSessionOverride(t *testing.T) {
	v := newTestVault(t)

	if err := v.SaveHost(Host{ID: "h-del", Name: "gone", Addr: "127.0.0.1:22", User: "root", AuthMethod: AuthPassword}, nil); err != nil {
		t.Fatal(err)
	}
	p := v.Policy()
	p.SessionOverrides = map[string]SessionLimits{
		"h-del":  {MaxSessionTurns: 30},
		"h-keep": {MaxSessionTurns: 12},
	}
	if err := v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}

	if err := v.DeleteHost("h-del"); err != nil {
		t.Fatal(err)
	}

	got := v.Policy().SessionOverrides
	if _, ok := got["h-del"]; ok {
		t.Errorf("被删主机的覆盖应一并清掉，实得 %+v", got)
	}
	if _, ok := got["h-keep"]; !ok {
		t.Errorf("其他主机的覆盖不该受影响，实得 %+v", got)
	}
}

func strPtr(s string) *string { return &s }

// 回归测试：默认白名单不得包含可执行任意子进程的解释器。
//
// 历史漏洞：出厂白名单把 awk 与 find 当作「只读诊断」放进去了。二者都有直接的
// 执行出口（awk 的 system()、find 的 -exec），而它们的命令行**不含任何分隔符**，
// 段首就是白名单词 —— 于是命中 Allow，而 Allow 不经过 requestApproval，
// 等于一条无人确认的任意命令执行通道，开箱即用。
//
// 这条测试的价值在于「防止有人好心加回来」：解释器命令看起来人畜无害。
func TestDefaultWhitelistHasNoInterpreter(t *testing.T) {
	// 只要出现在白名单里就等于 RCE 的命令。判定标准是「能否执行任意子进程」，
	// 不是「像不像查询命令」。
	banned := []string{
		"awk", "gawk", "mawk",
		"find",
		"sed",
		"xargs", "eval", "exec", "source",
		"sh", "bash", "zsh", "dash", "ksh",
		"perl", "python", "python3", "ruby", "node", "php", "lua",
		"env", "printenv",
		"tee", "install", "nohup", "watch",
	}
	wl := DefaultWhitelist()
	for _, w := range wl {
		low := strings.ToLower(strings.TrimSpace(w))
		for _, b := range banned {
			// 精确匹配或「命令 + 空格」形式（如 "awk -f"）都算命中
			if low == b || strings.HasPrefix(low, b+" ") {
				t.Errorf("默认白名单不应包含解释器命令 %q —— 它可执行任意子进程，"+
					"而白名单命中走 Allow（无人确认直发远端）", w)
			}
		}
	}
}

// 反向保证：清理解释器时别把清单清空，否则白名单模式会退化成「什么都需确认」。
func TestDefaultWhitelistStillUseful(t *testing.T) {
	wl := DefaultWhitelist()
	if len(wl) < 40 {
		t.Fatalf("默认白名单只剩 %d 条，疑似误删过多", len(wl))
	}
	// 几个必须保留的核心只读诊断命令
	for _, must := range []string{"ls", "cat", "grep", "tail", "df", "journalctl", "systemctl status"} {
		found := false
		for _, w := range wl {
			if w == must {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("默认白名单缺少核心只读命令 %q", must)
		}
	}
}
