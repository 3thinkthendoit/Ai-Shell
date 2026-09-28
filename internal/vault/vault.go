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
	if v.st.Policy.Mode == "" {
		v.st.Policy = defaultStore().Policy
	}
	v.opened = true
	return nil
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
	if v.st.LLMAPIKey != "" {
		add(v.st.LLMAPIKey)
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

// ---- LLM 配置 ----

// LLMSettingsView 返回可下发给前端的 LLM 配置（不含密钥）。
func (v *Vault) LLMSettingsView() LLMSettingsView {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return LLMSettingsView{
		BaseURL:   v.st.LLM.BaseURL,
		Model:     v.st.LLM.Model,
		HasAPIKey: v.st.LLMAPIKey != "",
	}
}

// SetLLM 更新 LLM 配置。apiKey 为 nil 时保留原密钥。
func (v *Vault) SetLLM(baseURL, model string, apiKey *string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.opened {
		return ErrLocked
	}
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

// LLM 返回 LLM 配置与密钥。仅供后端调用。
func (v *Vault) LLM() (LLMSettings, string) {
	v.mu.RLock()
	defer v.mu.RUnlock()
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
