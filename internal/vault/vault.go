package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// KeyProtection 描述主密钥被什么保护着 —— 会显示在 UI 上，让用户知道真实安全等级。
type KeyProtection string

const (
	// ProtectionKeychain 主密钥托管在操作系统钥匙串里（Windows 凭据管理器 /
	// macOS Keychain / Linux libsecret）。同机其他软件要拿到它，必须能调用
	// 同一用户的钥匙串 —— 这正是「防止别的软件分析」想要的强度。
	ProtectionKeychain KeyProtection = "os_keychain"
	// ProtectionKeyFile 降级方案：钥匙串不可用时，主密钥落到一个 0600 的文件里。
	// 安全等级明显更低，UI 必须显式警告。
	ProtectionKeyFile KeyProtection = "key_file"
)

const (
	keyringService = "AiShell"
	keyringUser    = "vault-master-key"
	keySize        = 32 // AES-256
	vaultFileName  = "vault.enc"
	keyFileName    = "master.key"
)

// ErrLocked 表示 vault 尚未 Open。
var ErrLocked = errors.New("vault: 未解锁")

// Vault 是加密凭证库。所有密钥材料只在后端内存中出现。
type Vault struct {
	mu         sync.RWMutex
	dir        string
	key        []byte
	protection KeyProtection
	st         store
	opened     bool
}

// New 创建一个指向 dir 的 Vault（尚未解锁）。
func New(dir string) *Vault {
	return &Vault{dir: dir}
}

// DefaultDir 返回默认的配置目录。
//
// 依次尝试：系统配置目录 → 用户家目录 → 可执行文件同级目录。
// 之所以要有降级链：os.UserConfigDir() 在 %AppData% 缺失的环境（精简容器、
// 特殊启动方式）会直接报错，导致整个应用不可用。
func DefaultDir() (string, error) {
	if base, err := os.UserConfigDir(); err == nil && base != "" {
		return filepath.Join(base, "Ai-Shell"), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".ai-shell"), nil
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return filepath.Join(filepath.Dir(exe), "ai-shell-config"), nil
	}
	return "", errors.New("无法定位配置目录：系统配置目录、家目录、可执行文件路径均不可用")
}

// Open 加载主密钥并解密凭证库。首次运行会自动生成主密钥。
func (v *Vault) Open() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	key, prot, err := v.loadOrCreateKey()
	if err != nil {
		return err
	}
	v.key = key
	v.protection = prot

	v.st = defaultStore()

	blob, err := os.ReadFile(v.path(vaultFileName))
	if err != nil {
		if os.IsNotExist(err) {
			v.opened = true
			// 全新安装也要走一次迁移：defaultStore 里预置的旧字段
			// （llm / llmApiKey）正是靠它变成第一套方案的。
			v.migrateLLMProfilesLocked()
			return v.saveLocked() // 落一个初始文件
		}
		return fmt.Errorf("读取凭证库失败: %w", err)
	}

	plain, err := decrypt(v.key, blob)
	if err != nil {
		return fmt.Errorf("解密凭证库失败（主密钥可能已变更）: %w", err)
	}
	if err := json.Unmarshal(plain, &v.st); err != nil {
		return fmt.Errorf("解析凭证库失败: %w", err)
	}
	if v.st.Secrets == nil {
		v.st.Secrets = map[string]HostSecret{}
	}
	if v.st.HostKeys == nil {
		v.st.HostKeys = map[string]string{}
	}
	if v.st.LLMAPIKeys == nil {
		v.st.LLMAPIKeys = map[string]string{}
	}
	if v.st.Policy.Mode == "" {
		v.st.Policy = defaultStore().Policy
	}
	v.migrateLLMProfilesLocked()
	v.opened = true
	return nil
}

// migrateLLMProfilesLocked 把旧版单套 LLM 配置迁移成多方案格式（须持锁）。
//
// 触发条件：还没有任何方案，但旧字段里有配置。迁移是幂等的 ——
// 一旦 LLMProfiles 非空就什么都不做，重复 Open 不会产生重复方案。
// ID 固定为 defaultLLMProfileID，这样「迁移 → 删掉旧字段 → 再迁移」
// 这种异常序列也只会得到一个方案。
//
// 迁移后旧字段不再使用（但保留在 JSON 里，见 store 的注释）。
// 这里刻意不立即落盘：Open 是热路径，等下一次任何写入顺手把新格式带上即可。
func (v *Vault) migrateLLMProfilesLocked() {
	if len(v.st.LLMProfiles) != 0 {
		return
	}
	if v.st.LLM.BaseURL == "" && v.st.LLMAPIKey == "" {
		return
	}
	v.st.LLMProfiles = []LLMProfile{{
		ID:      defaultLLMProfileID,
		Name:    "默认",
		BaseURL: v.st.LLM.BaseURL,
		Model:   v.st.LLM.Model,
	}}
	v.st.LLMAPIKeys[defaultLLMProfileID] = v.st.LLMAPIKey
	v.st.ActiveLLM = defaultLLMProfileID
}

// Protection 返回当前主密钥的保护方式。
func (v *Vault) Protection() KeyProtection {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.protection
}

// Dir 返回凭证库所在目录。
func (v *Vault) Dir() string { return v.dir }

// Seal 用主密钥加密任意数据。供审计日志等模块复用同一把密钥 ——
// 这样审计记录与凭证库享有同等的静态保护等级。
func (v *Vault) Seal(plain []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.key) == 0 {
		return nil, ErrLocked
	}
	return encrypt(v.key, plain)
}

// Unseal 用主密钥解密数据。
func (v *Vault) Unseal(blob []byte) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.key) == 0 {
		return nil, ErrLocked
	}
	return decrypt(v.key, blob)
}

// ---- 主机 ----

// ListHosts 返回不含密钥的主机列表。
func (v *Vault) ListHosts() []Host {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]Host, len(v.st.Hosts))
	copy(out, v.st.Hosts)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetHost 按 ID 取主机视图。
func (v *Vault) GetHost(id string) (Host, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, h := range v.st.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}

// SaveHost 新增或更新主机。sec 为 nil 时保留原有密钥材料。
func (v *Vault) SaveHost(h Host, sec *HostSecret) error {
	if strings.TrimSpace(h.Name) == "" {
		return errors.New("主机名称不能为空")
	}
	if strings.TrimSpace(h.Addr) == "" {
		return errors.New("地址不能为空")
	}
	if strings.TrimSpace(h.User) == "" {
		return errors.New("用户名不能为空")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}

	if sec != nil {
		v.st.Secrets[h.ID] = *sec
	}
	_, has := v.st.Secrets[h.ID]
	h.HasSecret = has && !v.st.Secrets[h.ID].Empty()

	replaced := false
	for i := range v.st.Hosts {
		if v.st.Hosts[i].ID == h.ID {
			v.st.Hosts[i] = h
			replaced = true
			break
		}
	}
	if !replaced {
		v.st.Hosts = append(v.st.Hosts, h)
	}
	return v.saveLocked()
}

// DeleteHost 删除主机及其密钥材料。
func (v *Vault) DeleteHost(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	hosts := v.st.Hosts[:0]
	for _, h := range v.st.Hosts {
		if h.ID != id {
			hosts = append(hosts, h)
		}
	}
	v.st.Hosts = hosts
	delete(v.st.Secrets, id)
	// 顺手清掉该主机的会话上限覆盖。
	//
	// 不清的话它会永远留在加密文件里：主机的 ID 是随机生成的、不会复用，
	// 所以这条覆盖再也不可能生效；但它会被原样下发到前端，
	// 让「3 台主机设了独立上限」这类计数永远比实际多，
	// 而用户在主机的列表里根本找不到那第 3 台。
	delete(v.st.Policy.SessionOverrides, id)
	return v.saveLocked()
}

// Secret 返回指定主机的密钥材料。仅供后端 SSH 模块调用。
//
// 这是全项目唯一读取密钥的出口 —— 任何把它暴露给 LLM 的改动都是安全缺陷。
func (v *Vault) Secret(id string) (HostSecret, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.st.Secrets[id]
	return s, ok
}

// SecretStrings 返回所有密钥材料的字面值，用于输出脱敏比对。
func (v *Vault) SecretStrings() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) >= 4 { // 太短的值脱敏会误伤正常文本
			out = append(out, s)
		}
	}
	// 旧字段与新方案表的 Key 都要参与脱敏比对。旧字段在迁移后是空的，
	// 但留着这个出口：万一哪天迁移逻辑变了，这里不能成为漏网之鱼。
	add(v.st.LLMAPIKey)
	for _, k := range v.st.LLMAPIKeys {
		add(k)
	}
	for _, s := range v.st.Secrets {
		add(s.Password)
		add(s.Passphrase)
		if s.PrivateKey != "" {
			add(s.PrivateKey)
			// 私钥正文逐行也比对，避免只脱敏整体而漏掉单行
			for _, ln := range strings.Split(s.PrivateKey, "\n") {
				ln = strings.TrimSpace(ln)
				if len(ln) >= 16 && !strings.HasPrefix(ln, "-----") {
					out = append(out, ln)
				}
			}
		}
	}
	return out
}

// ---- 主机指纹（TOFU 防中间人） ----

// HostKey 返回已记录的主机密钥指纹。
func (v *Vault) HostKey(addr string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	fp, ok := v.st.HostKeys[addr]
	return fp, ok
}

// SetHostKey 记录主机密钥指纹。
func (v *Vault) SetHostKey(addr, fingerprint string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	if v.st.HostKeys == nil {
		v.st.HostKeys = map[string]string{}
	}
	v.st.HostKeys[addr] = fingerprint
	return v.saveLocked()
}

// ForgetHostKey 清除主机指纹，下次连接重新 TOFU。
func (v *Vault) ForgetHostKey(addr string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.st.HostKeys, addr)
	return v.saveLocked()
}

// ---- LLM 配置（多方案） ----
//
// 所有读路径都收敛到「当前激活的方案」：LLM() / LLMSettingsView() 只认
// activeProfileLocked() 的结果，调用方（Agent、测试连接）完全不必知道
// 方案的存在。切换方案 = 改一个字符串字段，下一次对话即生效。

// activeProfileLocked 返回当前激活的方案（须持锁）。
// ActiveLLM 为空或指向已删除的方案时回落到第一个 —— 删除路径会维护
// ActiveLLM，这里只是兜底，保证读路径永远拿得到一套可用配置。
func (v *Vault) activeProfileLocked() *LLMProfile {
	for i := range v.st.LLMProfiles {
		if v.st.LLMProfiles[i].ID == v.st.ActiveLLM {
			return &v.st.LLMProfiles[i]
		}
	}
	if len(v.st.LLMProfiles) > 0 {
		return &v.st.LLMProfiles[0]
	}
	return nil
}

// activeProfileIDLocked 返回有效激活 ID（须持锁），与上面同一个兜底规则。
func (v *Vault) activeProfileIDLocked() string {
	if p := v.activeProfileLocked(); p != nil {
		return p.ID
	}
	return ""
}

// LLMProfiles 返回可下发给前端的方案列表（不含密钥）。
func (v *Vault) LLMProfiles() []LLMProfileView {
	v.mu.RLock()
	defer v.mu.RUnlock()
	active := v.activeProfileIDLocked()
	out := make([]LLMProfileView, 0, len(v.st.LLMProfiles))
	for _, p := range v.st.LLMProfiles {
		out = append(out, LLMProfileView{
			ID:        p.ID,
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Model:     p.Model,
			HasAPIKey: v.st.LLMAPIKeys[p.ID] != "",
			Active:    p.ID == active,
		})
	}
	return out
}

// LLMProfile 按 ID 取一套方案（不含密钥）。
func (v *Vault) LLMProfile(id string) (LLMProfile, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	for _, p := range v.st.LLMProfiles {
		if p.ID == id {
			return p, true
		}
	}
	return LLMProfile{}, false
}

// LLMKey 返回指定方案的 API Key。仅供后端调用。
func (v *Vault) LLMKey(profileID string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.st.LLMAPIKeys[profileID]
}

// SaveLLMProfile 新增或更新一套方案。apiKey 为 nil 时保留原密钥。
// p.ID 必须非空（生成新 ID 是调用方的职责 —— 那是应用层的关注点）。
func (v *Vault) SaveLLMProfile(p LLMProfile, apiKey *string) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("方案名称不能为空")
	}
	if p.ID == "" {
		return errors.New("缺少方案 ID")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}

	p.Name = strings.TrimSpace(p.Name)
	// 与 SetLLM 相同的规范化语义：非空才更新（编辑时留空 = 保留原值）。
	if nb := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"); nb != "" {
		p.BaseURL = nb
	} else if i := v.indexOfProfileLocked(p.ID); i >= 0 {
		p.BaseURL = v.st.LLMProfiles[i].BaseURL
	}
	if m := strings.TrimSpace(p.Model); m != "" {
		p.Model = m
	} else if i := v.indexOfProfileLocked(p.ID); i >= 0 {
		p.Model = v.st.LLMProfiles[i].Model
	}

	if i := v.indexOfProfileLocked(p.ID); i >= 0 {
		v.st.LLMProfiles[i] = p
	} else {
		v.st.LLMProfiles = append(v.st.LLMProfiles, p)
		// 第一套方案自动成为激活方案 —— 保证「永远至少有一套可用配置」。
		if v.activeProfileIDLocked() == "" {
			v.st.ActiveLLM = p.ID
		}
	}
	if apiKey != nil {
		if v.st.LLMAPIKeys == nil {
			v.st.LLMAPIKeys = map[string]string{}
		}
		v.st.LLMAPIKeys[p.ID] = strings.TrimSpace(*apiKey)
	}
	return v.saveLocked()
}

// DeleteLLMProfile 删除一套方案。至少保留一套 —— 空列表会让所有读路径
// 落到旧字段兜底上，那是一条迁移之后没人再维护的死路。
func (v *Vault) DeleteLLMProfile(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("缺少方案 ID")
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	i := v.indexOfProfileLocked(id)
	if i < 0 {
		return errors.New("方案不存在")
	}
	if len(v.st.LLMProfiles) == 1 {
		return errors.New("至少保留一套配置方案，不能删除最后一套")
	}
	v.st.LLMProfiles = append(v.st.LLMProfiles[:i], v.st.LLMProfiles[i+1:]...)
	delete(v.st.LLMAPIKeys, id)
	// 删的正好是激活方案：切到剩下的第一套，而不是留一个悬空引用
	// 让读路径去猜（activeProfileLocked 虽然有兜底，但状态要 honest）。
	if v.st.ActiveLLM == id {
		v.st.ActiveLLM = v.st.LLMProfiles[0].ID
	}
	return v.saveLocked()
}

// ActivateLLMProfile 切换当前使用的方案。
func (v *Vault) ActivateLLMProfile(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	if v.indexOfProfileLocked(id) < 0 {
		return errors.New("方案不存在")
	}
	v.st.ActiveLLM = id
	return v.saveLocked()
}

// indexOfProfileLocked 返回方案下标，不存在返回 -1（须持锁）。
func (v *Vault) indexOfProfileLocked(id string) int {
	for i := range v.st.LLMProfiles {
		if v.st.LLMProfiles[i].ID == id {
			return i
		}
	}
	return -1
}

// LLMSettingsView 返回当前激活方案的可下发视图（不含密钥）。
func (v *Vault) LLMSettingsView() LLMSettingsView {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if p := v.activeProfileLocked(); p != nil {
		return LLMSettingsView{
			BaseURL:   p.BaseURL,
			Model:     p.Model,
			HasAPIKey: v.st.LLMAPIKeys[p.ID] != "",
		}
	}
	return LLMSettingsView{
		BaseURL:   v.st.LLM.BaseURL,
		Model:     v.st.LLM.Model,
		HasAPIKey: v.st.LLMAPIKey != "",
	}
}

// SetLLM 更新 LLM 配置。apiKey 为 nil 时保留原密钥。
//
// 多方案引入后它退化为「更新当前激活方案」：既有调用方（测试与老代码）
// 的语义不变 —— 它们本来就只想改「正在用的那套」。
func (v *Vault) SetLLM(baseURL, model string, apiKey *string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	if p := v.activeProfileLocked(); p != nil {
		if strings.TrimSpace(baseURL) != "" {
			p.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		}
		if strings.TrimSpace(model) != "" {
			p.Model = strings.TrimSpace(model)
		}
		if apiKey != nil {
			v.st.LLMAPIKeys[p.ID] = strings.TrimSpace(*apiKey)
		}
		return v.saveLocked()
	}
	// 兜底：没有任何方案时退回旧字段。只有「配置文件损坏或迁移被跳过」
	// 才可能到这里（Open 的迁移会为旧字段生成首套方案）；留着这条路
	// 是为了让 LLM() 永远有值可读，而不是 panic 或返回零值。
	if strings.TrimSpace(baseURL) != "" {
		v.st.LLM.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	}
	if strings.TrimSpace(model) != "" {
		v.st.LLM.Model = strings.TrimSpace(model)
	}
	if apiKey != nil {
		v.st.LLMAPIKey = strings.TrimSpace(*apiKey)
	}
	return v.saveLocked()
}

// LLM 返回当前激活方案的配置与密钥。仅供后端调用。
func (v *Vault) LLM() (LLMSettings, string) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if p := v.activeProfileLocked(); p != nil {
		return LLMSettings{BaseURL: p.BaseURL, Model: p.Model}, v.st.LLMAPIKeys[p.ID]
	}
	return v.st.LLM, v.st.LLMAPIKey
}

// ---- 策略 ----

// Policy 返回策略配置。
//
// 会话上下文上限也在这里补默认值：早期版本落盘的 store 里没有这三个字段，
// 反序列化后是 0。若不补，Agent 会按「0 轮」保留历史 —— 表现为
// 升级后上下文功能静默失效，而用户什么都没改过。补默认值让老配置
// 也能获得与新版一致的默认行为。
func (v *Vault) Policy() PolicySettings {
	v.mu.RLock()
	defer v.mu.RUnlock()
	p := v.st.Policy

	// 深拷贝引用型字段再返回。
	//
	// 直接返回等于把库里的 map / slice 交出去：调用方（前端绑定、
	// 测试、策略面板）随手改一下，就**绕过 SetPolicy** 改了库里的状态 ——
	// 既不落盘也不留审计，而之后每次 Policy() 都读到那个脏值，
	// 直到重启才恢复。这种「改了设置、当时生效、重启后变回去」的现象
	// 极难归因，所以宁可在读路径上多分配一次。
	p.SessionOverrides = cloneSessionLimits(v.st.Policy.SessionOverrides)

	if len(p.Whitelist) == 0 {
		p.Whitelist = v.st.Whitelist
	}
	if len(p.Whitelist) == 0 {
		p.Whitelist = defaultWhitelist()
	}
	p.Whitelist = cloneStrings(p.Whitelist)

	applySessionDefaults(&p)
	return p
}

// cloneSessionLimits 复制一份主机级覆盖表。nil 保持 nil ——
// 让「没有任何覆盖」在 JSON 里仍然被 omitempty 省掉。
func cloneSessionLimits(m map[string]SessionLimits) map[string]SessionLimits {
	if m == nil {
		return nil
	}
	out := make(map[string]SessionLimits, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneStrings 复制一份字符串切片，nil 保持 nil。
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// applySessionDefaults 给未设置（<=0）的会话上下文上限填入默认值。
//
// 与 MaxOutput / MaxSteps 的处理保持一致：都是「非正值视为未设置」。
// 这样前端传 0 或空值不会把上下文窗口意外关掉。
func applySessionDefaults(p *PolicySettings) {
	if p.MaxSessionTurns <= 0 {
		p.MaxSessionTurns = defaultMaxSessionTurns
	}
	if p.MaxStoredToolBytes <= 0 {
		p.MaxStoredToolBytes = defaultMaxStoredToolBytes
	}
	if p.MaxSessionBytes <= 0 {
		p.MaxSessionBytes = defaultMaxSessionBytes
	}
	sanitizeSessionOverrides(p)
}

// sanitizeSessionOverrides 规整主机级覆盖表。
//
// 做两件事，缺一不可：
//
//  1. 负数一律归零（= 继承全局）。与全局的「非正值视为未设置」对齐；
//     留着负数会让下游的 `> 0` 判断自然忽略它，但表里存在一个
//     语义为负的条目，会让「有 N 台主机设了独立上限」这类计数失真。
//
//  2. 三项都为空的条目直接删掉。
//
// 第 2 条不是洁癖：界面按「哪些主机有覆盖」呈现状态，留着一堆空条目
// 会让用户看到「3 台主机有自定义上限」，而实际上一个都没设 ——
// 之后他调全局值发现这三台「没跟」，会以为是 bug。
//
// **注意不能给覆盖项回填默认值**：那样每个字段都会变成非零，
// 「继承」就消失了 —— 一台只想调轮数的主机会被钉死在当时的
// 单条工具输出上限上，之后改全局它再也不跟。
func sanitizeSessionOverrides(p *PolicySettings) {
	for id, ov := range p.SessionOverrides {
		if ov.MaxSessionTurns < 0 {
			ov.MaxSessionTurns = 0
		}
		if ov.MaxStoredToolBytes < 0 {
			ov.MaxStoredToolBytes = 0
		}
		if ov.MaxSessionBytes < 0 {
			ov.MaxSessionBytes = 0
		}
		if ov.Empty() {
			delete(p.SessionOverrides, id)
			continue
		}
		p.SessionOverrides[id] = ov
	}
}

// SetPolicy 更新策略配置。
func (v *Vault) SetPolicy(p PolicySettings) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
	// 先脱离调用方的底层存储再规整：下面的 sanitizeSessionOverrides
	// 会删条目，若直接操作入参的 map，就会顺手改掉调用方持有的那份数据
	// （前端绑定里是一次性对象无所谓，但测试与内部调用方会踩到）。
	p.SessionOverrides = cloneSessionLimits(p.SessionOverrides)
	p.Whitelist = cloneStrings(p.Whitelist)

	if p.Mode != ModeManual && p.Mode != ModeWhitelist {
		return fmt.Errorf("未知的策略模式: %s", p.Mode)
	}
	if p.MaxOutput <= 0 {
		p.MaxOutput = 32 * 1024
	}
	if p.MaxSteps <= 0 {
		p.MaxSteps = 12
	}
	applySessionDefaults(&p)
	v.st.Policy = p
	// 存的是同一份副本而不是 p.Whitelist 的别名：两者共享底层数组时，
	// 之后谁 append 一下都可能把对方的内容覆盖掉。
	v.st.Whitelist = p.Whitelist
	return v.saveLocked()
}

// ---- 落盘 ----

func (v *Vault) path(name string) string { return filepath.Join(v.dir, name) }

func (v *Vault) saveLocked() error {
	plain, err := json.MarshalIndent(v.st, "", "  ")
	if err != nil {
		return err
	}
	blob, err := encrypt(v.key, plain)
	if err != nil {
		return err
	}
	return atomicWrite(v.path(vaultFileName), blob, 0o600)
}

// ---- 主密钥 ----

func (v *Vault) loadOrCreateKey() ([]byte, KeyProtection, error) {
	// 允许显式降级到密钥文件（无钥匙串守护进程的环境，如 headless Linux / CI）
	if os.Getenv("AISHELL_KEYFILE") == "1" {
		return v.loadOrCreateKeyFile()
	}

	if raw, err := keyring.Get(keyringService, keyringUser); err == nil && raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err == nil && len(key) == keySize {
			return key, ProtectionKeychain, nil
		}
	} else if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		// 钥匙串不可用，走降级路径
		return v.loadOrCreateKeyFile()
	}

	// 钥匙串可用但还没有密钥 —— 生成并写入
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	if err := keyring.Set(keyringService, keyringUser, base64.StdEncoding.EncodeToString(key)); err != nil {
		return v.loadOrCreateKeyFile()
	}
	return key, ProtectionKeychain, nil
}

func (v *Vault) loadOrCreateKeyFile() ([]byte, KeyProtection, error) {
	p := v.path(keyFileName)
	if raw, err := os.ReadFile(p); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err == nil && len(key) == keySize {
			return key, ProtectionKeyFile, nil
		}
	}
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	if err := atomicWrite(p, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, "", fmt.Errorf("写入主密钥失败: %w", err)
	}
	return key, ProtectionKeyFile, nil
}

// ---- 加解密 ----

func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(key, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("密文长度非法")
	}
	return gcm.Open(nil, blob[:ns], blob[ns:], nil)
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
