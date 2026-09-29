package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ai-shell/internal/agent"
	"ai-shell/internal/audit"
	"ai-shell/internal/llm"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// App 是暴露给前端的 Wails 绑定对象。
type App struct {
	ctx     context.Context
	v       *vault.Vault
	ssh     *sshclient.Client
	ag      *agent.Agent
	audit   *audit.Logger
	openErr error
	shell   shellGate   // 人工 shell 命令的审批闸门（与 Agent 工具审批分开）
	lane    sessionLane // Ask 与 RunShell 互斥道
}

// NewApp 创建应用实例。
func NewApp() *App {
	return &App{}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	dir, err := vault.DefaultDir()
	if err != nil {
		a.openErr = fmt.Errorf("定位配置目录失败: %w", err)
		return
	}
	a.v = vault.New(dir)
	if err := a.v.Open(); err != nil {
		a.openErr = err
		return
	}
	a.ssh = sshclient.New(a.v)
	a.ag = agent.New(a.v, a.ssh, a.emit)

	// 审计日志复用凭证库的主密钥 —— 记录里会出现命令原文，必须与凭证同级保护。
	if au, err := audit.New(a.v.Dir(), a.v); err == nil {
		a.audit = au
		a.ag.SetAuditor(au)
		// 走 auditLog 而不是直接 Append：这样「启动记录写失败」也会被告警，
		// 而不是像以前那样被 `_ =` 丢掉。
		a.auditLog(audit.Entry{Kind: audit.KindSystem, Note: "应用启动"})
	} else {
		a.openErr = fmt.Errorf("初始化审计日志失败: %w", err)
	}

	// 会话历史也复用同一把主密钥落盘。
	//
	// 载入失败**不阻断启动**：会话历史是优化项，不是必需品 ——
	// 文件坏了就当作没有历史，而不是让整个应用起不来。
	// 但要说出来：静默降级会让用户以为「历史还在」，重启后才发现没了。
	if err := a.ag.SetSessionStore(agent.NewSessionStore(a.v.Dir(), a.v)); err != nil {
		fmt.Fprintln(os.Stderr, "[sessions] 载入失败:", err)
		a.emit(agent.EvError, map[string]string{
			"message": "载入已保存的会话历史失败（已从空白开始，不影响使用）：" + err.Error(),
		})
	}
}

// emit 向前端发事件。ctx 尚未就绪时静默忽略（启动早期可能发生）。
func (a *App) emit(ev string, payload any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, ev, payload)
	}
}

// auditLog 写一条审计记录（audit 可能因初始化失败而为 nil）。
//
// 注意：不能吞掉写入错误。审计日志的价值全在于「完整且可校验」，
// 一旦写入失败却静默通过，用户会以为有完整轨迹。失败时发 audit:error 事件，
// 由界面给出持久告警；同时打到 stderr，便于从终端启动时也能看到。
func (a *App) auditLog(e audit.Entry) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Append(e); err != nil {
		fmt.Fprintln(os.Stderr, "[audit] 写入失败:", err)
		a.emit(agent.EvAuditError, map[string]string{
			"message": "审计日志写入失败，审计轨迹可能已不完整：" + err.Error(),
		})
	}
}

func (a *App) shutdown(_ context.Context) {
	// 会话在每次变更时就已经落盘了，这里是**兜底**而不是主路径：
	// 万一某条写入路径漏了 flush，关一次应用能把它补上。
	// 代价是退出时多写一次文件，可以忽略。
	if a.ag != nil {
		a.ag.FlushSessions()
	}
	if a.ssh != nil {
		a.ssh.Close()
	}
}

func (a *App) ready() error {
	if a.openErr != nil {
		return a.openErr
	}
	if a.v == nil {
		return fmt.Errorf("凭证库尚未初始化")
	}
	return nil
}

// ---- 启动信息 ----

// Posture 是安全态势，用于在 UI 上如实告知用户当前的真实保护等级。
type Posture struct {
	KeyProtection string `json:"keyProtection"` // os_keychain | key_file
	ConfigDir     string `json:"configDir"`
	HostCount     int    `json:"hostCount"`
	Degraded      bool   `json:"degraded"`
}

// BootstrapInfo 是前端启动时一次性拉取的全部状态。
type BootstrapInfo struct {
	Posture     Posture                `json:"posture"`
	Hosts       []vault.Host           `json:"hosts"`
	LLM         vault.LLMSettingsView  `json:"llm"`
	LLMProfiles []vault.LLMProfileView `json:"llmProfiles"`
	Policy      vault.PolicySettings   `json:"policy"`
	Error       string                 `json:"error"`
}

// Bootstrap 返回启动信息。
func (a *App) Bootstrap() BootstrapInfo {
	if err := a.ready(); err != nil {
		return BootstrapInfo{Error: err.Error()}
	}
	dir := a.v.Dir()
	prot := string(a.v.Protection())
	hosts := a.v.ListHosts()
	return BootstrapInfo{
		Posture: Posture{
			KeyProtection: prot,
			ConfigDir:     dir,
			HostCount:     len(hosts),
			Degraded:      a.v.Protection() == vault.ProtectionKeyFile,
		},
		Hosts:       hosts,
		LLM:         a.v.LLMSettingsView(),
		LLMProfiles: a.v.LLMProfiles(),
		Policy:      a.v.Policy(),
	}
}

// ---- 主机管理 ----

// ListHosts 返回主机列表（不含任何密钥材料）。
func (a *App) ListHosts() []vault.Host {
	if a.v == nil {
		return []vault.Host{}
	}
	return a.v.ListHosts()
}

// SaveHostRequest 是新增/编辑主机的入参。密钥字段为空表示「保持原值」。
type SaveHostRequest struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Addr        string   `json:"addr"`
	User        string   `json:"user"`
	AuthMethod  string   `json:"authMethod"`
	Note        string   `json:"note"`
	Tags        []string `json:"tags"`
	Password    string   `json:"password"`
	PrivateKey  string   `json:"privateKey"`
	Passphrase  string   `json:"passphrase"`
	ClearSecret bool     `json:"clearSecret"`
}

// SaveHost 新增或更新一台主机。
func (a *App) SaveHost(req SaveHostRequest) error {
	if err := a.ready(); err != nil {
		return err
	}
	id := strings.TrimSpace(req.ID)
	isNew := id == ""
	if isNew {
		id = newID()
	}

	host := vault.Host{
		ID:         id,
		Name:       strings.TrimSpace(req.Name),
		Addr:       strings.TrimSpace(req.Addr),
		User:       strings.TrimSpace(req.User),
		AuthMethod: vault.AuthMethod(req.AuthMethod),
		Note:       strings.TrimSpace(req.Note),
		Tags:       req.Tags,
	}

	var sec *vault.HostSecret
	if req.ClearSecret {
		sec = &vault.HostSecret{}
	} else if req.Password != "" || req.PrivateKey != "" || req.Passphrase != "" {
		sec = &vault.HostSecret{
			Password:   req.Password,
			PrivateKey: req.PrivateKey,
			Passphrase: req.Passphrase,
		}
	}

	if err := a.v.SaveHost(host, sec); err != nil {
		return err
	}
	if !isNew && a.ssh != nil {
		a.ssh.Disconnect(id) // 配置变了，断开旧连接
	}

	action := "更新主机"
	if isNew {
		action = "新增主机"
	}
	secretNote := "仅元数据（未改动密钥材料）"
	if sec != nil {
		if sec.Empty() {
			secretNote = "已清除密钥材料"
		} else {
			secretNote = "已写入密钥材料"
		}
	}
	// 注意：只记录登录方式与地址，绝不记录密码 / 私钥 / 口令原文
	a.auditLog(audit.Entry{
		Kind:     audit.KindHost,
		HostID:   id,
		HostName: host.Name,
		Command:  host.User + "@" + host.Addr,
		Note:     fmt.Sprintf("%s，登录方式 %s，%s", action, host.AuthMethod, secretNote),
	})
	return nil
}

// DeleteHost 删除主机。
func (a *App) DeleteHost(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	name := ""
	if h, ok := a.v.GetHost(id); ok {
		name = h.Name
	}
	if a.ssh != nil {
		a.ssh.Disconnect(id)
	}
	if err := a.v.DeleteHost(id); err != nil {
		return err
	}
	// 主机没了，会话上下文与对话记录都没了意义。Forget 是无条件的；
	// 若正好有 agent 在跑，那一轮结束后会重新建出一个空的 session —— 那时
	// 拿 hostID 去查就是空，行为退化成「该主机无会话」，没有错。
	a.ag.Forget(id)
	a.auditLog(audit.Entry{
		Kind: audit.KindHost, HostID: id, HostName: name,
		Note: "删除主机及其本地密钥材料",
	})
	return nil
}

// TestResult 是连通性测试结果。
type TestResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	DurationMs int64  `json:"durationMs"`
}

// TestHost 测试主机连通性。
func (a *App) TestHost(id string) TestResult {
	if err := a.ready(); err != nil {
		return TestResult{Message: err.Error()}
	}
	start := time.Now()
	res, err := a.ssh.Exec(id, "echo connected; uname -sr", 15*time.Second)
	if err != nil {
		return TestResult{Message: err.Error(), DurationMs: time.Since(start).Milliseconds()}
	}
	return TestResult{
		OK:         true,
		Message:    strings.TrimSpace(res.Stdout),
		DurationMs: time.Since(start).Milliseconds(),
	}
}

// ForgetHostKey 清除主机指纹，下次连接重新信任。
func (a *App) ForgetHostKey(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	h, ok := a.v.GetHost(id)
	if !ok {
		return fmt.Errorf("主机不存在")
	}
	return a.v.ForgetHostKey(h.Addr)
}

// ---- LLM 与策略 ----

// SaveLLMProfileRequest 是新增/编辑一套 LLM 方案的入参。
// ID 为空表示新增（后端生成 ID）；API Key 为空表示保持原值。
type SaveLLMProfileRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"`
	ClearKey bool   `json:"clearKey"`
}

// SaveLLMProfile 新增或更新一套 LLM 配置方案，返回保存后的视图
// （新增时前端需要它拿后端生成的 ID）。
func (a *App) SaveLLMProfile(req SaveLLMProfileRequest) (vault.LLMProfileView, error) {
	if err := a.ready(); err != nil {
		return vault.LLMProfileView{}, err
	}

	id := strings.TrimSpace(req.ID)
	isNew := id == ""
	if isNew {
		id = newID()
	}

	// 规范化后再落盘（同 SaveLLM：UI 刷新回来的就是能用的地址）。
	base := llm.NormalizeBaseURL(req.BaseURL)
	// 编辑时前端会把原值回填进表单，所以「空」只可能是用户真的清空了
	// 或新建没填 —— 此时存一套空地址的方案，问题要到发起对话时才暴露，
	// 且用户很难关联到是哪套方案。在这里拒绝，信息离错误最近。
	if strings.TrimSpace(base) == "" {
		return vault.LLMProfileView{}, fmt.Errorf("Base URL 不能为空（应填到 /v1 为止）")
	}

	var key *string
	if req.ClearKey {
		empty := ""
		key = &empty
	} else if strings.TrimSpace(req.APIKey) != "" {
		k := strings.TrimSpace(req.APIKey)
		key = &k
	}

	p := vault.LLMProfile{ID: id, Name: req.Name, BaseURL: base, Model: req.Model}
	if err := a.v.SaveLLMProfile(p, key); err != nil {
		return vault.LLMProfileView{}, err
	}

	note := "API Key 未改动"
	if req.ClearKey {
		note = "已清除 API Key"
	} else if key != nil {
		note = "已更新 API Key"
	}
	action := "更新 LLM 方案"
	if isNew {
		action = "新增 LLM 方案"
	}
	// 只记地址与模型名，绝不记 Key 原文。
	a.auditLog(audit.Entry{
		Kind:    audit.KindLLM,
		Command: base + " / " + req.Model,
		Note:    fmt.Sprintf("%s「%s」（%s）", action, strings.TrimSpace(req.Name), note),
	})

	// 从 LLMProfiles 里取回保存后的视图：Active 标记由 vault 的有效激活规则
	// 决定（比如新增的第一套方案会自动激活），这里不自己推一遍。
	for _, v := range a.v.LLMProfiles() {
		if v.ID == id {
			return v, nil
		}
	}
	return vault.LLMProfileView{ID: id, Name: strings.TrimSpace(req.Name), BaseURL: base, Model: req.Model}, nil
}

// DeleteLLMProfile 删除一套 LLM 配置方案（至少保留一套，由 vault 把关）。
func (a *App) DeleteLLMProfile(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	name := ""
	if p, ok := a.v.LLMProfile(id); ok {
		name = p.Name
	}
	if err := a.v.DeleteLLMProfile(id); err != nil {
		return err
	}
	a.auditLog(audit.Entry{
		Kind: audit.KindLLM,
		Note: fmt.Sprintf("删除 LLM 方案「%s」", name),
	})
	return nil
}

// ActivateLLMProfile 切换当前使用的方案。下一次对话即用新方案，无需重启。
func (a *App) ActivateLLMProfile(id string) error {
	if err := a.ready(); err != nil {
		return err
	}
	name := ""
	if p, ok := a.v.LLMProfile(id); ok {
		name = p.Name
	}
	if err := a.v.ActivateLLMProfile(id); err != nil {
		return err
	}
	if name != "" {
		a.auditLog(audit.Entry{
			Kind: audit.KindLLM,
			Note: fmt.Sprintf("切换 LLM 方案到「%s」", name),
		})
	}
	return nil
}

// LLMTestResult 是「测试模型配置」的结果。
type LLMTestResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	URL        string `json:"url"`    // 实际请求的地址，便于用户对照着改
	Status     int    `json:"status"` // HTTP 状态码；0 = 没连上
	Model      string `json:"model"`
	Reply      string `json:"reply"`
	DurationMs int64  `json:"durationMs"`
}

// llmTestTimeout 是「测试连接」的超时。
//
// 之所以做成变量而不是直接写 20*time.Second：测试需要验证这个 deadline
// **确实**在生效，但又不能让测试套件真的干等 20 秒。
var llmTestTimeout = 20 * time.Second

// resolveLLMConfig 决定「测试连接」实际使用哪一组配置。
//
// 表单值优先 —— 这是「先测再存」成立的前提；留空表示沿用已保存的值。
//
// 抽出成纯函数是为了能直接测优先级规则：这段逻辑（谁覆盖谁）比它看起来
// 更容易写错，而它藏在 TestLLM 里时只能靠构造整个 App 才能测到。
func resolveLLMConfig(formBase, formModel, formKey string, saved vault.LLMSettings, savedKey string) (base, model, key string) {
	base = strings.TrimSpace(formBase)
	if base == "" {
		base = saved.BaseURL
	}
	// 规范化放在**回落之后**统一做。
	// 只规范化表单分支会让两条路径行为不一致：历史配置里存着
	// ".../v1/chat/completions" 时，实际请求会被 llm.New 修好，
	// 但错误提示里显示的地址却是坏的 —— 用户照着改反而改错。
	base = llm.NormalizeBaseURL(base)

	model = strings.TrimSpace(formModel)
	if model == "" {
		model = saved.Model
	}

	key = strings.TrimSpace(formKey)
	if key == "" {
		key = savedKey
	}
	return base, model, key
}

// TestLLM 真的发一次最小请求，验证当前配置能不能用。
//
// 参数是**表单里的值**而不是已保存的值，这样「先测再存」是可行的；
// 传空串表示「用已保存的」。API Key 留空时用钥匙串里那把。
//
// 为什么不做成「只校验配置形状」：形状对但地址错恰恰是最常见的情况
// （把完整端点填进 Base URL 就是），只有真发一次请求才测得出来。
// 之前那个「检查配置」按钮只查了有没有 API Key，然后让人自己去控制台试 ——
// 那种按钮会给人已经验过的错觉，比没有更糟。
func (a *App) TestLLM(baseURL, model, apiKey string) LLMTestResult {
	if err := a.ready(); err != nil {
		return LLMTestResult{Message: err.Error()}
	}

	saved, savedKey := a.v.LLM()
	return a.runLLMTest(saved, savedKey, baseURL, model, apiKey)
}

// TestLLMProfile 测试一套指定方案的连通性，空字段回落到该方案已保存的值
// （而不是当前激活方案的值 —— 编辑非激活方案时两者不同）。
// profileID 为空时行为与 TestLLM 一致；ID 不存在时显式报错而不是静默回落 ——
// 回落会让用户拿到「另一套方案」的测试结论，比失败更误导。
func (a *App) TestLLMProfile(profileID, baseURL, model, apiKey string) LLMTestResult {
	if err := a.ready(); err != nil {
		return LLMTestResult{Message: err.Error()}
	}

	saved, savedKey := a.v.LLM()
	if id := strings.TrimSpace(profileID); id != "" {
		p, ok := a.v.LLMProfile(id)
		if !ok {
			return LLMTestResult{Message: "该方案已不存在（可能已被删除），请刷新后重试。"}
		}
		saved = vault.LLMSettings{BaseURL: p.BaseURL, Model: p.Model}
		savedKey = a.v.LLMKey(id)
	}
	return a.runLLMTest(saved, savedKey, baseURL, model, apiKey)
}

// runLLMTest 是 TestLLM / TestLLMProfile 共用的执行段：
// 解析「表单优先、已保存回落」，然后真发一次最小请求。
func (a *App) runLLMTest(saved vault.LLMSettings, savedKey, formBase, formModel, formKey string) LLMTestResult {
	base, m, key := resolveLLMConfig(formBase, formModel, formKey, saved, savedKey)

	// 防御性检查：正常流程下走不到这里 —— vault 预置了默认值，
	// 且 SetLLM 忽略空串（无法把 Base URL 清空）。
	// 但配置文件被手工改坏 / 版本迁移出问题时，这行能挡住一个空地址请求。
	if base == "" || m == "" {
		return LLMTestResult{Message: "请先填写 Base URL 与模型名。"}
	}

	// 单独给一个较短的超时：连通性测试不该让用户等三分钟。
	// 客户端本身的 180s 超时是给正常对话用的（长回答要慢慢流）。
	ctx, cancel := context.WithTimeout(context.Background(), llmTestTimeout)
	defer cancel()

	res, err := llm.New(base, key, m).Ping(ctx)

	out := LLMTestResult{
		URL:        res.URL,
		Status:     res.Status,
		Model:      m,
		Reply:      res.Reply,
		DurationMs: res.Elapsed.Milliseconds(),
	}
	if err != nil {
		out.Message = diagnoseLLMError(err, res.Status, base, key != "")
		return out
	}
	out.OK = true
	out.Message = "连接成功"
	return out
}

// diagnoseLLMError 把底层错误翻译成用户能照着改的话。
//
// 直接把 "LLM 返回 404: {"error":{"message":"not found"}}" 摆给用户是没用的 ——
// 他看不出问题出在地址还是密钥。这里按状态码给出具体该改什么。
//
// hasKey 用来区分两种 401：压根没配密钥 vs 配了但不对。
// 前者是最常见的首次失败（默认 Base URL 指向 OpenAI，用户还没填 Key），
// 这时说「API Key 不正确」会让人以为是自己填错了 —— 而他根本没填。
func diagnoseLLMError(err error, status int, base string, hasKey bool) string {
	switch status {
	case http.StatusNotFound:
		return fmt.Sprintf("地址不对（404）。Base URL 应填到 /v1 为止，不要带 /chat/completions。"+
			"实际请求的是：%s/chat/completions", base)
	case http.StatusUnauthorized, http.StatusForbidden:
		if !hasKey {
			return "认证失败（" + strconv.Itoa(status) + "）：还没有配置 API Key。" +
				"如果这个地址不需要密钥，请确认它确实开放；否则请填入 API Key 后再测试。"
		}
		return "认证失败（" + strconv.Itoa(status) + "）：API Key 不正确、已过期，或没有访问该模型的权限。"
	case http.StatusTooManyRequests:
		return "被限流（429）：稍后重试，或检查账户额度。"
	case http.StatusBadRequest:
		return "请求被拒绝（400）：多半是模型名写错了。原始信息：" + err.Error()
	}
	if status >= 500 {
		return fmt.Sprintf("服务端错误（%d）：这是对方服务的问题，不是你的配置。原始信息：%s", status, err.Error())
	}
	if status == 0 {
		if strings.Contains(err.Error(), "context deadline exceeded") {
			return "连接超时（20 秒）：地址可能不可达，或需要走代理。"
		}
		return "连不上这个地址：检查网络、端口，以及地址是否写对。原始信息：" + err.Error()
	}
	return err.Error()
}

// DefaultWhitelist 返回内置白名单，供 UI「恢复默认」使用。
func (a *App) DefaultWhitelist() []string {
	return vault.DefaultWhitelist()
}

// SavePolicy 保存策略配置。
func (a *App) SavePolicy(p vault.PolicySettings) error {
	if err := a.ready(); err != nil {
		return err
	}
	if err := a.v.SetPolicy(p); err != nil {
		return err
	}
	// 记的是**存进去的那份**而不是入参：SetPolicy 会把非正值补成默认值，
	// 拿入参记会和实际生效的值对不上（用户传 0，日志显示 0，实际却在用 8）。
	// 覆盖台数同理取 eff：入参里那些三项全空的条目会被规整掉，
	// 记入参的话日志会显示一个比实际多的数字。
	eff := a.v.Policy()
	a.auditLog(audit.Entry{
		Kind:    audit.KindPolicy,
		Command: string(eff.Mode),
		Note: fmt.Sprintf("白名单 %d 条，输出脱敏 %v，单轮最大步数 %d，跨主机执行 %v，上下文 %d 轮（单条工具输出上限 %dKiB，总量上限 %dKiB，%d 台主机有独立上限）",
			len(eff.Whitelist), eff.RedactOutput, eff.MaxSteps, eff.AllowCrossHost,
			eff.MaxSessionTurns, eff.MaxStoredToolBytes/1024, eff.MaxSessionBytes/1024,
			len(eff.SessionOverrides)),
	})
	return nil
}

// ---- 控制台 ----

// ---- 交互终端 ----
//
// 申请伪终端、跑一个常驻 shell，输出走事件流、按键实时转发。
// 人工「一条命令一次 Exec」的直连入口已去掉：交互终端是其超集。
//
// 它**完全绕过策略引擎**（人是所有者），所以界面必须把这件事说清楚。
// 审计只记会话的开始与结束，不逐键记录 —— 逐键记录会把审计日志变成
// 一份键盘记录，既没有用，又平白多存一份敏感数据（包括所有密码）。

// 事件名。输出用 base64 而不是裸字符串，理由见 OpenTerminal。
const (
	EvTermData = "term:data"
	EvTermExit = "term:exit"
)

// termDataPayload 构造 term:data 的事件载荷。
//
// 抽成纯函数是为了能直接断言编码结果。载荷一旦编错，前端拿到的是乱码或
// 丢数据，而那种 bug 从界面上几乎定位不回来 —— 你只会看到「终端里偶尔
// 出现问号」，既复现不了，也想不到是后端编码的问题。
func termDataPayload(hostID string, b []byte) map[string]string {
	return map[string]string{
		"hostId": hostID,
		"data":   base64.StdEncoding.EncodeToString(b),
	}
}

// OpenTerminal 打开（或复用）某台主机的交互终端。
//
// 不返回输出：终端是一条**持续的流**，用「一次调用一次结果」建模会把实时性
// 整个丢掉。输出通过 term:data 事件推送，退出通过 term:exit。
//
// 已经开着活终端时复用而不是重开 —— 界面在切模式、切主机时会反复调这里，
// 而用户 cd 过去的目录、跑着的进程都在那条会话里。
func (a *App) OpenTerminal(hostID string, cols, rows int) error {
	if err := a.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(hostID) == "" {
		return errors.New("请先选择一台主机")
	}

	before := a.ssh.PTY(hostID)
	p, err := a.ssh.OpenPTY(hostID, "xterm-256color", sshclient.TerminalSize{Cols: cols, Rows: rows},
		func(b []byte) {
			// base64 而不是字符串：一次读取完全可能把一个多字节 UTF-8 字符
			// 切成两半（远端一次 write 的边界和字符边界无关）。当成字符串发，
			// 前端就会把半个字符渲染成乱码。交给 xterm.js 的 UTF-8 解码器去
			// 处理跨块的续字节 —— 它本来就是干这个的。
			a.emit(EvTermData, termDataPayload(hostID, b))
		},
		func(reason string) {
			a.emit(EvTermExit, map[string]string{"hostId": hostID, "reason": reason})
			a.auditLog(audit.Entry{
				Kind:     audit.KindTerminal,
				HostID:   hostID,
				HostName: hostNameOf(a.v, hostID),
				Decision: "human",
				Rule:     "pty",
				Note:     "交互终端已结束：" + reason,
			})
		})
	if err != nil {
		return err
	}

	// 只有**真的新开了一条**才记审计。复用时不记：界面每次切回终端都会调
	// 这里，照记的话审计里会堆满「打开了终端」而用户其实一次都没开过新的，
	// 反而把真正的那一次淹没掉。
	if p != before {
		a.auditLog(audit.Entry{
			Kind:     audit.KindTerminal,
			HostID:   hostID,
			HostName: hostNameOf(a.v, hostID),
			Decision: "human",
			Rule:     "pty",
			Note:     "打开了交互终端（不经过策略引擎，命令以登录用户身份直接执行）",
		})
	}
	return nil
}

// WriteTerminal 把用户的按键转发到远端。data 是 base64 编码的原始字节。
//
// 用 base64 而不是字符串：中文输入法、粘贴进来的内容都要按原始字节送，
// 中间任何一次「字符串 ↔ 字节」的转换都可能引入替换字符，
// 而用户看到的是自己敲的字变成了问号。
func (a *App) WriteTerminal(hostID, data string) error {
	if err := a.ready(); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("按键数据解码失败: %w", err)
	}
	p := a.ssh.PTY(hostID)
	if p == nil {
		return errors.New("交互终端没有打开")
	}
	// 终端已经关了还收到按键（用户手快）不是错误，忽略即可 ——
	// 弹一条报错只会让用户以为出了问题。
	if err := p.Write(raw); err != nil && !errors.Is(err, sshclient.ErrPTYClosed) {
		return err
	}
	return nil
}

// ResizeTerminal 上报终端尺寸变化。
//
// 少了这一步，远端程序会一直以为自己还是初始尺寸：top 的进程列表挤在
// 左边一小条里，vim 画错半屏。窗口尺寸不是装饰，是程序的排版依据。
func (a *App) ResizeTerminal(hostID string, cols, rows int) error {
	if err := a.ready(); err != nil {
		return err
	}
	p := a.ssh.PTY(hostID)
	if p == nil {
		return nil // 还没打开就谈不上调整，静默忽略
	}
	if err := p.Resize(sshclient.TerminalSize{Cols: cols, Rows: rows}); err != nil &&
		!errors.Is(err, sshclient.ErrPTYClosed) {
		return err
	}
	return nil
}

// CloseTerminal 关掉某台主机的交互终端。
//
// 返回 false 表示本来就没开着 —— 那不是错误，用户按晚了而已。
func (a *App) CloseTerminal(hostID string) bool {
	if a.ssh == nil {
		return false
	}
	if !a.ssh.ClosePTY(hostID) {
		return false
	}
	a.auditLog(audit.Entry{
		Kind:     audit.KindTerminal,
		HostID:   hostID,
		HostName: hostNameOf(a.v, hostID),
		Decision: "human",
		Rule:     "pty",
		Note:     "关闭了交互终端",
	})
	return true
}

// ---- 会话（每台主机多条）----
//
// 这组绑定只做「转发 + 就绪检查」，业务规则全在 agent 层 ——
// 尤其是「默认会话不能删」这条不变式，它必须只有一处实现。
// 在这里再判一次的话，两边迟早会分叉，而分叉的表现是
// 「界面允许删、后端拒绝」，用户只会看到一句没头没尾的报错。

// ListSessions 列出某台主机的全部会话。
//
// 默认会话**总在**列表里，哪怕它还没有任何内容 —— 它是「不带会话 ID
// 直接提问」时的落点，界面必须能让用户看见并选中它，
// 否则用户的第一段对话会变成看不见也清不掉的幽灵。
func (a *App) ListSessions(hostID string) []agent.SessionInfo {
	if err := a.ready(); err != nil {
		return nil
	}
	return a.ag.ListSessions(hostID)
}

// CreateSession 在某台主机下新建一条命名会话。
func (a *App) CreateSession(hostID, name string) (agent.SessionInfo, error) {
	if err := a.ready(); err != nil {
		return agent.SessionInfo{}, err
	}
	return a.ag.CreateSession(hostID, name)
}

// RenameSession 改会话名。默认会话**也可以**改名 ——
// 它由 ID 认定而不是由名字认定，改成「nginx 排查」之后它仍然是兜底的那条。
func (a *App) RenameSession(hostID, sessionID, name string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return a.ag.RenameSession(hostID, sessionID, name)
}

// DeleteSession 删掉一条会话。默认会话删不掉，要清空它请用 ClearSession。
func (a *App) DeleteSession(hostID, sessionID string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return a.ag.DeleteSession(hostID, sessionID)
}

// Ask 让 LLM agent 在指定会话上开始一轮工作（异步，结果通过事件推送）。
//
// sessionID 传空字符串表示「默认会话」—— 调用方不必知道默认会话的 ID 叫什么，
// 也就不会出现「前端写死了一个 ID、后端改了常量」这种静默错位。
func (a *App) Ask(hostID, sessionID, prompt string) error {
	if err := a.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("请输入内容")
	}
	if a.ag == nil {
		return fmt.Errorf("Agent 尚未初始化")
	}
	// 先占 lane，再查 Running：避免「查 Running 通过 → shell 插入 → 再占 lane」的窗口，
	// 也避免双 Ask 都过 Running 检查后抢 Run。
	if ok, reason := a.lane.tryEnterAgent(); !ok {
		return fmt.Errorf("%s", reason)
	}
	if a.ag.Running() {
		a.lane.leaveAgent()
		return fmt.Errorf("已有会话正在运行")
	}
	go func() {
		defer a.lane.leaveAgent()
		_ = a.ag.Run(a.ctx, hostID, sessionID, prompt)
	}()
	return nil
}

// ClearSessionResult 是清空会话上下文的结果。
type ClearSessionResult struct {
	Cleared int  `json:"cleared"` // 被丢弃的轮数
	Busy    bool `json:"busy"`    // 有会话正在运行，未清空
}

// ClearSession 清空某条会话的 LLM 上下文。sessionID 传空表示默认会话。
//
// 清空会**立即落盘**（见 agent.ClearSession）：不落的话，重启后
// 被清掉的上下文会复活，用户看到模型又记得了，只会以为按钮没生效。
//
// 清空的粒度是**一条会话**而不是一台主机：用户完全可能想留住
// 「nginx 排查」那段上下文，只把另一条试验性的对话清掉。
//
// 返回轮数而不是 bool，是为了让界面能区分「清掉了 3 轮」和「本来就没有上下文」——
// 否则用户点了没反应只会以为按钮坏了。
//
// 运行中拒绝清空（busy=true）。这个判断**在 agent 的锁里做**，不在这里先查
// Running() 再调 ClearSession —— 那样中间会有一个窗口让另一轮 Run 读走历史，
// 界面却已经报了「已清空」。
func (a *App) ClearSession(hostID, sessionID string) ClearSessionResult {
	if err := a.ready(); err != nil {
		return ClearSessionResult{}
	}
	cleared, busy := a.ag.ClearSession(hostID, sessionID)
	return ClearSessionResult{Cleared: cleared, Busy: busy}
}

// CompactSession 让模型把某条会话的历史压缩成一段摘要。sessionID 传空表示默认会话。
//
// 与 ClearSession 的区别：清空是**丢掉**记忆，压缩是**换一种更省的方式留着**。
// 用户排查链路很长时，压缩比清空合适得多。
//
// 这里返回 (结果, error) 而不是像 ClearSession 那样只返回结果：
// 「正忙」「没得压」是预期内的状态（走结果对象），而「没配 API Key」
// 「模型调用失败」是真故障（走 error）。前端据此区分提示与报错 ——
// 把「没有可压缩的内容」渲染成一条红色错误是误导。
func (a *App) CompactSession(hostID, sessionID string) (agent.CompactResult, error) {
	if err := a.ready(); err != nil {
		return agent.CompactResult{}, err
	}
	return a.ag.CompactSession(a.ctx, hostID, sessionID)
}

// Approve 回应一次审批请求（Agent 工具或人工 shell，先试 Agent）。
func (a *App) Approve(id string, approved bool) bool {
	if a.ag != nil && a.ag.Resolve(id, approved) {
		return true
	}
	return a.resolveShell(id, approved)
}

// Stop 中断当前 agent 会话，取消挂起的人工 shell 审批，并杀掉本轮 shell 的 Exec。
func (a *App) Stop() {
	if a.ag != nil {
		a.ag.Stop()
	}
	a.cancelShellApprovals()
	// 只 Cancel 当前人工 shell 占用的那台主机，避免误杀其它主机会话。
	if a.ssh != nil {
		if id := a.lane.shellHostID(); id != "" {
			a.ssh.Cancel(id)
		}
	}
}

// ---- 审计日志 ----

// AuditView 是给前端的审计记录视图。
type AuditView struct {
	Entries  []audit.Entry `json:"entries"`
	Size     int64         `json:"size"`
	Segments int           `json:"segments"`
	Degraded bool          `json:"degraded"`
	Error    string        `json:"error"`
}

// ListAudit 返回最近 limit 条审计记录。
func (a *App) ListAudit(limit int) AuditView {
	if err := a.ready(); err != nil {
		return AuditView{Error: err.Error()}
	}
	if a.audit == nil {
		return AuditView{Error: "审计日志未初始化", Degraded: true}
	}
	if limit <= 0 {
		limit = 300
	}
	entries, err := a.audit.Recent(limit)
	if err != nil {
		return AuditView{Error: err.Error(), Degraded: true}
	}
	// Size 是所有段的总和（用户视角的「日志大小」），Segments 用于提示已发生轮转。
	return AuditView{Entries: entries, Size: a.audit.Size(), Segments: a.audit.Segments()}
}

// VerifyAudit 校验审计日志的哈希链完整性 —— 用于确认历史记录未被删改。
func (a *App) VerifyAudit() audit.VerifyResult {
	if a.audit == nil {
		return audit.VerifyResult{OK: false, Message: "审计日志未初始化"}
	}
	return a.audit.Verify()
}

// ExportAudit 把审计日志解密导出为明文 JSONL。
// 这是一次显式的用户动作：明文只写到用户选定位置，默认目录里始终保持密文。
func (a *App) ExportAudit() (string, error) {
	if a.audit == nil {
		return "", fmt.Errorf("审计日志未初始化")
	}
	dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "导出审计日志（明文）",
		DefaultFilename: fmt.Sprintf("ai-shell-audit-%s.jsonl", time.Now().Format("20060102-150405")),
		Filters: []runtime.FileFilter{
			{DisplayName: "JSON Lines (*.jsonl)", Pattern: "*.jsonl"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if dest == "" {
		return "", nil // 用户取消
	}
	n, err := a.audit.ExportPlaintext(dest)
	if err != nil {
		return "", err
	}
	a.auditLog(audit.Entry{
		Kind: audit.KindSystem,
		Note: fmt.Sprintf("导出审计日志 %d 条到 %s", n, dest),
	})
	return dest, nil
}

// ---- 工具 ----

func hostNameOf(v *vault.Vault, id string) string {
	if h, ok := v.GetHost(id); ok {
		return h.Name
	}
	return ""
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("h%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
