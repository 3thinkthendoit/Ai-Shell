package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"ai-shell/internal/agent"
	"ai-shell/internal/vault"
)

// 本文件验证 Go 后端与 Vue 前端之间的**跨界契约**。
//
// 这是所有 Go 单测都看不到的接缝：两边靠字符串约定通信，没有任何编译器检查。
//
//   - 后端 `runtime.EventsEmit(ctx, "agent:toolResult", …)` ←→ 前端 `EventsOn("agent:toolResult", …)`
//   - 前端 `window.go.main.App.SaveHost(...)` ←→ 后端 `func (a *App) SaveHost(...)`
//
// 任何一侧改名或拼错，功能就会**静默失效**（不报错、不崩溃，就是没反应）。
// 这类 bug 极难靠人工 review 发现，但可以被静态扫描抓住。

// goEvents 通过解析源码，自动收集所有 Ev* 事件常量的值。
//
// 扫**两个**包：internal/agent（agent 状态机的事件）与根包 app.go
// （由 App 直接发的事件，例如交互终端的 term:data）。
// 漏掉任何一个包，那个包里的事件都会被判成「前端听了但后端从不发」——
// 报出一个假失败。而这个测试的全部价值就在于「报出来的都是真的」：
// 一旦它开始冤枉人，人就会开始习惯性忽略它，真问题也就跟着被忽略了。
//
// 这里**刻意不手写清单**。手写的话，新增一个事件却忘了同步清单，
// Go→JS 方向就会静默跳过该事件、测试照样通过 —— 给出虚假的安心感。
// （这个坑真实发生过：加了 agent:delta 之后，只有反向检查把它兜住了。）
func goEvents(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, dir := range []string{filepath.Join("internal", "agent"), "."} {
		out = append(out, eventsInDir(t, dir)...)
	}
	sort.Strings(out)

	// 解析失效哨兵：核心事件必须被解析出来。
	// 否则「解析不到任何事件」会让两个方向的检查都空转通过。
	for _, must := range []string{"agent:done", "agent:tool", "agent:delta", "term:data", "term:exit"} {
		if !slicesContains(out, must) {
			t.Fatalf("未能从源码解析出事件常量 %q（实际解析到 %v）—— "+
				"要么事件被删了，要么本测试的解析逻辑失效", must, out)
		}
	}
	return out
}

// eventsInDir 解析一个目录下所有非测试 Go 文件里的 Ev* 字符串常量。
func eventsInDir(t *testing.T, dir string) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析 %s 源码失败: %v", dir, err)
	}

	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Ev") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if v, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, v)
					}
				}
				return true
			})
		}
	}
	return out
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

var (
	eventsOnRe = regexp.MustCompile(`EventsOn\(\s*['"]([^'"]+)['"]`)
	// 前端有两条调用后端的路径：直接 window.go.main.App.X，以及经 api() 辅助函数 api().X。
	// 两条都必须覆盖 —— 只匹配其中一条会让测试给出假信心。
	appCallRe = regexp.MustCompile(`(?:go\.main\.App|api\(\))\.(\w+)`)
)

// requiredMethods 是 UI 正常运行所必需的后端方法。
// 这是一个「正则失效哨兵」：如果有人重构了前端的调用方式导致上面的正则匹配不到，
// 这些方法就会从发现集合里消失，测试立刻失败 —— 而不是静默通过。
var requiredMethods = []string{
	"Bootstrap", "ListHosts", "SaveHost", "DeleteHost", "TestHost",
	// LLM 多方案：TestLLM 仍在（新建方案时的测试路径），日常的
	// 保存/测试/切换/删除走下面这组；vault.SetLLM 仅剩内部与测试用途。
	"TestLLM", "SaveLLMProfile", "TestLLMProfile",
	"ActivateLLMProfile", "DeleteLLMProfile",
	"SavePolicy", "Ask", "Approve", "Stop", "RunShell",
	"ClearSession", "CompactSession",
	// 会话的增删改查。这四条一起构成「一台主机多条会话」的全部入口，
	// 少任何一条，对应的界面操作就会静默失效（点了没反应）。
	"ListSessions", "CreateSession", "RenameSession", "DeleteSession",
	"ListAudit", "VerifyAudit", "ExportAudit",
}

// frontendFiles 返回前端的全部源码文件（含 .vue）。
func frontendFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("frontend", "src")
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".vue") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描前端源码失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没有找到任何前端源码文件 —— 路径可能变了，本测试会静默失效")
	}
	return out
}

// 后端发出的事件，前端必须都有监听者。
func TestEventContract_GoToJS(t *testing.T) {
	listeners := map[string]string{} // 事件名 → 所在文件
	for _, f := range frontendFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range eventsOnRe.FindAllStringSubmatch(string(b), -1) {
			listeners[m[1]] = f
		}
	}

	for _, ev := range goEvents(t) {
		if _, ok := listeners[ev]; !ok {
			t.Errorf("后端会发出事件 %q，但前端没有任何 EventsOn 监听它 —— 该功能会静默失效", ev)
		}
	}
}

// 前端监听的事件，后端必须真的会发出（防止拼写错误或废弃事件残留）。
func TestEventContract_JSToGo(t *testing.T) {
	known := map[string]bool{}
	for _, ev := range goEvents(t) {
		known[ev] = true
	}

	checked := 0
	for _, f := range frontendFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range eventsOnRe.FindAllStringSubmatch(string(b), -1) {
			checked++
			if !known[m[1]] {
				t.Errorf("%s 监听了 %q，但后端从不发出该事件 —— 拼写错误或事件已废弃", f, m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有在前端找到任何 EventsOn 调用 —— 正则可能失效了")
	}
}

// 前端调用的后端方法，必须在 App 上真实存在。
// 这条能抓住「前端调用了一个被改名/从未实现的方法」这类静默失效。
func TestAppMethodContract_JSToGo(t *testing.T) {
	appPtr := reflect.TypeOf(&App{})
	available := map[string]bool{}
	var names []string
	for i := 0; i < appPtr.NumMethod(); i++ {
		n := appPtr.Method(i).Name
		available[n] = true
		names = append(names, n)
	}
	sort.Strings(names)

	// 暴露给前端的方法必须首字母大写（Wails 的绑定要求）
	if len(available) == 0 {
		t.Fatal("App 上没有任何导出方法 —— Wails 绑定会为空")
	}

	checked := 0
	seen := map[string]string{}
	for _, f := range frontendFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range appCallRe.FindAllStringSubmatch(string(b), -1) {
			checked++
			seen[m[1]] = f
			if !available[m[1]] {
				t.Errorf("%s 调用了 App.%s()，但后端不存在该方法。可用方法: %v",
					f, m[1], names)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有在前端找到任何后端调用 —— 正则可能失效了")
	}

	// 正则失效哨兵：核心方法必须都被发现到
	for _, req := range requiredMethods {
		if _, ok := seen[req]; !ok {
			t.Errorf("未在前端发现对 App.%s() 的调用。要么该功能被删了，"+
				"要么本测试的匹配模式已失效（会给出假信心）", req)
		}
	}

	t.Logf("前端调用了 %d 处后端方法，覆盖 %d 个不同方法；后端共导出 %d 个",
		checked, len(seen), len(available))
}

// 后端导出但前端从未调用的方法 —— 不是错误，但值得知道（可能是死代码）。
func TestAppMethodContract_UnusedBackendMethods(t *testing.T) {
	appPtr := reflect.TypeOf(&App{})
	available := map[string]bool{}
	for i := 0; i < appPtr.NumMethod(); i++ {
		available[appPtr.Method(i).Name] = true
	}

	used := map[string]bool{}
	for _, f := range frontendFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range appCallRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
		}
	}

	var unused []string
	for name := range available {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Logf("后端导出但前端未调用的方法（可能是有意保留的 API）: %v", unused)
	}
}

// 策略设置字段必须完整地出现在生成的 TS 绑定里。
//
// 为什么值得单独测：PolicySettings 是「用户可调」的配置，它的字段要经过
// Go 结构体 → Wails 绑定生成 → models.ts → 前端表单 这条链。
// 任何一环漏掉字段，表现都是**静默的**：界面能填、能保存、不报错，
// 但那个值根本没被存下来 —— 用户只会觉得「设了没用」。
//
// 字段清单从 Go 结构体反射得到，不手写。手写的话，新增字段时
// 很容易忘了同步这份清单，测试照样通过，等于给出假信心。
func TestPolicySettingsFieldsReachBindings(t *testing.T) {
	const modelsPath = "frontend/wailsjs/go/models.ts"
	b, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Skipf("未找到 %s（尚未生成绑定，跳过）: %v", modelsPath, err)
	}
	ts := string(b)

	// 找出 PolicySettings 类的那一段，避免拿别的类里的同名字段蒙混过关。
	start := strings.Index(ts, "export class PolicySettings")
	if start < 0 {
		t.Fatalf("%s 里没有 PolicySettings 类 —— 绑定生成可能失败了", modelsPath)
	}
	// 取到下一个 class 声明为止（或文件末尾）。
	rest := ts[start:]
	if end := strings.Index(rest[1:], "export class "); end >= 0 {
		rest = rest[:end+1]
	}

	// 从 Go 结构体反射出全部 json 字段名。
	typ := reflect.TypeOf(vault.PolicySettings{})
	var missing []string
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		checked++
		if !bindingFieldRe(name).MatchString(rest) {
			missing = append(missing, name)
		}
	}

	if checked == 0 {
		t.Fatal("没有从 PolicySettings 反射出任何字段 —— 测试会静默失效")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("这些策略字段没有出现在 %s 的 PolicySettings 里：%v\n"+
			"后果是前端能填能存但值不会生效（静默失败）。"+
			"通常是绑定没重新生成（跑一次 wails build）。", modelsPath, missing)
	}
	// 只在全部命中时才说「都出现了」—— 否则这条日志会和上面的报错自相矛盾，
	// 让人以为只是警告。
	if len(missing) == 0 {
		t.Logf("PolicySettings 的 %d 个字段都已出现在绑定中", checked)
	}
}

// 会话上下文上限必须同时出现在 Go 结构体和 TS 绑定里。
//
// 这是上面那条通用检查的**哨兵**：如果反射逻辑哪天失效（比如字段全被跳过），
// 上面那条会因为 checked==0 而失败，但若它被误改成 skip，这条仍能兜住。
func TestSessionLimitFieldsExistInBindings(t *testing.T) {
	b, err := os.ReadFile("frontend/wailsjs/go/models.ts")
	if err != nil {
		t.Skipf("尚未生成绑定，跳过: %v", err)
	}
	ts := string(b)
	// sessionOverrides 也在这里：它是主机级覆盖的载体，
	// 少了它前端就没有任何办法下发「这台主机单独设多少」。
	for _, name := range []string{"maxSessionTurns", "maxStoredToolBytes", "maxSessionBytes", "sessionOverrides"} {
		// 同样要求是字段声明（名字 + 可选的 ? + 冒号），避免被前缀相似的字段骗过。
		if !bindingFieldRe(name).MatchString(ts) {
			t.Errorf("绑定里缺少 %s 字段 —— 会话上下文上限无法下发到前端", name)
		}
	}
}

// bindingFieldRe 匹配 models.ts 里某个字段的声明。
//
// 必须是「字段名 + 可选的 ? + 冒号」，**不能**只要求「名字 + 冒号」：
// 带 omitempty 的 Go 字段会生成成可选属性（`sessionOverrides?: ...`），
// 名字与冒号之间多出一个 `?`，只匹配 `\s*:` 的话这类字段会被**全部漏掉**。
//
// 这不是假设：本用例原先就是这么写的，对带 omitempty 的字段一直是假绿，
// 直到新增 sessionOverrides 才暴露出来 —— 而它本该拦住的就是
// 「字段没下发到前端」这件事，漏掉一整类字段等于没有防护。
//
// 用「字段名 + 冒号」而不是裸的子串，是为了不被前缀相似的字段骗过：
// 实测把 maxSessionBytes 改名成 maxSessionBytesX 时，
// 裸 Contains("maxSessionBytes") 仍然为真，用例照样通过。
func bindingFieldRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\??\s*:`)
}

// 匹配规则本身也要锁住。
//
// 它的失效方式是**静默**的：正则漏掉某一类字段时，用例照样全绿，
// 只是从此不再保护那些字段 —— 比没有用例更糟，因为它给出的是假信心。
// 实测踩过一次（带 omitempty 的字段生成成 `name?: type`，
// 旧正则只认 `\s*:`，整类字段都匹配不上），所以这里直接对正则断言。
func TestBindingFieldReMatchesOptionalFields(t *testing.T) {
	cases := []struct{ name, line string }{
		{"maxSessionBytes", "    maxSessionBytes: number;"},
		// omitempty → 可选属性，名字与冒号之间多一个 `?`。
		{"sessionOverrides", "    sessionOverrides?: Record<string, SessionLimits>;"},
		{"sessionOverrides", "    sessionOverrides ?: Record<string, SessionLimits>;"},
	}
	for _, c := range cases {
		if !bindingFieldRe(c.name).MatchString(c.line) {
			t.Errorf("正则匹配不上 %s 的声明: %q", c.name, c.line)
		}
	}

	// 反过来必须能挡住前缀相似的字段，否则就退化成裸子串匹配了。
	if bindingFieldRe("maxSessionBytes").MatchString("    maxSessionBytesX: number;") {
		t.Error("不该匹配前缀相似的字段 maxSessionBytesX")
	}
}

// SessionInfo 是「会话列表」这个界面元素的唯一数据来源，
// 它的字段同样要跨过 Go 结构体 → Wails 绑定 → 前端 这条链。
//
// 漏字段的表现和策略字段一样是静默的：列表能渲染，但那一项是空的 ——
// 比如 isDefault 没下发，界面上「删除」按钮就不会对默认会话置灰，
// 用户点下去才吃一个后端报错。
//
// 字段清单从结构体反射得到，不手写（手写的话新增字段时容易忘了同步）。
func TestSessionInfoFieldsReachBindings(t *testing.T) {
	const modelsPath = "frontend/wailsjs/go/models.ts"
	b, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Skipf("未找到 %s（尚未生成绑定，跳过）: %v", modelsPath, err)
	}
	ts := string(b)

	// 绑定的类名带包名前缀（agent.SessionInfo）。只认这一段，
	// 免得拿别的类里的同名字段蒙混过关。
	start := strings.Index(ts, "export class SessionInfo")
	if start < 0 {
		start = strings.Index(ts, "SessionInfo {")
	}
	if start < 0 {
		t.Fatalf("%s 里没有 SessionInfo —— 绑定生成可能失败了，"+
			"或者类名变了（那会让本用例变成摆设）", modelsPath)
	}
	rest := ts[start:]
	if end := strings.Index(rest[1:], "export class "); end >= 0 {
		rest = rest[:end+1]
	}

	typ := reflect.TypeOf(agent.SessionInfo{})
	var missing []string
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		checked++
		if !bindingFieldRe(name).MatchString(rest) {
			missing = append(missing, name)
		}
	}
	if checked == 0 {
		t.Fatal("没有从 SessionInfo 反射出任何字段 —— 测试会静默失效")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("这些字段没有出现在 %s 的 SessionInfo 里：%v\n"+
			"后果是会话列表渲染出来是残缺的（静默失败）。"+
			"通常是绑定没重新生成（跑一次 wails build）。", modelsPath, missing)
	}
	if len(missing) == 0 {
		t.Logf("SessionInfo 的 %d 个字段都已出现在绑定中", checked)
	}
}

// 前后端的「默认会话 ID」必须是同一个字面量。
//
// 前端 store.js 里写死了 DEFAULT_SESSION（bucketKey 靠它把空会话 ID
// 归一化），后端写的是 agent.DefaultSessionID。两边没有任何编译器检查，
// 只能靠这条用例钉住。
//
// 漂移的后果很隐蔽：假设后端把常量改成 "main"，前端仍按 "default" 建桶，
// 那么「会话列表还没拉回来时产生的提示」会落进一个**谁也读不到**的桶 ——
// 用户看不到任何报错，只是有些消息凭空消失了。
func TestDefaultSessionIDMatchesFrontend(t *testing.T) {
	const storePath = "frontend/src/store.js"
	b, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", storePath, err)
	}
	// 要求是常量声明（const DEFAULT_SESSION = '...'），不是注释里的一串文字。
	re := regexp.MustCompile(`const\s+DEFAULT_SESSION\s*=\s*'([^']*)'`)
	m := re.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s 里没有找到 DEFAULT_SESSION 常量声明 —— 要么它被改名或删掉了，"+
			"要么本测试的正则已失效（那这条检查就成了摆设）", storePath)
	}
	if m[1] != agent.DefaultSessionID {
		t.Errorf("前后端的默认会话 ID 不一致：前端 %q，后端 %q。\n"+
			"两者必须相同 —— 否则前端按空会话 ID 建的桶与后端报回来的 ID 对不上，\n"+
			"那一批消息会落进一个谁也读不到的地方，且不报任何错。",
			m[1], agent.DefaultSessionID)
	}
}
