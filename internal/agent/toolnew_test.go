package agent

// 新增工具的端到端测试。
//
// 与 toolregistry_test.go 的分工：那一边测「注册表本身是否自洽」，
// 这一边测「每个工具真的能跑通，且跑通的姿势符合安全预期」。
// 缺了后者，一个三份能力齐全但执行体写错的工具照样全绿。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

// webFixture 是出网工具的测试夹具：一个假 HTTP 服务 + 一套 harness。
type webFixture struct {
	ctx toolCtx
	h   *harness
	// url 是假服务的地址，测试用它当 fetch_webpage 的入参。
	url string
}

// newWebFixture 造一个返回固定 body 的假站点。
//
// 用 httptest 而不是真网：真网让测试依赖外部站点（会变、会限流、会挂），
// 而这里要测的是「解析与边界」，不是「互联网通不通」。
func newWebFixture(t *testing.T, body string) webFixture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	h := newHarness(t, nil, policy.ModeWhitelist, false)
	t.Cleanup(func() { h.ssh.Close() })

	return webFixture{
		ctx: toolCtx{
			ssh:  h.ssh,
			v:    h.v,
			emit: func(string, any) {},
			http: srv.Client(),
		},
		h:   h,
		url: srv.URL + "/doc",
	}
}

// ---- json_query ----

// 主路径：从远端 JSON 里取出一个字段。
func TestJSONQueryExtractsField(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	doc := map[string]any{
		"spec": map[string]any{"replicas": 3, "image": "nginx:1.25"},
		"items": []any{
			map[string]any{"name": "first"},
			map[string]any{"name": "second"},
		},
	}
	raw, _ := json.Marshal(doc)
	const path = "/srv/deploy.json"
	h.srv.SetRemoteFile(path, string(raw))

	res, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": path, "query": ".spec.replicas",
	})
	if err != nil {
		t.Fatalf("json_query failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "3") {
		t.Fatalf("want replicas=3, got: %s", res.Stdout)
	}
}

// 数组下标：.items[1].name 应取到第二个元素。
func TestJSONQueryArrayIndex(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	h.srv.SetRemoteFile("/srv/a.json", `{"items":[{"name":"first"},{"name":"second"}]}`)

	res, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": "/srv/a.json", "query": ".items[1].name",
	})
	if err != nil {
		t.Fatalf("json_query failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "second") {
		t.Fatalf("want second, got: %s", res.Stdout)
	}
}

// 错误必须能指导模型改参数，而不是一句「查询失败」。
//
// 字段不存在时要把**该对象有哪些字段**说出来 —— 否则模型只能瞎猜，
// 而它猜的下一次调用又是一次失败往返。
func TestJSONQueryUnknownFieldListsAvailableKeys(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	h.srv.SetRemoteFile("/srv/b.json", `{"alpha":1,"beta":2}`)

	_, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": "/srv/b.json", "query": ".gamma",
	})
	if err == nil {
		t.Fatal("unknown field should error")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error should list available keys, got: %v", err)
	}
}

// 非 JSON 内容必须明确报错，而不是当空文档返回。
func TestJSONQueryRejectsNonJSON(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	h.srv.SetRemoteFile("/srv/c.conf", "listen = 8080\nnot json at all\n")

	_, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": "/srv/c.conf", "query": ".",
	})
	if err == nil {
		t.Fatal("non-JSON content should error")
	}
}

// 深层嵌套必须被拒绝，而不是递归到爆栈。
func TestJSONQueryRejectsDeepNesting(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	deep := strings.Repeat(`{"a":`, 200) + "1" + strings.Repeat("}", 200)
	h.srv.SetRemoteFile("/srv/deep.json", deep)

	_, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": "/srv/deep.json", "query": ".",
	})
	if err == nil {
		t.Fatal("deep nesting should error")
	}
	if !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("error should mention nesting depth, got: %v", err)
	}
}

// 多个 JSON 值（JSON Lines）必须报错：静默只取第一个会让模型
// 以为看全了数据。
func TestJSONQueryRejectsMultipleValues(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	h.srv.SetRemoteFile("/srv/lines.jsonl", `{"a":1}`+"\n"+`{"b":2}`+"\n")

	_, err := runJSONQuery(newToolCtx(t, h), map[string]any{
		"host_id": h.hostID, "path": "/srv/lines.jsonl", "query": ".",
	})
	if err == nil {
		t.Fatal("multiple JSON values should error")
	}
}

// 缺参数时必须给出具体是哪个参数缺失。
func TestJSONQueryValidatesArgs(t *testing.T) {
	h := newHarness(t, nil, policy.ModeWhitelist, false)
	defer h.ssh.Close()

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing host_id", map[string]any{"path": "/x", "query": "."}, "host_id"},
		{"missing path", map[string]any{"host_id": "h", "query": "."}, "path"},
		{"missing query", map[string]any{"host_id": "h", "path": "/x"}, "query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runJSONQuery(newToolCtx(t, h), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %s, got: %v", tc.want, err)
			}
		})
	}
}

// ---- fetch_webpage ----

// 主路径：抓到 HTML 后应返回正文而不是标签。
func TestFetchWebpageExtractsText(t *testing.T) {
	f := newWebFixture(t, `<!DOCTYPE html><html><head>
		<title>Nginx Docs</title><style>body{color:red}</style>
		<script>alert('x')</script></head>
		<body><h1>worker_connections</h1><p>per-worker connection limit.</p>
		<script>tracker()</script></body></html>`)

	res, err := runFetchWebpage(f.ctx, map[string]any{"url": f.url})
	if err != nil {
		t.Fatalf("fetch_webpage failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "worker_connections") {
		t.Fatalf("want body text, got: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "Nginx Docs") {
		t.Fatalf("want title, got: %s", res.Stdout)
	}
	// script/style must be dropped: they cost context and are irrelevant.
	if strings.Contains(res.Stdout, "alert") || strings.Contains(res.Stdout, "color:red") {
		t.Fatalf("script/style content should be stripped, got: %s", res.Stdout)
	}
}

// 非 http/https scheme 必须拒绝。
//
// file:// 在本工具里没有任何合理用途，却是读取本地文件的通道 ——
// 而本地有凭证库。
func TestFetchWebpageRejectsNonHTTPScheme(t *testing.T) {
	f := newWebFixture(t, "ok")

	for _, u := range []string{
		"file:///C:/Users/admin/.kiro/settings/permissions.yaml",
		"gopher://example.com/1",
	} {
		if _, err := runFetchWebpage(f.ctx, map[string]any{"url": u}); err == nil {
			t.Fatalf("should reject %s", u)
		}
	}
}

// 省略 scheme 时应补 https 而不是报错。
func TestFetchWebpageAcceptsBareHost(t *testing.T) {
	f := newWebFixture(t, "hello")

	bare := strings.TrimPrefix(f.url, "http://")
	_, err := runFetchWebpage(f.ctx, map[string]any{"url": bare})
	// 补成 https 后连不上本地 http 测试服务器 —— 这里只验证
	// 「没有因为缺 scheme 而被拒」这件事本身。
	if err != nil && strings.Contains(err.Error(), "scheme") {
		t.Fatalf("should not reject a bare host for scheme reasons: %v", err)
	}
}

// 4xx/5xx 必须带状态码：404 与 403 对模型的下一步完全不同
// （前者该换 URL，后者该考虑是否需要登录）。
func TestFetchWebpageReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f := newWebFixture(t, "x")
	f.ctx.http = srv.Client()

	_, err := runFetchWebpage(f.ctx, map[string]any{"url": srv.URL})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want a 403 report, got: %v", err)
	}
}

// ---- search_web ----

// 策略关闭时 search_web 必须不可用，且告知去哪开。
func TestSearchWebDeniedWhenPolicyOff(t *testing.T) {
	f := newWebFixture(t, "x")

	_, err := runSearchWeb(f.ctx, map[string]any{"query": "nginx 502"})
	if err == nil {
		t.Fatal("search_web should fail when web access is off")
	}
	if !strings.Contains(err.Error(), "策略") {
		t.Fatalf("error should say it is a policy denial, got: %v", err)
	}
}

// 策略开启但未配提供方时，应提示去配置，并指出可用 fetch_webpage。
func TestSearchWebNeedsProvider(t *testing.T) {
	f := newWebFixture(t, "x")

	if err := f.h.v.SetPolicy(vault.PolicySettings{Mode: vault.ModeWhitelist, AllowWeb: true}); err != nil {
		t.Fatal(err)
	}
	_, err := runSearchWeb(f.ctx, map[string]any{"query": "x"})
	if err == nil {
		t.Fatal("should fail without a configured provider")
	}
	if !strings.Contains(err.Error(), "fetch_webpage") {
		t.Fatalf("should suggest fetch_webpage, got: %v", err)
	}
}

// 配了提供方后应能搜索并解析出结果。
func TestSearchWebParsesResults(t *testing.T) {
	page := `<html><body>
		<a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fnginx.org%2Fen%2Fdocs%2F">nginx official docs</a>
		<a class="result__snippet" href="//x">worker_connections reference</a>
		</body></html>`
	f := newWebFixture(t, page)

	// 把搜索端点换到本地假服务器：真网会让这条测试因限流、页面改版、
	// 网络不通而随机失败，等于没测。
	t.Cleanup(func() { ddgEndpointOverride = "" })
	ddgEndpointOverride = strings.TrimSuffix(f.url, "/doc")

	if err := f.h.v.SetPolicy(vault.PolicySettings{
		Mode: vault.ModeWhitelist, AllowWeb: true, SearchProvider: "duckduckgo",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := runSearchWeb(f.ctx, map[string]any{"query": "nginx worker_connections"})
	if err != nil {
		t.Fatalf("search_web failed: %v", err)
	}
	if !strings.Contains(res.Stdout, "nginx official docs") {
		t.Fatalf("want parsed title, got: %s", res.Stdout)
	}
	// 跳转链接必须解成真实目标，否则模型会拿着一个跳转页去 fetch。
	if !strings.Contains(res.Stdout, "nginx.org") {
		t.Fatalf("want unwrapped real link, got: %s", res.Stdout)
	}
}

// ---- local path guard ----

// 凭证库、主密钥、审计日志一律不可上传。
func TestUploadLocalFileBlocksCredentialFiles(t *testing.T) {
	f := newWebFixture(t, "x")

	dir := t.TempDir()
	for _, name := range []string{"vault.enc", "master.key", "audit.log", "sessions.enc"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := runUploadLocalFile(f.ctx, map[string]any{
			"host_id": f.h.hostID, "path": "/tmp/x", "local": p,
		})
		if err == nil {
			t.Fatalf("uploading %s should be refused", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error should name the file, got: %v", err)
		}
	}
}

// 归档段的审计日志同样要拦（audit.seg-*.log）。
func TestUploadLocalFileBlocksAuditSegments(t *testing.T) {
	f := newWebFixture(t, "x")

	p := filepath.Join(t.TempDir(), "audit.seg-0000000001-0000004200.log")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runUploadLocalFile(f.ctx, map[string]any{
		"host_id": f.h.hostID, "path": "/tmp/x", "local": p,
	}); err == nil {
		t.Fatal("uploading an audit segment should be refused")
	}
}

// 反向：远端内容不得覆盖本地凭证库。
func TestDownloadRemoteFileBlocksCredentialTarget(t *testing.T) {
	f := newWebFixture(t, "x")

	f.h.srv.SetRemoteFile("/srv/ok.conf", "hello")

	target := filepath.Join(t.TempDir(), "vault.enc")
	_, err := runDownloadRemoteFile(f.ctx, map[string]any{
		"host_id": f.h.hostID, "path": "/srv/ok.conf", "local": target,
	})
	if err == nil {
		t.Fatal("downloading onto vault.enc should be refused")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("a refused download must not leave a local file")
	}
}

// 主路径：上传一个普通文件，远端应真的收到内容。
func TestUploadLocalFileSendsContent(t *testing.T) {
	f := newWebFixture(t, "x")

	src := filepath.Join(t.TempDir(), "app.conf")
	if err := os.WriteFile(src, []byte("listen = 9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const dest = "/srv/uploaded.conf"
	res, err := runUploadLocalFile(f.ctx, map[string]any{
		"host_id": f.h.hostID, "path": dest, "local": src,
	})
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if !strings.Contains(res.Stdout, dest) {
		t.Fatalf("result should name the target path, got: %s", res.Stdout)
	}
	got, ok := f.h.srv.ReadRemoteFile(dest)
	if !ok || !strings.Contains(got, "listen = 9090") {
		t.Fatalf("remote should have the content, got ok=%v content=%q", ok, got)
	}
}

// content_base64 逃生门：没有本地文件时也能传内容。
func TestUploadLocalFileAcceptsInlineBase64(t *testing.T) {
	f := newWebFixture(t, "x")

	const dest = "/srv/inline.conf"
	if _, err := runUploadLocalFile(f.ctx, map[string]any{
		"host_id":        f.h.hostID,
		"path":           dest,
		"content_base64": base64.StdEncoding.EncodeToString([]byte("inline-ok\n")),
	}); err != nil {
		t.Fatalf("base64 upload failed: %v", err)
	}
	if got, ok := f.h.srv.ReadRemoteFile(dest); !ok || !strings.Contains(got, "inline-ok") {
		t.Fatalf("remote should have the decoded content, got ok=%v %q", ok, got)
	}
}

// 主路径：下载远端文件到本地，内容与大小都必须对得上。
func TestDownloadRemoteFileWritesContent(t *testing.T) {
	f := newWebFixture(t, "x")

	const src = "/srv/big.log"
	// 造一个跨多个块的文件：ChunkSize 是 512 KiB，用一个 1 MiB 的
	// 内容确保分块循环真的跑了不止一圈。
	big := strings.Repeat("log-line-0123456789\n", 100_000)
	f.h.srv.SetRemoteFile(src, big)

	dest := filepath.Join(t.TempDir(), "out.log")
	if _, err := runDownloadRemoteFile(f.ctx, map[string]any{
		"host_id": f.h.hostID, "path": src, "local": dest,
	}); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("reading local file failed: %v", rerr)
	}
	if string(got) != big {
		t.Fatalf("content mismatch: want %d bytes, got %d", len(big), len(got))
	}
}

// 成功的下载必须留下完整文件。
func TestDownloadRemoteFileKeepsCompleteFile(t *testing.T) {
	f := newWebFixture(t, "x")

	f.h.srv.SetRemoteFile("/srv/ok.log", strings.Repeat("x", 4096))

	dest := filepath.Join(t.TempDir(), "ok.log")
	if _, err := runDownloadRemoteFile(f.ctx, map[string]any{
		"host_id": f.h.hostID, "path": "/srv/ok.log", "local": dest,
	}); err != nil {
		t.Fatalf("download failed: %v", err)
	}
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("reading local file failed: %v", rerr)
	}
	if len(got) != 4096 {
		t.Fatalf("want the full 4096 bytes, got %d", len(got))
	}
}

// ---- helpers ----

// newToolCtx 造一个指向测试 harness 的 toolCtx。
func newToolCtx(t *testing.T, h *harness) toolCtx {
	t.Helper()
	return toolCtx{ssh: h.ssh, v: h.v, emit: func(string, any) {}}
}
