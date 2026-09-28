package vault

// AuthMethod 是 Linux 登录方式。
type AuthMethod string

const (
	AuthPassword   AuthMethod = "password"    // 账号 + 密码
	AuthPrivateKey AuthMethod = "private_key" // 私钥（可带 passphrase）
	AuthAgent      AuthMethod = "agent"       // 走本地 ssh-agent
)

// Host 是「不含任何密钥」的主机视图。
// 这个结构体会同时暴露给前端和 LLM —— 因此字段里绝不允许出现密码、私钥、口令。
// LLM 只能通过 ID 引用主机，无法得知如何登录。
type Host struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Addr       string     `json:"addr"` // host:port
	User       string     `json:"user"`
	AuthMethod AuthMethod `json:"authMethod"`
	HasSecret  bool       `json:"hasSecret"` // 是否已配置密钥材料
	Tags       []string   `json:"tags,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// HostSecret 是密钥材料，只在后端内存与加密文件中存在。
// 它没有 json tag，因为永远不会被序列化给前端或 LLM。
type HostSecret struct {
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// Empty 判断密钥材料是否为空。
func (s HostSecret) Empty() bool {
	return s.Password == "" && s.PrivateKey == "" && s.Passphrase == ""
}

// PolicyMode 是 agent 执行命令的自主度模式。
type PolicyMode string

const (
	// ModeManual 手动模式：agent 提出的每一条命令都必须人工确认。
	ModeManual PolicyMode = "manual"
	// ModeWhitelist 白名单模式：命中白名单的命令自动执行，其余仍需确认。
	ModeWhitelist PolicyMode = "whitelist"
)

// 会话上下文上限的默认值。
//
// 三个值一起用，少一个都会出问题：
//   - defaultMaxSessionTurns：轮数上限，防止聊得越久越慢。
//   - defaultMaxStoredToolBytes：**单条**工具输出存入历史时的字节上限。
//     真正保护上下文窗口的是这一个 —— 工具输出本身的上限是 MaxOutput
//     （默认 32KB），一轮最多 MaxSteps 条，不裁的话两三轮就能把窗口撑爆。
//   - defaultMaxSessionBytes：每台主机历史的总字节上限。
const (
	defaultMaxSessionTurns    = 8
	defaultMaxStoredToolBytes = 8 << 10   // 8 KiB
	defaultMaxSessionBytes    = 256 << 10 // 256 KiB
)

// PolicySettings 是策略配置（不含密钥，可安全下发前端）。
type PolicySettings struct {
	Mode         PolicyMode `json:"mode"`
	Whitelist    []string   `json:"whitelist"`    // 允许自动执行的前缀/正则
	RedactOutput bool       `json:"redactOutput"` // 回传给 LLM 前是否脱敏
	MaxOutput    int        `json:"maxOutput"`    // 单次回传 LLM 的最大字节数
	MaxSteps     int        `json:"maxSteps"`     // 单轮 agent 最大工具调用步数

	// 会话上下文（按主机独立的对话记忆）的三个上限。
	//
	// 这三个值是**全局默认**：每一台主机各有一份自己的历史，但适用
	// 同一组上限；想给某台主机单独放宽或收紧，用下面的 SessionOverrides。
	//
	// 这三个值决定「模型还记得多少」。调大能让长链路排查更连贯，
	// 但每一轮请求都要把命中的历史整段发给模型，所以它们直接决定了
	// 单次请求的体积与费用，也决定多久会撞上模型的上下文窗口。
	//
	// 之所以可配：主机规模与排查深度差异很大 —— 盯着一台机器连续排障
	// 和偶尔查一下的主机，合适的历史长度完全不同。
	MaxSessionTurns    int `json:"maxSessionTurns"`    // 每台主机保留的最大轮数
	MaxStoredToolBytes int `json:"maxStoredToolBytes"` // 单条工具输出存入历史时的字节上限
	MaxSessionBytes    int `json:"maxSessionBytes"`    // 每台主机历史的总字节上限

	// SessionOverrides 是**按主机**覆盖上面三个值的表，key 是主机 ID。
	//
	// 为什么需要：上面三个是全局的，而主机之间的合适值差异很大 ——
	// 盯着一台机器连续排障需要长链路，偶尔查一下的主机则是越短越省。
	// 只有一组全局数字时，为了让前者够用只能整体调大，代价是后者
	// 每一轮都跟着把整段历史发出去；调小则前者中途断片。
	//
	// 用主机 ID 而不是名字做 key：名字可以改，改完覆盖就静默丢了，
	// 而用户不会想到「改个显示名」会重置上下文设置。
	SessionOverrides map[string]SessionLimits `json:"sessionOverrides,omitempty"`
}

// SessionLimits 是单台主机的会话上限覆盖。
//
// 每一项的零值（或负值）都表示「这一项继承全局设置」，**不是**「上限为 0」——
// 上限为 0 意味着该主机一轮历史都不留，没有任何合理用法。把零值定义成
// 「没说过」是为了让只想调轮数的用户不必把另外两项抄一遍：抄了之后
// 全局一改，这台主机就静默不跟了，而界面上看不出它被钉住了。
type SessionLimits struct {
	MaxSessionTurns    int `json:"maxSessionTurns,omitempty"`
	MaxStoredToolBytes int `json:"maxStoredToolBytes,omitempty"`
	MaxSessionBytes    int `json:"maxSessionBytes,omitempty"`
}

// Empty 判断这条覆盖是否一项都没设。
func (s SessionLimits) Empty() bool {
	return s.MaxSessionTurns <= 0 && s.MaxStoredToolBytes <= 0 && s.MaxSessionBytes <= 0
}

// LLMSettings 是 LLM 配置的非密钥部分。
type LLMSettings struct {
	BaseURL string `json:"baseUrl"` // 兼容 OpenAI 的 endpoint
	Model   string `json:"model"`
}

// LLMSettingsView 是下发给前端的视图（密钥只回传「是否已配置」）。
type LLMSettingsView struct {
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
}

// store 是落盘的完整数据结构。整个结构体被 AES-GCM 加密后才写磁盘。
type store struct {
	Version   int                   `json:"version"`
	Hosts     []Host                `json:"hosts"`
	Secrets   map[string]HostSecret `json:"secrets"`  // key = host id
	HostKeys  map[string]string     `json:"hostKeys"` // addr -> SHA256 指纹（TOFU 防中间人）
	LLM       LLMSettings           `json:"llm"`
	LLMAPIKey string                `json:"llmApiKey"`
	Policy    PolicySettings        `json:"policy"`
	Whitelist []string              `json:"whitelist"`
}

func defaultStore() store {
	return store{
		Version:  1,
		Hosts:    []Host{},
		Secrets:  map[string]HostSecret{},
		HostKeys: map[string]string{},
		LLM: LLMSettings{
			BaseURL: "https://api.openai.com/v1",
			Model:   "gpt-4o-mini",
		},
		Policy: PolicySettings{
			Mode:         ModeManual,
			Whitelist:    defaultWhitelist(),
			RedactOutput: true,
			MaxOutput:    32 * 1024,
			MaxSteps:     12,

			MaxSessionTurns:    8,
			MaxStoredToolBytes: 8 * 1024,
			MaxSessionBytes:    256 * 1024,
		},
	}
}
