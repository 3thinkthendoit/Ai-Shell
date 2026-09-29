package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-shell/internal/vault"
)

// 本文件测的是「测试连接」这条**完整链路**：
//
//	SettingsPanel → App.TestLLM → llm.NormalizeBaseURL → llm.Ping → HTTP
//
// 为什么值得单独写：真实的那个 bug 不在错误文案里，也不在 client 内部，
// 而在「Base URL 里已经带了 /chat/completions，客户端又拼了一次」这个接缝上。
// app_test.go 只测了 diagnoseLLMError（纯函数），恰好测不到这个接缝。
// 所以这里必须用真的 HTTP 服务端，断言**收到的 path 到底是什么**。

// newTestApp 装配一个最小可用的 App。
//
// TestLLM 只用到 a.v，所以不必跑完整 startup()（那会去写用户真实配置目录）。
// AISHELL_KEYFILE=1 让主密钥落在临时目录里，避免碰系统钥匙串。
func newTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")

	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开临时凭证库失败: %v", err)
	}
	return &App{v: v}
}

// fakeLLM 是一个最小的 OpenAI 兼容服务端，记录收到的请求供断言。
type fakeLLM struct {
	*httptest.Server

	path   string // 最近一次请求的 path —— 本文件的核心断言对象
	auth   string
	body   map[string]any
	hits   int
	status int    // 非 0 时固定返回该状态码
	raw    string // 非空时直接返回这段原文（用于模拟非 LLM 服务）
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()
	f := &fakeLLM{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		f.path = r.URL.Path
		f.auth = r.Header.Get("Authorization")
		if b, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(b, &f.body)
		}

		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		if f.raw != "" {
			_, _ = w.Write([]byte(f.raw))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// 用户填的 Base URL 形态五花八门，但落到线上的 path 必须**永远**是
// `<base>/chat/completions`，而且只能有一个 /chat/completions。
//
// 这条用例直接对应用户踩到的真实故障：他填的是
// https://token.sensenova.cn/v1/chat/completions，
// 旧代码无条件再拼一次 → /v1/chat/completions/chat/completions → 404。
func TestTestLLM_BaseURLVariantsAllHitSingleChatCompletionsPath(t *testing.T) {
	cases := []struct {
		name string
		base func(srv string) string
	}{
		{"干净的 /v1", func(s string) string { return s + "/v1" }},
		{"带尾斜杠", func(s string) string { return s + "/v1/" }},
		{"多个尾斜杠", func(s string) string { return s + "/v1///" }},
		{"已经带了端点（用户踩到的那个）", func(s string) string { return s + "/v1/chat/completions" }},
		{"带了端点还带尾斜杠", func(s string) string { return s + "/v1/chat/completions/" }},
		{"首尾有空格", func(s string) string { return "  " + s + "/v1  " }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeLLM(t)
			a := newTestApp(t)

			res := a.TestLLM(c.base(f.URL), "test-model", "sk-test")

			// 核心断言：服务端实际收到的 path
			if f.path != "/v1/chat/completions" {
				t.Fatalf("服务端收到的 path 是 %q，期望 %q（重复拼接会让真实服务返回 404）",
					f.path, "/v1/chat/completions")
			}
			if f.hits != 1 {
				t.Fatalf("应恰好请求 1 次，实得 %d 次", f.hits)
			}
			if !res.OK {
				t.Fatalf("期望连接成功，实得: ok=%v msg=%s", res.OK, res.Message)
			}
			// 回给 UI 的 URL 必须是规范化后的，用户照着它改才改得对
			want := f.URL + "/v1/chat/completions"
			if res.URL != want {
				t.Errorf("结果里的 URL 应为 %q，实得 %q", want, res.URL)
			}
		})
	}
}

// 结果字段与请求形状：UI 直接展示这些字段，缺一个界面就空白。
func TestTestLLM_ResultFieldsAndRequestShape(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	res := a.TestLLM(f.URL+"/v1", "deepseek-flash", "sk-secret")

	if !res.OK {
		t.Fatalf("期望成功，实得: %s", res.Message)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status 应为 200，实得 %d", res.Status)
	}
	if res.Model != "deepseek-flash" {
		t.Errorf("model 应为 deepseek-flash，实得 %q", res.Model)
	}
	if res.Reply != "pong" {
		t.Errorf("reply 应解析出模型正文，实得 %q", res.Reply)
	}
	if res.DurationMs < 0 {
		t.Errorf("durationMs 不应为负，实得 %d", res.DurationMs)
	}
	if res.Message == "" {
		t.Error("成功时也应有可展示的 message")
	}

	// 请求头：带 key，且 URL 里不能出现 key（否则会进日志/审计）
	if f.auth != "Bearer sk-secret" {
		t.Errorf("Authorization 头应为 %q，实得 %q", "Bearer sk-secret", f.auth)
	}
	if strings.Contains(res.URL, "sk-secret") {
		t.Errorf("返回的 URL 里泄露了 API Key: %s", res.URL)
	}

	// 请求体：最小请求。max_tokens 必须小，否则「测试」会变成一次真消费。
	if got := f.body["model"]; got != "deepseek-flash" {
		t.Errorf("请求体 model 应为 deepseek-flash，实得 %v", got)
	}
	msgs, _ := f.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("测试请求应只带 1 条消息，实得 %d 条", len(msgs))
	}
	mt, _ := f.body["max_tokens"].(float64)
	if mt <= 0 || mt > 16 {
		t.Errorf("max_tokens 应为一个很小的值（≤16），实得 %v —— 测试不该产生实质费用", f.body["max_tokens"])
	}
	if st, ok := f.body["stream"].(bool); ok && st {
		t.Error("测试请求不应开启流式")
	}
	if _, ok := f.body["tools"]; ok {
		t.Error("测试请求不该携带工具定义")
	}
}

// 404 是最常见的配置错误，提示里必须同时有「怎么改」和「实际请求了什么」。
func TestTestLLM_404ReportsActionableMessage(t *testing.T) {
	f := newFakeLLM(t)
	f.status = http.StatusNotFound
	a := newTestApp(t)

	res := a.TestLLM(f.URL+"/v1", "m", "k")

	if res.OK {
		t.Fatal("404 不应报告成功")
	}
	if res.Status != http.StatusNotFound {
		t.Errorf("status 应回传 404，实得 %d", res.Status)
	}
	for _, want := range []string{"不要带 /chat/completions", f.URL + "/v1/chat/completions"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("404 提示里应包含 %q，实得: %s", want, res.Message)
		}
	}
	// 失败时 URL 也要回传，否则前端没东西可展示
	if res.URL == "" {
		t.Error("失败时也应回传实际请求地址，便于用户对照")
	}
}

// 地址指向一个普通网页（200 但不是 LLM）时要能识别出来，
// 否则用户会以为「通了」，然后在真正对话时才发现不对。
func TestTestLLM_RejectsNonLLMResponse(t *testing.T) {
	f := newFakeLLM(t)
	f.raw = "<html><body>hello</body></html>"
	a := newTestApp(t)

	res := a.TestLLM(f.URL+"/v1", "m", "k")

	if res.OK {
		t.Fatal("返回 HTML 不应报告成功")
	}
	if !strings.Contains(res.Message, "非 LLM") {
		t.Errorf("提示应指出地址可能指向了非 LLM 服务，实得: %s", res.Message)
	}
}

// 连不上（端口没人听）时 status 必须是 0，前端靠这个区分
// 「配置错」和「网络不通」—— 否则会给出误导性的建议。
func TestTestLLM_UnreachableReportsStatusZero(t *testing.T) {
	a := newTestApp(t)

	// 起一个服务再立刻关掉，拿到一个确定没人监听的端口
	f := newFakeLLM(t)
	dead := f.URL
	f.Close()

	res := a.TestLLM(dead+"/v1", "m", "k")

	if res.OK {
		t.Fatal("连不上不应报告成功")
	}
	if res.Status != 0 {
		t.Errorf("没发出请求时 status 应为 0，实得 %d", res.Status)
	}
	if !strings.Contains(res.Message, "连不上") {
		t.Errorf("提示应说明连不上，实得: %s", res.Message)
	}
}

// 「先测再存」要求表单里的值优先；但表单留空时必须回落到已保存的配置，
// 否则用户清空输入框点测试会得到一个莫名其妙的失败。
func TestTestLLM_FallsBackToSavedConfig(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	key := "sk-saved"
	if err := a.v.SetLLM(f.URL+"/v1", "saved-model", &key); err != nil {
		t.Fatal(err)
	}

	res := a.TestLLM("", "", "")

	if !res.OK {
		t.Fatalf("留空时应使用已保存配置并成功，实得: %s", res.Message)
	}
	if f.auth != "Bearer sk-saved" {
		t.Errorf("应使用已保存的 key，实得 Authorization=%q", f.auth)
	}
	if res.Model != "saved-model" {
		t.Errorf("应使用已保存的模型名，实得 %q", res.Model)
	}
}

// 表单里填了值就要优先用表单值（这就是「先测再存」的前提）。
func TestTestLLM_FormValuesOverrideSaved(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	key := "sk-saved"
	if err := a.v.SetLLM("http://saved.invalid/v1", "saved-model", &key); err != nil {
		t.Fatal(err)
	}

	res := a.TestLLM(f.URL+"/v1", "form-model", "sk-form")

	if !res.OK {
		t.Fatalf("应使用表单值并成功，实得: %s", res.Message)
	}
	if f.auth != "Bearer sk-form" {
		t.Errorf("应使用表单里的 key，实得 %q", f.auth)
	}
	if res.Model != "form-model" {
		t.Errorf("应使用表单里的模型名，实得 %q", res.Model)
	}
	if f.path != "/v1/chat/completions" {
		t.Errorf("path 应为 /v1/chat/completions，实得 %q", f.path)
	}
}

// 全新安装的真实行为：vault 里预置了默认 Base URL（api.openai.com/v1）与模型名，
// 所以「表单留空」并不等于「没配置」—— 请求会真的打到预置的那个地址。
//
// 这条用例把这个行为**固定下来**，因为它是刻意的：
// 预置值就是表单里显示的值，用户点了「测试连接」即表示要测这个地址。
// 而且不预置的话，本地无鉴权的 vLLM/Ollama 就没法测（见下一个用例）。
//
// 关键是结果里必须回传真实地址，用户才看得出自己测的是哪儿。
func TestTestLLM_FreshInstallUsesSeededDefaults(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	// 不往 vault 写任何东西，模拟全新安装
	saved, _ := a.v.LLM()
	if saved.BaseURL == "" || saved.Model == "" {
		t.Fatalf("前提不成立：全新安装的 vault 应预置 Base URL 与模型名，实得 %+v", saved)
	}

	// 让预置的 Base URL 指向我们的假服务端
	if err := a.v.SetLLM(f.URL+"/v1", "seeded-model", nil); err != nil {
		t.Fatal(err)
	}

	res := a.TestLLM("", "", "")

	if f.hits != 1 {
		t.Fatalf("表单留空时应回落到已保存（预置）配置并请求一次，实得 %d 次", f.hits)
	}
	if res.URL != f.URL+"/v1/chat/completions" {
		t.Errorf("结果里应回传真实请求地址，实得 %q", res.URL)
	}
}

// 优先级规则（谁覆盖谁）单独测。
//
// 为什么值得单独抽出来测：vault 预置了默认值、SetLLM 又忽略空串，
// 所以「已保存的 Base URL 为空」这个状态在正常流程里根本到不了 ——
// 想靠构造 App 来覆盖这几条分支是白费力气。
func TestResolveLLMConfig(t *testing.T) {
	saved := vault.LLMSettings{BaseURL: "https://saved.example/v1", Model: "saved-model"}

	t.Run("表单值优先", func(t *testing.T) {
		base, model, key := resolveLLMConfig("https://form.example/v1", "form-model", "sk-form", saved, "sk-saved")
		if base != "https://form.example/v1" || model != "form-model" || key != "sk-form" {
			t.Fatalf("表单值应优先，实得 %q / %q / %q", base, model, key)
		}
	})

	t.Run("留空则沿用已保存", func(t *testing.T) {
		base, model, key := resolveLLMConfig("", "", "", saved, "sk-saved")
		if base != "https://saved.example/v1" || model != "saved-model" || key != "sk-saved" {
			t.Fatalf("留空应沿用已保存值，实得 %q / %q / %q", base, model, key)
		}
	})

	t.Run("只有空白也算留空", func(t *testing.T) {
		base, model, key := resolveLLMConfig("   ", "\t", "  ", saved, "sk-saved")
		if base != "https://saved.example/v1" || model != "saved-model" || key != "sk-saved" {
			t.Fatalf("纯空白应视为留空，实得 %q / %q / %q", base, model, key)
		}
	})

	t.Run("表单里的脏地址要规范化", func(t *testing.T) {
		base, _, _ := resolveLLMConfig("https://form.example/v1/chat/completions/", "m", "k", saved, "")
		if base != "https://form.example/v1" {
			t.Fatalf("表单地址应规范化，实得 %q", base)
		}
	})

	t.Run("已保存的脏地址同样要规范化", func(t *testing.T) {
		// 这条正是历史配置的形态：旧版本保存时没做规范化，
		// 盘上就留着带端点的地址。回落分支若不规范化，
		// 错误提示里显示的地址会和实际请求的不一致。
		legacy := vault.LLMSettings{BaseURL: "https://token.sensenova.cn/v1/chat/completions", Model: "m"}
		base, _, _ := resolveLLMConfig("", "", "", legacy, "")
		if base != "https://token.sensenova.cn/v1" {
			t.Fatalf("已保存的地址也应规范化，实得 %q", base)
		}
	})

	t.Run("两边都空才返回空（守卫前提）", func(t *testing.T) {
		base, model, _ := resolveLLMConfig("", "", "", vault.LLMSettings{}, "")
		if base != "" || model != "" {
			t.Fatalf("两边都空时应返回空值，实得 %q / %q", base, model)
		}
	})
}

// 没配密钥时的 401，提示必须说「没配」而不是「不正确」——
// 否则用户会去找一个根本不存在的手误。
func TestTestLLM_401WithoutKeySaysNotConfigured(t *testing.T) {
	f := newFakeLLM(t)
	f.status = http.StatusUnauthorized
	a := newTestApp(t)

	res := a.TestLLM(f.URL+"/v1", "m", "")

	if res.OK {
		t.Fatal("401 不应报告成功")
	}
	if !strings.Contains(res.Message, "还没有配置 API Key") {
		t.Errorf("没配密钥时应说「还没有配置」，实得: %s", res.Message)
	}
}

// 配了密钥但服务端仍拒绝 —— 这时才该说「密钥不正确」。
func TestTestLLM_401WithKeySaysIncorrect(t *testing.T) {
	f := newFakeLLM(t)
	f.status = http.StatusUnauthorized
	a := newTestApp(t)

	res := a.TestLLM(f.URL+"/v1", "m", "sk-wrong")

	if res.OK {
		t.Fatal("401 不应报告成功")
	}
	if !strings.Contains(res.Message, "API Key 不正确") {
		t.Errorf("配了密钥时应说「不正确」，实得: %s", res.Message)
	}
}

// 凭证库没起来时要给出可读的错误，而不是 panic。
func TestTestLLM_VaultNotReady(t *testing.T) {
	a := &App{} // 没有 v

	res := a.TestLLM("http://x/v1", "m", "k")

	if res.OK {
		t.Fatal("凭证库未初始化不应报告成功")
	}
	if res.Message == "" {
		t.Error("应给出错误说明")
	}
}

// 超时保护：客户端本身是 180s（给长对话用），测试不该让用户干等三分钟。
//
// 这里把超时临时缩短到 200ms，而不是真等 20 秒 —— 断言的是
// 「deadline 确实由测试这条路径提供且会生效」，而不是某个魔法数字。
func TestTestLLM_UsesOwnShortTimeoutNotClientTimeout(t *testing.T) {
	old := llmTestTimeout
	llmTestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { llmTestTimeout = old })

	release := make(chan struct{})
	f := newFakeLLM(t)
	f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // 客户端放弃
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) })

	a := newTestApp(t)

	start := time.Now()
	res := a.TestLLM(f.URL+"/v1", "m", "k")
	elapsed := time.Since(start)

	if res.OK {
		t.Fatal("超时不应报告成功")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("测试连接应遵守 llmTestTimeout（200ms），实际等了 %s —— "+
			"说明用的还是客户端那 180s 超时", elapsed)
	}
	if !strings.Contains(res.Message, "超时") {
		t.Errorf("应提示超时，实得: %s", res.Message)
	}
}

// TestLLMProfile 对不存在的方案 ID 必须显式报错，而不是静默回落到
// 激活方案 —— 回落会让用户拿到「另一套方案」的测试结论，比失败更误导。
// （场景：编辑到一半，这套方案在别处被删了。）
func TestTestLLMProfile_UnknownIDReportsDeleted(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	// 前提：激活方案配置可用（迁移自 defaultStore 的旧字段）。
	// 若它自己就能测通，说明「报错」不是碰巧因配置不可用而触发的。
	key := "sk-active"
	if err := a.v.SetLLM(f.URL+"/v1", "active-model", &key); err != nil {
		t.Fatal(err)
	}
	if res := a.TestLLM("", "", ""); !res.OK {
		t.Fatalf("前提不成立：激活方案应可测通，实得 %s", res.Message)
	}
	hitsBefore := f.hits

	res := a.TestLLMProfile("nonexistent", f.URL+"/v1", "m", "k")

	if res.OK {
		t.Fatal("不存在的方案 ID 不应报告成功（那是在测别的方案的配置）")
	}
	if f.hits != hitsBefore {
		t.Fatalf("不存在的方案 ID 不应发出 HTTP 请求，多发了 %d 次", f.hits-hitsBefore)
	}
	if !strings.Contains(res.Message, "不存在") {
		t.Errorf("应说明方案已不存在，实得: %s", res.Message)
	}
}

// 编辑非激活方案且表单留空 Key 时，测试必须用**该方案**已保存的 Key，
// 而不是激活方案的 —— 这是 TestLLMProfile 存在的全部理由。
func TestTestLLMProfile_FallsBackToThatProfilesKey(t *testing.T) {
	f := newFakeLLM(t)
	a := newTestApp(t)

	activeKey := "sk-active"
	if err := a.v.SetLLM(f.URL+"/v1", "active-model", &activeKey); err != nil {
		t.Fatal(err)
	}
	bKey := "sk-profile-b"
	if err := a.v.SaveLLMProfile(vault.LLMProfile{
		ID: "pb", Name: "B", BaseURL: f.URL + "/v1", Model: "b-model",
	}, &bKey); err != nil {
		t.Fatal(err)
	}

	// 表单只填 Base URL 与模型名，Key 留空 → 必须回落到 pb 自己的 Key。
	res := a.TestLLMProfile("pb", f.URL+"/v1", "b-model", "")

	if !res.OK {
		t.Fatalf("应测通，实得: %s", res.Message)
	}
	if f.auth != "Bearer sk-profile-b" {
		t.Fatalf("应使用方案 B 自己的 Key，实得 %q", f.auth)
	}
}

// 保存方案时 Base URL 为空必须被拒绝：存一套空地址的方案，问题要到
// 发起对话时才暴露，且用户很难关联到是哪套方案。
func TestSaveLLMProfile_RejectsEmptyBaseURL(t *testing.T) {
	a := newTestApp(t)

	if _, err := a.SaveLLMProfile(SaveLLMProfileRequest{Name: "x", BaseURL: "   ", Model: "m"}); err == nil {
		t.Fatal("空 Base URL 应被拒绝")
	}
	if _, err := a.SaveLLMProfile(SaveLLMProfileRequest{ID: "p1", Name: "x", BaseURL: "", Model: "m"}); err == nil {
		t.Fatal("编辑时清空 Base URL 也应被拒绝")
	}

	// 正常值应能保存，且返回的视图带后端生成的 ID。
	view, err := a.SaveLLMProfile(SaveLLMProfileRequest{Name: "x", BaseURL: "https://x/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID == "" {
		t.Fatal("新增后应返回后端生成的 ID")
	}
}
