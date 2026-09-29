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

	// AllowCrossHost 决定 Agent 工具能否操作非本会话所属的主机。
	//
	// 工具的 host_id 是 LLM 自己填的，而它能看到全部已配置主机 —— 不设限的话，
	// 「在 A 机器排查问题的对话」可以悄悄把命令打到 B 机器上，那正是横向移动
	// 的形状。默认 false：本会话的工具只能打本会话的主机；打开后恢复
	// 「目标可以是任何已配置主机」，但每条命令仍要过策略与审批。
	//
	// 零值即禁止，老配置反序列化后天然安全 —— 宁可多弹一次说明，
	// 也不给一条用户不知道存在的跨机器通道。
	AllowCrossHost bool `json:"allowCrossHost"`

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

// LLMProfile 是一套可切换的 LLM 配置方案。
//
// API Key 不放在这里：密钥单独存在 LLMAPIKeys 里，与「不含密钥的视图」
// 天然隔离 —— 结构体本身就可以安全地下发前端。
type LLMProfile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
}

// LLMProfileView 是下发给前端的方案视图（密钥只回传「是否已配置」）。
type LLMProfileView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
	Active    bool   `json:"active"` // 是否为当前使用的方案
}

// LLMSettingsView 是当前激活方案的视图（密钥只回传「是否已配置」）。
// 多方案引入后它退化为「active 那一套」的便捷视图，保留是为了兼容既有前端。
type LLMSettingsView struct {
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	HasAPIKey bool   `json:"hasApiKey"`
}

// defaultLLMProfileID 是预置/迁移出来的那个方案的 ID。
// 固定值而不是随机值：迁移逻辑要靠它幂等，重复 Open 不产生重复方案。
const defaultLLMProfileID = "default"

// store 是落盘的完整数据结构。整个结构体被 AES-GCM 加密后才写磁盘。
//
// LLM 的多方案字段（LLMProfiles / LLMAPIKeys / ActiveLLM）与旧字段
// （LLM / LLMAPIKey）并存：旧字段只在「打开老配置文件时的一次性迁移」里
// 被读取，迁移后不再使用，但保留 JSON 形状让降级回老版本时不至于丢配置。
type store struct {
	Version   int                   `json:"version"`
	Hosts     []Host                `json:"hosts"`
	Secrets   map[string]HostSecret `json:"secrets"`   // key = host id
	HostKeys  map[string]string     `json:"hostKeys"`  // addr -> SHA256 指纹（TOFU 防中间人）
	LLM       LLMSettings           `json:"llm"`       // 旧字段：仅迁移时读取
	LLMAPIKey string                `json:"llmApiKey"` // 旧字段：仅迁移时读取

	LLMProfiles []LLMProfile      `json:"llmProfiles,omitempty"`
	LLMAPIKeys  map[string]string `json:"llmApiKeys,omitempty"` // profile id -> API Key
	ActiveLLM   string            `json:"activeLlm,omitempty"`  // 当前使用的方案 ID

	Policy    PolicySettings `json:"policy"`
	Whitelist []string       `json:"whitelist"`
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
		// 刻意不在这里预置方案：Open() 时若老 JSON 里没有 llmProfiles 键，
		// 预置值会在反序列化后残留，令迁移逻辑（LLMProfiles 非空则跳过）
		// 永远不触发。「首套方案」由 migrateLLMProfilesLocked 统一生成。
		LLMAPIKeys: map[string]string{},
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
