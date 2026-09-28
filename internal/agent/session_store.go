package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ai-shell/internal/llm"
)

// ---- 会话落盘 ----
//
// 为什么之前刻意不落盘、现在又落：
//
// 不落盘的理由是「会话含远端命令输出，落盘要面对加密、轮转、与审计日志的
// 重复存储，收益不抵成本」。加密这一项现在已经有现成答案 —— vault 的
// Seal/Unseal 与 audit 包共用同一把主密钥，会话沿用同一条路即可。
// 而收益变了：会话现在不只是「这一轮记得上一轮」，还有用户花 API 调用
// 换来的压缩摘要。重启就丢，那份成本等于白花。
//
// 仍然不做的：轮转。会话是**有上限的有限集合**（每台主机一条，各有
// 256KiB 上限），文件大小天然有界，不像审计日志那样是只增不减的流水。

// Sealer 由 vault 提供，让会话历史复用与凭证库、审计日志同一把主密钥。
//
// 与 audit 包的同名接口**结构相同但各自声明**：Go 的接口是结构化的，
// 两边都不需要 import 对方，也不会因为对方加了个方法而连带失败。
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	Unseal(blob []byte) ([]byte, error)
}

const (
	sessionFileName = "sessions.enc"

	// 磁盘格式版本。靠它做兼容分支 —— 没有版本号的话，只能靠
	// 「字段是不是零值」去猜，而零值是合法取值。
	//
	//	v1：sessions = map[主机ID]会话状态        （每台主机一条）
	//	v2：sessions = map[主机ID]map[会话ID]状态 （每台主机若干条）
	//
	// 两个版本在 JSON 里用的是**同一个键** `sessions`，只是值的类型不同。
	// 所以不能用「多带一个可选字段」的方式兼容 —— 反序列化会直接失败。
	// 只能先只读出版本号，再按版本去解析 sessions 那一段（见 Load）。
	sessionFileVersionV1 = 1
	sessionFileVersionV2 = 2

	// sessionFileVersion 是**写出**时用的版本。
	sessionFileVersion = sessionFileVersionV2
)

// maxPersistedBytes 是**序列化后**的落盘总量上限（所有主机的会话加起来）。
//
// 单条会话在内存里已有 256KiB 上限，但那是「每台主机」的，
// 主机多了总量仍然无界。落盘尤其需要一个总量闸门：
// 磁盘写满比内存占用高严重得多，而且难排查。
//
// 是变量而不是常量：测试可以把它压到几百字节来验证淘汰策略，
// 不必真的造出 8MiB 会话（那会让测试又慢又吃内存）。
// 与 sshclient.MaxCaptureBytes 同一个理由。
var maxPersistedBytes = 8 << 20 // 8 MiB

// SessionStore 把会话历史加密写到磁盘上的一个文件里。
type SessionStore struct {
	path   string
	sealer Sealer

	// stateMu 保护下面两个只在「载入失败 → 首次写入」这条路径上用到的标志。
	//
	// 单独一把锁而不是复用别处的：Save 可能被并发的写入路径调用
	// （一轮对话结束时的 remember、清空后的 flushSessions），
	// 而这些标志是有状态的，裸读裸写就是数据竞争。
	stateMu        sync.Mutex
	loadUnreadable bool
	backedUp       bool
}

// NewSessionStore 创建会话存储。dir 通常是 vault 的目录，sealer 通常是 vault 本身。
func NewSessionStore(dir string, s Sealer) *SessionStore {
	return &SessionStore{path: filepath.Join(dir, sessionFileName), sealer: s}
}

// Path 返回落盘文件路径（测试与排查用）。
func (ss *SessionStore) Path() string { return ss.path }

// markUnreadable 记下「启动时没能读懂磁盘上已有的文件」。
//
// 只记录，不在这里动文件：此时用户可能还没做任何事，
// 而改名会让他在别处（比如自己的备份脚本）看到文件莫名消失。
// 真正需要动文件的时刻是**第一次要覆盖它**的时候，见 backupUnreadableOnce。
func (ss *SessionStore) markUnreadable() {
	ss.stateMu.Lock()
	ss.loadUnreadable = true
	ss.stateMu.Unlock()
}

// backupUnreadableOnce 在第一次写入前，把读不懂的旧文件改名留档。
//
// 为什么必须做：读不懂的原因可能是「文件来自更新的版本」，也可能是
// 「被改坏了」。直接覆盖等于把那份数据永久删掉，而用户看到的
// 只是一句「载入失败，已从空白开始」—— 他没有任何机会知道
// 自己的历史是被程序覆盖掉的，也无从恢复。
//
// 只做一次：之后每次 Save 都改名的话，会把刚写好的新文件也搬走。
func (ss *SessionStore) backupUnreadableOnce() {
	ss.stateMu.Lock()
	need := ss.loadUnreadable && !ss.backedUp
	ss.backedUp = true
	ss.stateMu.Unlock()
	if !need {
		return
	}

	bak := ss.path + ".unreadable"
	if err := os.Rename(ss.path, bak); err != nil {
		if os.IsNotExist(err) {
			return // 文件本来就不在，没什么可留档的
		}
		// 留档失败**不能**让写入继续：那正是我们要避免的覆盖。
		fmt.Fprintf(os.Stderr, "[ai-shell] 无法留档读不懂的会话文件 %s: %v\n", ss.path, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[ai-shell] 已把读不懂的会话文件留档为 %s（原文件未被直接覆盖）\n", bak)
}

// Save 加密并落盘。超预算时按「最近更新时间」从旧到新丢弃整条**会话**。
func (ss *SessionStore) Save(snap sessionSnapshot) error {
	ss.backupUnreadableOnce()

	plain, err := ss.encode(snap)
	if err != nil {
		return err
	}
	// 丢**整条**会话，而不是截断某一条。
	//
	// 截断会破坏消息链结构：OpenAI 协议要求每条 tool 消息前面必须有
	// 携带同一 tool_call_id 的 assistant 消息。切一刀下去，那条会话
	// 此后**每一次**请求都会 400，而界面上只显示「模型调用失败」。
	// 丢整条最多是「那条会话的上下文没了」—— 用户能理解，也能重建。
	dropped := 0
	for countSessions(snap) > 0 && len(plain) > maxPersistedBytes {
		hostID, sessionID, ok := oldestRef(snap)
		if !ok {
			break
		}
		delete(snap[hostID], sessionID)
		if len(snap[hostID]) == 0 {
			// 顺手把空壳主机删掉：留下 map{} 会在文件里多一层无意义嵌套，
			// 也会让「有 N 台主机存了会话」这类计数虚高。
			delete(snap, hostID)
		}
		dropped++
		if plain, err = ss.encode(snap); err != nil {
			return err
		}
	}
	if dropped > 0 {
		// 丢弃必须留痕：用户下次发现某条会话的上下文没了，
		// 得能从日志里看出是「预算不足被淘汰」，而不是以为程序出错了。
		fmt.Fprintf(os.Stderr, "[ai-shell] 会话历史超出 %d 字节预算，已淘汰 %d 条会话\n",
			maxPersistedBytes, dropped)
	}

	blob, err := ss.sealer.Seal(plain)
	if err != nil {
		return fmt.Errorf("加密会话历史失败: %w", err)
	}
	if err := atomicWriteFile(ss.path, blob, 0o600); err != nil {
		return fmt.Errorf("写入会话历史失败: %w", err)
	}
	return nil
}

func (ss *SessionStore) encode(snap sessionSnapshot) ([]byte, error) {
	if snap == nil {
		snap = sessionSnapshot{}
	}
	return json.Marshal(sessionFile{Version: sessionFileVersion, Sessions: snap})
}

func countSessions(snap sessionSnapshot) int {
	n := 0
	for _, m := range snap {
		n += len(m)
	}
	return n
}

// oldestRef 找出「最近更新时间」最早的那条会话。
func oldestRef(snap sessionSnapshot) (hostID, sessionID string, ok bool) {
	var oldestAt time.Time
	for h, m := range snap {
		for id, w := range m {
			if !ok || w.UpdatedAt.Before(oldestAt) {
				hostID, sessionID, oldestAt, ok = h, id, w.UpdatedAt, true
			}
		}
	}
	return hostID, sessionID, ok
}

// sessionWire 是单个会话的落盘形态。
//
// 刻意与内存里的 session 分开：内存结构可以随实现自由调整，
// 而落盘格式一旦发布就必须向后兼容 —— 老版本写下的文件新版本要能读。
// 用同一个结构体会让「改内存字段名」变成「悄悄破坏磁盘格式」。
//
// bytes / archivedBytes 不落盘：它们是长度的派生值，读回来重算即可。
// 存下来反而多一个可能与实际不一致的字段。
type sessionWire struct {
	// Name 是用户起的会话名。**空名字与「没有名字」是同一件事**，
	// 所以这个字段决定了一条空会话要不要被保留：
	// 用户特意建的那条（有名字）必须留下，而默认会话只要没有内容就不必占地方。
	Name string `json:"name,omitempty"`

	Turns         [][]llm.Message `json:"turns,omitempty"`
	Archived      []string        `json:"archived,omitempty"`
	ArchivedTurns int             `json:"archivedTurns,omitempty"`
	Deep          string          `json:"deep,omitempty"`
	DeepTurns     int             `json:"deepTurns,omitempty"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// sessionFile 是 v2 的文件结构：主机 → 会话 → 状态。
type sessionFile struct {
	Version  int                               `json:"version"`
	Sessions map[string]map[string]sessionWire `json:"sessions"`
}

// sessionSnapshot 是「某一瞬间的全部会话」，从 Agent 的锁里取出、到锁外写盘。
// 外层 key 是主机 ID，内层是会话 ID。
type sessionSnapshot map[string]map[string]sessionWire

// Load 读出并解密会话历史。文件不存在时返回 (nil, nil) —— 那是首次运行，
// 不是错误。
//
// v1 文件（每台主机一条会话）会被读成「该主机的默认会话」，
// 于是升级后老用户的上下文原样还在，只是多了一个会话的概念。
func (ss *SessionStore) Load() (map[string]map[string]*session, error) {
	blob, err := os.ReadFile(ss.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取会话历史失败: %w", err)
	}
	plain, err := ss.sealer.Unseal(blob)
	if err != nil {
		return nil, fmt.Errorf("解密会话历史失败（主密钥可能已变更）: %w", err)
	}

	// 只先读出版本号与 sessions 那段的原始字节，因为两个版本里
	// sessions 的类型不同，必须等知道版本之后再解析。
	var head struct {
		Version  int             `json:"version"`
		Sessions json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(plain, &head); err != nil {
		return nil, fmt.Errorf("解析会话历史失败: %w", err)
	}

	switch head.Version {
	case sessionFileVersionV1:
		return loadV1(head.Sessions)
	case sessionFileVersionV2:
		return loadV2(head.Sessions)
	default:
		// 不认识的版本必须**报错**，不能当成空文件。
		// 当成空文件的话，下一次写入就会把它的内容永久覆盖掉 ——
		// 而用户看到的只是一句「载入失败，已从空白开始」。
		return nil, fmt.Errorf("会话文件版本是 %d，本程序只认 1 和 2；"+
			"它可能来自更新的版本，已停止载入以免覆盖", head.Version)
	}
}

func loadV1(raw json.RawMessage) (map[string]map[string]*session, error) {
	var legacy map[string]sessionWire
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, fmt.Errorf("解析 v1 会话历史失败: %w", err)
		}
	}
	out := make(map[string]map[string]*session, len(legacy))
	for hostID, w := range legacy {
		if s := w.toSession(); s != nil {
			out[hostID] = map[string]*session{DefaultSessionID: s}
		}
	}
	return out, nil
}

func loadV2(raw json.RawMessage) (map[string]map[string]*session, error) {
	var cur map[string]map[string]sessionWire
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cur); err != nil {
			return nil, fmt.Errorf("解析 v2 会话历史失败: %w", err)
		}
	}
	out := make(map[string]map[string]*session, len(cur))
	for hostID, byID := range cur {
		m := make(map[string]*session, len(byID))
		for sessionID, w := range byID {
			if s := w.toSession(); s != nil {
				m[normalizeSessionID(sessionID)] = s
			}
		}
		if len(m) > 0 {
			out[hostID] = m
		}
	}
	return out, nil
}

// toSession 把落盘形态还原成内存结构，并**重算派生值**。
func (w sessionWire) toSession() *session {
	s := &session{
		name:          w.Name,
		archived:      w.Archived,
		archivedTurns: w.ArchivedTurns,
		deep:          w.Deep,
		deepTurns:     w.DeepTurns,
		updatedAt:     w.UpdatedAt,
	}
	for _, r := range s.archived {
		s.archivedBytes += len(r)
	}
	// 只收完整的轮次。
	//
	// 文件可能是手改过的、被截断的、或由将来某个有 bug 的版本写下的。
	// 半轮对话的代价是那条会话**此后每次**请求都 400，而错误信息
	// 只有一句「模型调用失败」—— 排查成本极高。这里丢掉它是唯一
	// 廉价且安全的处理：少一轮历史，不会有人察觉。
	for _, t := range w.Turns {
		if !completeTurn(t) {
			continue
		}
		s.turns = append(s.turns, t)
		s.bytes += turnSize(t)
	}
	// 完全空的会话不保留 —— 但**有名字的例外**。
	//
	// 名字是用户特意起过的证据：他建了一条会话、可能还没来得及用就
	// 关了应用，重启后那条会话不该凭空消失（他会以为「新建没生效」）。
	// 而默认会话没有内容时本来就不该占地方。
	if len(s.turns) == 0 && len(s.archived) == 0 && s.deep == "" && s.name == "" {
		return nil
	}
	return s
}

// ---- Agent 侧的接线 ----

// snapshotLocked 取出一份可安全带到锁外的会话快照。
//
// **必须复制外层切片**：trim 会执行 `s.turns[0] = nil` 再 `s.turns = s.turns[1:]`，
// 前者写的是底层数组。若快照与内存共享同一个底层数组，锁释放后
// 另一次写入就能让快照里冒出一个 nil 轮次 —— 表现为落盘文件里多一条
// 空记录，或者序列化时直接崩。
//
// 内层 map 也必须新建：Save 超预算时会**删条目**来淘汰会话，
// 若它拿到的是 a.sessions 里那张内层 map，淘汰就会直接改掉内存状态 ——
// 用户看到的是「重启后某条会话没了」，而当前这次运行里它也已经被删了，
// 且这次删除没有任何日志解释（Save 的日志只说「淘汰了几条」）。
//
// 调用方必须持有 a.mu。
func (a *Agent) snapshotLocked() sessionSnapshot {
	snap := make(sessionSnapshot, len(a.sessions))
	for hostID, m := range a.sessions {
		inner := make(map[string]sessionWire, len(m))
		for sessionID, s := range m {
			turns := make([][]llm.Message, len(s.turns))
			copy(turns, s.turns)
			archived := make([]string, len(s.archived))
			copy(archived, s.archived)
			inner[sessionID] = sessionWire{
				Name:          s.name,
				Turns:         turns,
				Archived:      archived,
				ArchivedTurns: s.archivedTurns,
				Deep:          s.deep,
				DeepTurns:     s.deepTurns,
				UpdatedAt:     s.updatedAt,
			}
		}
		snap[hostID] = inner
	}
	return snap
}

// SetSessionStore 注入会话持久化，并**立即载入**磁盘上已有的历史。
//
// 载入失败不会阻断启动：会话历史是优化项，不是必需品。
// 但错误要回传给调用方 —— 由它决定是否提示用户，而不是静默咽掉。
// 无论载入成功与否，store 都会被装上，这样后续的写入仍然有效
// （文件坏了不该让「以后也存不进去」）。
//
// 载入失败时还会给 store 打个标记：第一次写入前先把读不懂的旧文件
// 改名留档，而不是直接覆盖（见 backupUnreadableOnce）。
// 不这么做的话，「文件来自更新的版本」会静默变成「数据永久丢失」。
func (a *Agent) SetSessionStore(s *SessionStore) error {
	if s == nil {
		return nil
	}
	a.mu.Lock()
	a.store = s
	a.mu.Unlock()

	loaded, err := s.Load()
	if err != nil {
		s.markUnreadable()
		return err
	}
	if len(loaded) == 0 {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for hostID, byID := range loaded {
		// 不覆盖已经存在的那些：载入发生在启动阶段，但若将来有人
		// 在运行中重新注入 store，覆盖会把用户当前正在用的上下文换掉。
		if _, exists := a.sessions[hostID]; exists {
			continue
		}
		a.sessions[hostID] = byID
	}
	return nil
}

// persist 把快照写到磁盘。
//
// 失败只报告，不影响主流程：一轮对话已经跑完了，不该因为写不了磁盘
// 而向用户报错。但**也不能静默** —— 用户以为历史存下来了、
// 重启后却发现没了，那是最难排查的一类问题。
func (a *Agent) persist(snap sessionSnapshot) {
	a.mu.Lock()
	st := a.store
	a.mu.Unlock()
	if st == nil {
		return
	}
	err := st.Save(snap)

	a.mu.Lock()
	firstFailure := err != nil && !a.persistErrReported
	if err != nil {
		// 只在**第一次**失败时报一次。磁盘满这类问题会每轮都失败，
		// 每轮都喊一遍只会把真正有用的告警淹掉。
		a.persistErrReported = true
	} else {
		a.persistErrReported = false // 恢复成功后重新武装，下次再坏还会报
	}
	a.mu.Unlock()

	// emit 放在锁外：事件最终会走到 Wails 运行时，没有理由让
	// agent 的锁横跨一次 IPC。
	if firstFailure {
		a.emit(EvError, map[string]string{
			"message": "会话历史落盘失败，重启后可能丢失：" + err.Error(),
		})
	}
}

// flushSessions 把当前全部会话立刻落盘一次。
// 给 Forget / ClearSession / 关机这类「状态刚被清掉」的时刻用 ——
// 不清盘的话，重启后已经被删掉的会话会**复活**。
func (a *Agent) flushSessions() {
	a.mu.Lock()
	snap := a.snapshotLocked()
	a.mu.Unlock()
	a.persist(snap)
}

// FlushSessions 是 flushSessions 的导出形式，供 App 在关机时兜底调用。
//
// 会话在每次变更时就已经落盘了，所以这不是主路径 —— 它的价值在于
// 万一将来某条写入路径漏了 flush，关一次应用还能补上。
func (a *Agent) FlushSessions() { a.flushSessions() }

// atomicWriteFile 先写临时文件再改名，避免写到一半崩溃留下半个文件。
//
// 与 vault 里的同名函数实现相同，这里刻意各留一份：为了 5 行工具代码
// 让 agent 包依赖 vault 的内部实现不划算，而这段逻辑本身没有任何
// 会分叉的业务含义。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sessionIDs 返回当前有会话的主机 id（按 id 排序，便于测试与排查）。
func (a *Agent) sessionIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.sessions))
	for id := range a.sessions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
