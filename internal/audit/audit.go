// Package audit 提供防篡改的本地审计日志。
//
// 设计目标（按重要性排序）：
//
//  1. **可追溯** —— 每一次对远端主机的操作、每一次人工批准/拒绝、每一次配置变更，
//     都留下一条不可否认的记录。这是运维工具与合规场景的硬需求。
//  2. **静态加密** —— 每条记录单独用主密钥 AES-GCM 加密后以 JSONL 追加落盘。
//     逐条加密（而非整文件加密）是为了支持纯追加写入：不必为了记一条日志而重写整个文件。
//  3. **防篡改** —— 每条记录携带 Seq 与 PrevHash，构成哈希链。任何对历史记录的
//     删改都会让链条断裂，Verify 能定位到断点。
//  4. **可轮转** —— 单个段超过阈值后归档为 audit.seg-*.log，并按保留策略丢弃最旧的段，
//     使日志不会无限增长。
//
// # 轮转如何不破坏哈希链
//
// Seq 是**全局单调**的，不随轮转重置。归档只是把已有内容改名，新段接着往下写，
// 因此把各段按序号顺序拼接后，得到的正是一条连续的链 —— Verify 的逻辑完全不用改。
// 校验的起点不是硬编码的 1，而是「当前保留的最早一条记录」自身的 (Seq, PrevHash)：
// 该记录的 PrevHash 就是它自己的链根，于是无论前面丢了多少段，剩余部分仍可完整校验。
//
// # 明确的安全边界
//
// 保留策略会丢弃最旧的段，这等价于**前缀截断**，因此「更早的记录不存在了」这件事
// 仅凭日志文件本身无法分辨是保留策略还是攻击者所为。两点缓解：
//
//   - 丢弃动作本身会写一条 KindRetention 记录进新段。也就是说，凡是本程序主动做的
//     截断，链上都会留下声明。不过该声明自身也可能被后续轮转丢弃，所以它的缺失
//     只是一个提示，不能作为「日志被人为截断」的结论。
//   - 中间段被整段删除**可以**被发现 —— 拼接后 Seq 会出现断档。
//
// 彻底消除该边界需要把链锚点写到本机之外（如远端日志服务），超出当前威胁模型。
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sealer 由 vault 提供，让审计日志复用同一把主密钥。
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	Unseal(blob []byte) ([]byte, error)
}

// 记录类型。
const (
	KindAgentRun  = "agent_run" // 用户发起一轮 agent 会话
	KindTool      = "tool"      // agent 的一次工具调用及其裁决/结果
	KindDirect    = "direct"    // 人工 shell（经策略）：交互式会话里人敲的命令；亦含历史直连记录
	KindTerminal  = "terminal"  // 交互终端会话的开始/结束
	KindHost      = "host"      // 主机配置变更
	KindLLM       = "llm"       // LLM 配置变更
	KindPolicy    = "policy"    // 安全策略变更
	KindRetention = "retention" // 保留策略丢弃了历史段（日志被有意截断的声明）
	KindSystem    = "system"    // 其他（启动、校验等）
)

// 轮转与保留的默认值。
const (
	// DefaultMaxSize 是单个段的字节上限。8 MiB 约可容纳数万条记录。
	DefaultMaxSize int64 = 8 << 20
	// DefaultMaxFiles 是保留的历史段数量。默认保留 8 段，即最多约 72 MiB。
	DefaultMaxFiles = 8

	liveName  = "audit.log"
	segPrefix = "audit.seg-"
	segSuffix = ".log"
)

// segNameRe 严格匹配归档段文件名。用严格匹配而非前缀/后缀判断，
// 是为了让目录里的无关文件（编辑器临时文件、用户误放的文件）被安全忽略，
// 而不是让整个日志写入失败。
var segNameRe = regexp.MustCompile(`^audit\.seg-(\d{10})-(\d{10})\.log$`)

// Entry 是一条审计记录。
//
// 注意：Command 字段会记录命令原文。这也是日志本身要加密的原因 ——
// 命令行里可能夹带口令（例如 mysql -p'...'），而用户的威胁模型正是「防止别的软件分析」。
type Entry struct {
	Seq        int      `json:"seq"`
	Time       string   `json:"time"`
	Kind       string   `json:"kind"`
	HostID     string   `json:"hostId,omitempty"`
	HostName   string   `json:"hostName,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Command    string   `json:"command,omitempty"`
	Decision   string   `json:"decision,omitempty"` // allow | confirm | deny
	Rule       string   `json:"rule,omitempty"`
	Approved   *bool    `json:"approved,omitempty"` // 仅 confirm 时有意义
	ExitCode   *int     `json:"exitCode,omitempty"`
	DurationMs int64    `json:"durationMs,omitempty"`
	Redacted   int      `json:"redacted,omitempty"`
	Injection  []string `json:"injection,omitempty"`
	Note       string   `json:"note,omitempty"`
	PrevHash   string   `json:"prevHash"`
	Hash       string   `json:"hash"`
}

// VerifyResult 是完整性校验结果。
type VerifyResult struct {
	OK       bool `json:"ok"`
	Count    int  `json:"count"`
	BrokenAt int  `json:"brokenAt,omitempty"`
	// Truncated 表示本次校验的起点不是序号 1，即更早的记录已不在。
	// 可能是保留策略丢弃，也可能是日志被截断 —— 仅凭日志无法区分。
	Truncated bool `json:"truncated,omitempty"`
	// StartSeq 是本次实际校验到的起始序号。
	StartSeq int `json:"startSeq,omitempty"`
	// Segments 是当前存在的日志段数量（含正在写入的段）。
	Segments int    `json:"segments,omitempty"`
	Message  string `json:"message"`
}

// Config 控制轮转与保留行为。
type Config struct {
	// MaxSize 是单个段的字节上限。<=0 时用 DefaultMaxSize。
	MaxSize int64
	// MaxFiles 是保留的历史段数量。<0 表示不限制（永不丢弃历史）。
	// 0 表示用 DefaultMaxFiles。
	MaxFiles int
}

func (c Config) withDefaults() Config {
	if c.MaxSize <= 0 {
		c.MaxSize = DefaultMaxSize
	}
	if c.MaxFiles == 0 {
		c.MaxFiles = DefaultMaxFiles
	}
	return c
}

// Logger 是审计日志写入器。并发安全。
type Logger struct {
	mu   sync.Mutex
	dir  string
	path string // 正在写入的段
	s    Sealer
	seq  int
	prev string
	cfg  Config
}

// New 打开（或创建）指定目录下的审计日志，使用默认轮转配置。
func New(dir string, s Sealer) (*Logger, error) {
	return NewWithConfig(dir, s, Config{})
}

// NewWithConfig 同上，但可指定轮转与保留策略。
func NewWithConfig(dir string, s Sealer, c Config) (*Logger, error) {
	if s == nil {
		return nil, errors.New("audit: 缺少 Sealer")
	}
	l := &Logger{
		dir:  dir,
		path: filepath.Join(dir, liveName),
		s:    s,
		cfg:  c.withDefaults(),
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// Path 返回正在写入的段路径。
func (l *Logger) Path() string { return l.path }

// Config 返回生效的轮转配置。
func (l *Logger) Config() Config { return l.cfg }

// ---- 段管理 ----

// segments 按序号顺序返回所有段：历史段在前，正在写入的段在最后。
//
// 段名形如 audit.seg-0000000001-0000004200.log，起止序号零填充，
// 因此字典序与序号序一致，无需额外解析排序。
// 不符合该格式的文件一律忽略 —— 目录里混进无关文件不应让日志写入失败。
func (l *Logger) segments() []string {
	var out []string
	if des, err := os.ReadDir(l.dir); err == nil {
		for _, de := range des {
			if de.IsDir() || !segNameRe.MatchString(de.Name()) {
				continue
			}
			out = append(out, filepath.Join(l.dir, de.Name()))
		}
	}
	sort.Strings(out)
	return append(out, l.path)
}

// Segments 返回当前日志段数量（含正在写入的段）。
func (l *Logger) Segments() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.segments())
}

// Size 返回所有段的字节数之和 —— 这是用户视角的「日志大小」。
func (l *Logger) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sizeLocked()
}

func (l *Logger) sizeLocked() int64 {
	var total int64
	for _, p := range l.segments() {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// LiveSize 返回正在写入的段的字节数，用于判断是否需要轮转。
func (l *Logger) LiveSize() int64 { return l.liveSize() }

func (l *Logger) liveSize() int64 {
	fi, err := os.Stat(l.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// load 恢复链头（seq 与 prev hash）。
//
// 只读最新一个非空段的最后一条记录，不读全量 —— 日志可能很大，启动不该因此变慢。
func (l *Logger) load() error {
	segs := l.segments()
	for i := len(segs) - 1; i >= 0; i-- {
		raw, err := lastRawLine(segs[i])
		if err != nil {
			return err
		}
		if raw == "" {
			continue
		}
		e, err := l.decodeLine(raw)
		if err != nil {
			return fmt.Errorf("恢复审计链头失败（%s）: %w", filepath.Base(segs[i]), err)
		}
		l.seq = e.Seq
		l.prev = e.Hash
		return nil
	}
	return nil
}

// Append 追加一条记录，自动填充 Seq / Time / PrevHash / Hash。
//
// 若正在写入的段已达上限，会先归档该段再写入。
func (l *Logger) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cfg.MaxSize > 0 && l.liveSize() >= l.cfg.MaxSize {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	return l.appendLocked(e)
}

// 任何一条失败路径都不得让「本该存在的记录」消失得无影无踪。
//
// 核心不变量：**一旦 e.Seq 确定下来（= l.seq+1），序号 N 这条记录「本该存在」这件事
// 就已经成立**。此后无论哪一步失败，都必须推进 l.seq，故意在链上留下一个永久空洞，
// 使这次丢失可被 Verify() 发现。
//
// 反例（本函数曾经的写法，只覆盖了 WriteString 一条路径）：失败时不推进 seq，
// 于是这次丢失彻底无痕 —— 序号连续、前序哈希连续、Verify() 报 OK，
// 而记录实际上少了。这是审计日志最不该有的失败模式：用户以为有完整轨迹，其实没有。
//
// 因此把提交动作放进 defer，覆盖 hashEntry / json.Marshal / Seal / OpenFile / Write
// 全部失败点，而不是逐个 if 里手写一遍（那样迟早会漏掉新增的失败点）。
//
// 代价：一次瞬时故障（磁盘满、密钥串短暂不可用）会让日志永久显示为不完整。
// 但这就是事实 —— 审计日志宁可如实报缺，也不该假装完整。
//
// 注意 prev 只在成功时推进：空洞之后的记录仍接在最后一条**成功落盘**的记录哈希上，
// 这样链条本身不断，断的只是序号连续性，Verify 才能准确定位到「缺了哪一条」。
//
// 已知边界：若失败发生在本次进程的最后一次写入之后，磁盘上链头停在最后一条成功记录，
// 空洞无从比对（没有后续记录作参照）。这无法在本地解决，需要把链锚点写到本机之外。
func (l *Logger) appendLocked(e Entry) (err error) {
	e.Seq = l.seq + 1
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	e.PrevHash = l.prev
	e.Hash = ""

	defer func() {
		if err != nil {
			l.seq = e.Seq
		}
	}()

	h, err := hashEntry(e)
	if err != nil {
		return err
	}
	e.Hash = h

	plain, err := json.Marshal(e)
	if err != nil {
		return err
	}
	blob, err := l.s.Seal(plain)
	if err != nil {
		return fmt.Errorf("加密审计记录失败: %w", err)
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开审计日志失败: %w", err)
	}
	defer f.Close()

	if _, werr := f.WriteString(base64.StdEncoding.EncodeToString(blob) + "\n"); werr != nil {
		return fmt.Errorf("写入审计日志失败: %w", werr)
	}

	l.seq = e.Seq
	l.prev = h
	return nil
}

// rotateLocked 把当前段归档，并按保留策略丢弃最旧的段。
//
// 调用方必须持有 l.mu。
func (l *Logger) rotateLocked() error {
	if l.liveSize() == 0 {
		return nil
	}
	first, err := l.firstSeqOf(l.path)
	if err != nil {
		return err
	}
	dst := filepath.Join(l.dir,
		fmt.Sprintf("%s%010d-%010d%s", segPrefix, first, l.seq, segSuffix))

	// 目标已存在说明上一次轮转只做了一半（例如进程被杀）。
	// 直接覆盖会导致数据丢失，因此宁可报错也不要静默合并。
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("审计日志轮转目标已存在: %s", filepath.Base(dst))
	}
	if err := os.Rename(l.path, dst); err != nil {
		return fmt.Errorf("轮转审计日志失败: %w", err)
	}
	return l.enforceRetentionLocked()
}

// enforceRetentionLocked 丢弃超出保留数量的最旧段。
//
// 丢弃前会往新段写一条 KindRetention 记录，使「本程序主动截断」这件事本身进入哈希链，
// 从而与「被攻击者截断」区分开。调用方必须持有 l.mu。
func (l *Logger) enforceRetentionLocked() error {
	if l.cfg.MaxFiles < 0 {
		return nil // 不限制
	}
	segs := l.segments()
	archived := segs[:len(segs)-1] // 最后一个是正在写入的段
	if len(archived) <= l.cfg.MaxFiles {
		return nil
	}

	drop := archived[:len(archived)-l.cfg.MaxFiles]
	var ranges []string
	for _, p := range drop {
		first, last, err := l.segRange(p)
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("丢弃历史审计段失败: %w", err)
		}
		ranges = append(ranges, fmt.Sprintf("%d-%d", first, last))
	}

	return l.appendLocked(Entry{
		Kind: KindRetention,
		Note: fmt.Sprintf("保留策略生效：已丢弃 %d 个历史段（序号 %s），更早的记录不再保留",
			len(drop), strings.Join(ranges, "、")),
	})
}

// segRange 解析归档段的起止序号（来自文件名，不读内容）。
func (l *Logger) segRange(path string) (int, int, error) {
	m := segNameRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return 0, 0, fmt.Errorf("无法解析审计段名: %s", filepath.Base(path))
	}
	first, _ := strconv.Atoi(m[1])
	last, _ := strconv.Atoi(m[2])
	return first, last, nil
}

// firstSeqOf 返回某段第一条记录的序号（只解密首行）。
func (l *Logger) firstSeqOf(path string) (int, error) {
	raw, err := firstRawLine(path)
	if err != nil || raw == "" {
		return 0, err
	}
	e, err := l.decodeLine(raw)
	if err != nil {
		return 0, err
	}
	return e.Seq, nil
}

// ---- 读取 ----

// decodeLine 解密并解析一行。
func (l *Logger) decodeLine(raw string) (Entry, error) {
	var e Entry
	blob, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return e, fmt.Errorf("base64 解码失败: %w", err)
	}
	plain, err := l.s.Unseal(blob)
	if err != nil {
		return e, fmt.Errorf("解密失败: %w", err)
	}
	if err := json.Unmarshal(plain, &e); err != nil {
		return e, fmt.Errorf("解析失败: %w", err)
	}
	return e, nil
}

// readFile 解密并返回单个段的全部记录。
func (l *Logger) readFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		e, err := l.decodeLine(raw)
		if err != nil {
			return out, fmt.Errorf("%s 第 %d 行: %w", filepath.Base(path), line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// ReadAll 按序号顺序返回所有段的全部记录。
func (l *Logger) ReadAll() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAllLocked()
}

func (l *Logger) readAllLocked() ([]Entry, error) {
	var out []Entry
	for _, p := range l.segments() {
		ents, err := l.readFile(p)
		if err != nil {
			return out, err
		}
		out = append(out, ents...)
	}
	return out, nil
}

// Recent 返回最近 limit 条记录（按时间正序）。
//
// 从最新的段往前读，读满即停 —— 日志轮转之后可能有几十万条历史记录，
// 不该为了取最后 50 条而解密全部。
func (l *Logger) Recent(limit int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if limit <= 0 {
		return l.readAllLocked()
	}
	segs := l.segments()
	out := make([]Entry, 0, limit)
	for i := len(segs) - 1; i >= 0 && len(out) < limit; i-- {
		ents, err := l.readFile(segs[i])
		if err != nil {
			return nil, err
		}
		for j := len(ents) - 1; j >= 0 && len(out) < limit; j-- {
			out = append(out, ents[j])
		}
	}
	// 反转为时间正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Verify 校验哈希链完整性。
//
// 逐条重算 Hash 并检查 PrevHash 串联与 Seq 连续性。任何一处被删改都会暴露。
//
// 校验起点是「当前保留的最早一条记录」，而非序号 1：保留策略可能已经丢弃了更早的段，
// 那条记录的 PrevHash 就是剩余链的根。若起点不是 1，结果里的 Truncated 会置位，
// 提示「更早的记录已不在」（保留策略与恶意截断在日志层面无法区分）。
func (l *Logger) Verify() VerifyResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.readAllLocked()
	if err != nil {
		return VerifyResult{OK: false, Message: "读取失败: " + err.Error()}
	}
	segs := len(l.segments())
	if len(entries) == 0 {
		return VerifyResult{OK: true, Count: 0, Segments: segs, Message: "日志为空，无需校验"}
	}

	startSeq := entries[0].Seq
	prevHash := entries[0].PrevHash
	for i, e := range entries {
		wantSeq := startSeq + i
		if e.Seq != wantSeq {
			return VerifyResult{
				OK: false, Count: len(entries), BrokenAt: i + 1, Segments: segs,
				Message: fmt.Sprintf("第 %d 条（序号 %d）应为序号 %d —— 序号不连续，中间有条目缺失"+
					"（可能是被删除，也可能是当时写入失败）",
					i+1, e.Seq, wantSeq),
			}
		}
		if e.PrevHash != prevHash {
			return VerifyResult{
				OK: false, Count: len(entries), BrokenAt: i + 1, Segments: segs,
				Message: fmt.Sprintf("第 %d 条的前序哈希不匹配（有条目被插入、删除或重排）", i+1),
			}
		}
		want, err := hashEntry(Entry{
			Seq: e.Seq, Time: e.Time, Kind: e.Kind,
			HostID: e.HostID, HostName: e.HostName,
			Tool: e.Tool, Command: e.Command,
			Decision: e.Decision, Rule: e.Rule,
			Approved: e.Approved, ExitCode: e.ExitCode,
			DurationMs: e.DurationMs, Redacted: e.Redacted,
			Injection: e.Injection, Note: e.Note,
			PrevHash: e.PrevHash, Hash: "",
		})
		if err != nil {
			return VerifyResult{OK: false, Count: len(entries), BrokenAt: i + 1, Segments: segs, Message: err.Error()}
		}
		if want != e.Hash {
			return VerifyResult{
				OK: false, Count: len(entries), BrokenAt: i + 1, Segments: segs,
				Message: fmt.Sprintf("第 %d 条的内容哈希不匹配（记录被篡改）", i+1),
			}
		}
		prevHash = e.Hash
	}

	res := VerifyResult{OK: true, Count: len(entries), Segments: segs, StartSeq: startSeq}
	if startSeq != 1 {
		res.Truncated = true
		res.Message = fmt.Sprintf(
			"自第 %d 条起哈希链完整，共 %d 条、%d 个段。更早的记录已不在：可能是保留策略丢弃，也可能是日志被截断 —— 仅凭日志无法区分。"+
				"链上若有「保留策略生效」记录可作为佐证，但该声明本身也可能已被后续轮转丢弃，因此「没有该记录」不能作为被篡改的结论。",
			startSeq, len(entries), segs)
		return res
	}
	res.Message = fmt.Sprintf("哈希链完整，共 %d 条记录、%d 个段", len(entries), segs)
	return res
}

// ExportPlaintext 把日志解密后导出为明文 JSONL，供外部工具分析。
// 这是一次显式的用户动作 —— 明文只写到用户指定的位置，不留在默认目录。
func (l *Logger) ExportPlaintext(dest string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := l.readAllLocked()
	if err != nil {
		return 0, err
	}
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// ---- 工具 ----

// lastRawLine 返回文件最后一行非空内容，不解密。
func lastRawLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var last string
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			last = s
		}
	}
	return last, sc.Err()
}

// firstRawLine 返回文件第一行非空内容，不解密。
func firstRawLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			return s, nil
		}
	}
	return "", sc.Err()
}

// hashEntry 计算一条记录的内容哈希（Hash 字段本身不参与计算）。
func hashEntry(e Entry) (string, error) {
	e.Hash = ""
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
