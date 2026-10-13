package agent

// 出网工具：fetch_webpage 与 search_web。
//
// 这两个工具给 Agent **新的能力**，因此也带来新的攻击面，边界必须说清楚：
//
//  1. 抓回来的网页内容是**不可信数据**（与远端主机输出同级），
//     调用方统一走 PrepareForLLM 的脱敏/注入检测/包裹管线。
//  2. 出网默认关闭：vault.PolicySettings.AllowWeb 零值即禁止。
//     老配置反序列化后天然安全，不需要迁移。
//  3. 这两个工具只**读**公网，不把本地/远端内容发出去。
//     刻意不做「POST 一段内容到某个 URL」这类工具 —— 那等于给模型
//     一条数据外传通道，而它正是提示注入最想达到的目的。
//  4. URL 限制为 http/https：file://、gopher:// 等 scheme 在本工具里
//     没有任何合理用途，却可能被用来读取本地文件。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ai-shell/internal/sshclient"
)

// fetchMaxBytes 是抓取单个网页保留的最大字节数。
//
// 与 MaxCaptureBytes（1 MiB）分开定：网页里一大半是导航、脚本、样式，
// 真正有用的正文通常几 KB 到几十 KB。取 256 KiB 既能覆盖绝大多数
// 文档页，又不会把一次抓取的上下文开销推到失控。
const fetchMaxBytes = 256 * 1024

// runFetchWebpage 抓取一个网页并抽出可读正文。
func runFetchWebpage(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	raw := strings.TrimSpace(strArg(args, "url"))
	if raw == "" {
		return sshclient.Result{}, fmt.Errorf("url 为空")
	}
	u, err := parseWebURL(raw)
	if err != nil {
		return sshclient.Result{}, err
	}

	if ctx.http == nil {
		return sshclient.Result{}, fmt.Errorf("出网能力未初始化")
	}

	start := time.Now()
	body, err := fetchURL(ctx.http, u)
	if err != nil {
		return sshclient.Result{}, err
	}

	text, title := extractReadable(body)
	var sb strings.Builder
	if title != "" {
		fmt.Fprintf(&sb, "标题: %s\n", title)
	}
	fmt.Fprintf(&sb, "来源: %s\n\n", u.String())
	sb.WriteString(text)
	if strings.TrimSpace(text) == "" {
		sb.WriteString("(未能从该页面提取出正文文本 —— 它可能是纯 JS 渲染的应用页，" +
			"或返回的不是 HTML。可以尝试提供更具体的 URL，或改用搜索。)")
	}

	return sshclient.Result{
		Stdout:     sb.String(),
		ExitCode:   0,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// parseWebURL 校验并规范化一个 URL。
//
// 只接受 http/https：其它 scheme 在本工具里没有合理用途，
// 而 file:// 之类的存在本身就是风险（读本地文件）。
func parseWebURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("URL 无法解析：%v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		// 模型经常直接给 "example.com/x"（省略 scheme）。
		// 补上 https 而不是报错 —— 那是一次可以省掉的往返。
		u, err = url.Parse("https://" + raw)
		if err != nil {
			return nil, fmt.Errorf("URL 无法解析：%v", err)
		}
		scheme = "https"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("只支持 http/https URL，实得 scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL 缺少主机名")
	}
	return u, nil
}

// fetchURL 发一次 GET 并读回正文。
//
// 三个细节：
//   - 显式设 User-Agent：不少站点对空 UA 直接返回 403，
//     而报错只会说「抓取失败」，看不出是 UA 的问题。
//   - 读的时候带上限：即使 Content-Length 撒谎或缺失，
//     也不会把一个大文件整个吞进内存。
//   - 不跟随跳转到非 http/https（net/http 自己会处理重定向，
//     这里只需要在拿到响应后确认最终 URL 的 scheme 仍合法）。
func fetchURL(client httpDoer, u *url.URL) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("构造请求失败：%v", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	if final := resp.Request.URL; final != nil {
		if s := strings.ToLower(final.Scheme); s != "http" && s != "https" {
			return "", fmt.Errorf("重定向到了不支持的 scheme：%s", final.Scheme)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 把状态码与原因都带上：404 与 403 对模型的下一步动作
		// 影响完全不同（前者该换 URL，后者该考虑是否需要登录）。
		return "", fmt.Errorf("服务器返回 %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// 限长读取：多读一个字节是为了检测「还有更多内容」，
	// 从而在结果里明确告知被截断了（见下）。
	limited := io.LimitReader(resp.Body, fetchMaxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("读取响应失败：%v", err)
	}
	if len(data) > fetchMaxBytes {
		return string(data[:fetchMaxBytes]) + fetchTruncatedNote, nil
	}
	return string(data), nil
}

// fetchUserAgent 是本工具抓网页时用的 UA。
//
// 用一个真实的浏览器串：很多站点（尤其是文档站）对空 UA 或
// 含 "Go-http-client" 的 UA 直接返回 403 或验证页。
const fetchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// fetchTruncatedNote 是抓取内容被截断时的说明。
//
// 必须明确说「后面的内容没拿到」：否则模型会把截断处当作页面结尾，
// 得出「这个页面就讲了这么多」的错误结论。
const fetchTruncatedNote = "\n…[内容过长，只保留了开头部分；后面的内容未获取]"

// ---- search_web ----

// runSearchWeb 调搜索服务并返回结果列表。
//
// 提供方通过 vault.PolicySettings.SearchProvider 选择。
// 未配置时返回一条**能指导用户去配置**的反馈，而不是干巴巴的「不可用」。
func runSearchWeb(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	query := strings.TrimSpace(strArg(args, "query"))
	if query == "" {
		return sshclient.Result{}, fmt.Errorf("query 为空")
	}
	if ctx.http == nil {
		return sshclient.Result{}, fmt.Errorf("出网能力未初始化")
	}

	pol := ctx.v.Policy()
	if !pol.AllowWeb {
		return sshclient.Result{}, fmt.Errorf("出网工具已被策略禁用（可在「安全策略」中开启「允许访问互联网」）")
	}
	if pol.SearchProvider == "" {
		return sshclient.Result{}, fmt.Errorf("尚未配置搜索服务。请到「安全策略」里选择搜索提供方；" +
			"若只需抓取已知 URL，可改用 fetch_webpage")
	}

	provider, ok := searchProviders[pol.SearchProvider]
	if !ok {
		return sshclient.Result{}, fmt.Errorf("未知的搜索提供方 %q（可用：%s）",
			pol.SearchProvider, searchProviderNames())
	}

	start := time.Now()
	out, err := provider.search(ctx.http, query, pol.SearchAPIKey)
	if err != nil {
		return sshclient.Result{}, err
	}
	if out == "" {
		out = fmt.Sprintf("搜索 %q 没有返回任何结果。可以换个关键词，或先用 fetch_webpage 直接访问已知页面。", query)
	}
	return sshclient.Result{
		Stdout:     out,
		ExitCode:   0,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// searchProvider 是一个搜索服务的适配器。
type searchProvider struct {
	// search 执行搜索并返回已格式化的结果文本。
	// apiKey 为空表示该服务不需要密钥。
	search func(client httpDoer, query, apiKey string) (string, error)
}

// searchProviders 是已支持的搜索服务表。
//
// 刻意只放「能用公开端点、或文档清晰的」服务：每多一个提供方
// 就多一套要维护的请求/解析代码，而它们的失败模式各不相同
// （限流、返回格式变更、需要登录）。
var searchProviders = map[string]searchProvider{
	// DuckDuckGo 的 HTML 端点：不需要 API Key，返回的是可解析的 HTML。
	// 选它作为默认提供方的理由正是「零配置可用」。
	"duckduckgo": {search: searchDuckDuckGo},
}

// searchProviderNames 返回可用的提供方名单，用在错误信息里。
func searchProviderNames() string {
	names := make([]string, 0, len(searchProviders))
	for k := range searchProviders {
		names = append(names, k)
	}
	sortStrings(names)
	return strings.Join(names, " / ")
}

// ddgMaxResults 是 DuckDuckGo 搜索返回的结果条数。
//
// 取 8 条：少于 5 常常漏掉真正相关的那条，多于 10 则后面几条
// 的信噪比已经很低，只是白白占用上下文。
const ddgMaxResults = 8

// searchDuckDuckGo 用 DuckDuckGo 的 HTML 端点搜索。
//
// 不申请 API Key 的代价：返回的是 HTML 而不是结构化 JSON，
// 需要自己解析。这里只取结果标题、链接与摘要三段 ——
// 它们在同一对标签里，用一个正则就能稳定取出，
// 不需要引入一个 HTML 解析依赖。
// endpoint 单独取一层而不是内联进 NewRequest：测试要能把它换掉
// （指向 httptest 服务器），否则这条路径只能在真网上跑 ——
// 那样测试会因限流、页面改版、网络不通而随机失败，等于没测。
func searchDuckDuckGo(client httpDoer, query, _ string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, ddgEndpoint(query), nil)
	if err != nil {
		return "", fmt.Errorf("构造搜索请求失败：%v", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("搜索请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("搜索服务返回 %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	limited := io.LimitReader(resp.Body, fetchMaxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("读取搜索结果失败：%v", err)
	}
	return parseDDGResults(string(data))
}

// ddgEndpoint 是 DuckDuckGo 搜索端点的构造函数。
//
// 抽成函数是为了让测试能替换真实地址（见 ddgEndpointOverride）：
// 直接写字面量会让搜索路径无法离线测试。
func ddgEndpoint(query string) string {
	base := ddgRealEndpoint
	if ddgEndpointOverride != "" {
		base = ddgEndpointOverride
	}
	return base + "?q=" + url.QueryEscape(query)
}

// ddgRealEndpoint 是 DuckDuckGo HTML 端点的真实地址。
const ddgRealEndpoint = "https://html.duckduckgo.com/html/"

// ddgEndpointOverride 非空时替换真实端点（仅测试使用）。
var ddgEndpointOverride string

// parseDDGResults 从 DuckDuckGo HTML 里抽出结果。
//
// 做法是**分别**收集标题链接与摘要，再按下标配对。
//
// 早先是一条正则同时抓三段（标题、链接、摘要按顺序出现）。那个写法
// 在真实页面上**匹配不到任何东西**：DDG 的 HTML 里摘要并不是紧跟在
// 标题的 </a> 后面，中间还隔着一层包裹节点。表现是「搜索永远没有结果」，
// 而它与「真的没有结果」在前端看起来一模一样 —— 一个纯粹的正则错误，
// 却伪装成网络或服务故障。
//
// 分开抓之后，标题与摘要按出现顺序配对：两者数量通常一致，
// 不一致时以标题为准（摘要缺失只损失一点上下文，标题缺失则整条结果无用）。
func parseDDGResults(html string) (string, error) {
	links := ddgLinkRe.FindAllStringSubmatch(html, -1)
	if len(links) == 0 {
		return "", nil
	}
	snips := ddgSnippetRe.FindAllStringSubmatch(html, -1)

	var sb strings.Builder
	count := 0
	for i, m := range links {
		if count >= ddgMaxResults {
			break
		}
		title := stripTags(decodeEntities(strings.TrimSpace(m[2])))
		link := decodeEntities(strings.TrimSpace(m[1]))
		if title == "" || link == "" {
			continue
		}
		// DuckDuckGo 的链接是跳转形式（/l/?uddg=...），
		// 解出真实目标再给模型 —— 否则它会拿着一个跳转链接去 fetch。
		link = unwrapDDGLink(link)
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", count+1, title, link)
		if i < len(snips) {
			if snip := strings.TrimSpace(snips[i][1]); snip != "" {
				fmt.Fprintf(&sb, "   %s\n", stripTags(decodeEntities(snip)))
			}
		}
		sb.WriteString("\n")
		count++
	}

	if count == 0 {
		return "", nil
	}
	return sb.String(), nil
}

// ddgLinkRe 匹配一条结果的链接与标题。
//
// 两组捕获：href、标题文本。用 [^>]* 而不是 .*? 来跨属性 ——
// 结果节点上常带 data-testid 等额外属性，.*? 在属性里意外出现 ">"
// 时会整体失配。
var ddgLinkRe = regexp.MustCompile(
	`(?is)<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)

// ddgSnippetRe 匹配一条结果的摘要文本。
//
// 摘要的标签形态在不同版本的页面上变过（<a> / <div>），所以这里只认
// class 而不认标签名。
var ddgSnippetRe = regexp.MustCompile(
	`(?is)class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</`)

// unwrapDDGLink 把 DuckDuckGo 的跳转链接解成真实目标。
//
// 形式是 `//duckduckgo.com/l/?uddg=<encoded>&rut=...`。
// 解不出来时原样返回 —— 一个跳转链接对模型仍有参考价值
// （它能看出是哪家站点），而为此丢掉整条结果更糟。
func unwrapDDGLink(link string) string {
	if !strings.Contains(link, "uddg=") {
		return link
	}
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return link
}

// stripTags 去掉 HTML 标签，只留文本。
//
// 刻意做得极简（不去解析 DOM、不处理嵌套）：这里处理的是搜索结果
// 的标题与摘要，它们是纯文本节点里套几个 <b> 高亮标签。
// 引入一个真正的 HTML 解析器来处理这种输入是过大的代价。
func stripTags(s string) string {
	var sb strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				sb.WriteRune(r)
			}
		}
	}
	return sb.String()
}

// decodeEntities 解开常见的 HTML 实体。
//
// 只处理搜索结果里真会出现的那几个：&amp; &lt; &gt; &quot; &#39; &nbsp;。
// 用 html.UnescapeString 会顺带处理几千个实体名，代价是多一次全表扫描 ——
// 对每次搜索都要跑几十遍的路径来说不必要。
func decodeEntities(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	repl := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&#x27;", "'",
		"&nbsp;", " ",
	)
	return repl.Replace(s)
}

// extractReadable 从一段 HTML 里抽出标题与可读正文。
//
// 做法是「按块丢弃 + 保空白压缩」：
//  1. 整段丢掉 <script>/<style>/<head> 里的内容（它们占了 HTML 的绝大部分体积）；
//  2. 把块级标签换成换行，让段落之间保留分隔（否则所有文字粘成一行）；
//  3. 去掉其余标签，只留文本；
//  4. 压缩连续空行。
//
// 刻意不做真正的「可读性算法」（Readability 那套按文本密度打分找正文）：
// 那需要解析 DOM 树，而这里连一个 HTML 解析依赖都没有引入。
// 对本工具的用途（看文档、看报错页、看 API 说明）来说，
// 上面四步拿到的文本已经足够，且实现与失败模式都可预测。
func extractReadable(html string) (text, title string) {
	// 标题单独取：它对模型判断「这个页面是不是我要的」极有价值，
	// 而丢掉 head 时顺手就没了。
	if m := titleRe.FindStringSubmatch(html); m != nil {
		title = strings.TrimSpace(decodeEntities(stripTags(m[1])))
	}

	s := html
	// 先摘掉整块无用内容。顺序无关（三类互不嵌套），但都要在
	// 标签替换**之前**做 —— 否则 script 里的字符串会被当成正文留下。
	s = scriptBlockRe.ReplaceAllString(s, " ")
	s = styleBlockRe.ReplaceAllString(s, " ")
	s = headBlockRe.ReplaceAllString(s, " ")

	// 块级标签 → 换行。这一步是「保住段落结构」的关键：
	// 直接去标签会让 <p>a</p><p>b</p> 变成 "ab"。
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = brTagRe.ReplaceAllString(s, "\n")

	s = stripTags(s)
	s = decodeEntities(s)
	// 压缩空行：上面把每个块级标签都换成了换行，连续标签会产生
	// 大片空行，而空行对模型没有信息量，却占上下文。
	s = multiNewlineRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s), title
}

var (
	// titleRe 取 <title>…</title>。
	titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

	// scriptBlockRe / styleBlockRe / headBlockRe 是三类要整段丢弃的内容。
	scriptBlockRe = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	styleBlockRe  = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	headBlockRe   = regexp.MustCompile(`(?is)<head\b.*?</head\s*>`)

	// blockTagRe 是块级标签：它们前后在渲染时各有一个换行。
	blockTagRe = regexp.MustCompile(`(?i)</?(?:p|div|section|article|header|footer|nav|aside|main|ul|ol|li|table|tr|td|th|h[1-6]|blockquote|pre|form|fieldset|figure|figcaption|dl|dt|dd)\b[^>]*>`)
	brTagRe    = regexp.MustCompile(`(?i)<br\s*/?>`)

	// multiNewlineRe 压缩三个及以上连续换行。
	multiNewlineRe = regexp.MustCompile(`\n{3,}`)
)

// strconv 的引用：searchProviderNames 之外暂无使用，保留 import 以便
// 将来按页数取结果时不必再改 import 块。
var _ = strconv.Itoa
