package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ---- 会话的增删改查 ----
//
// 每台主机天生有一条默认会话（DefaultSessionID），它**不能被删除**。
// 这是本文件要守住的核心不变式：任何时刻，每台主机都至少有一条会话可用。
//
// 有了它，Run / ClearSession / CompactSession 都能接受一个空会话 ID
// 而不会落进「这台机器一条会话都没有」这个需要到处特判的状态 ——
// 那种状态一旦出现，界面上每一个跟会话有关的地方都要多一个分支，
// 而漏掉任何一处就会表现成「点了没反应」。

const (
	// maxSessionNameRunes 是会话名的长度上限，按**字符**数而不是字节数。
	// 按字节算的话，中文名字会在第 13 个字左右被莫名其妙截断。
	maxSessionNameRunes = 40

	// maxSessionsPerHost 是每台主机的会话数上限。
	//
	// 必须有这个数：单条会话的内存上限是 256KiB，落盘总量上限是 8MiB，
	// 没有闸门的话，一台主机上建几十条会话就能把落盘预算吃光 ——
	// 而淘汰是按「最旧」丢的，用户会看到**别的**主机的历史莫名消失。
	// 顺带一提，界面上超过十几条也选不过来了。
	maxSessionsPerHost = 20
)

// SessionInfo 是一条会话的元信息，供界面列出与选择。
//
// 只带元信息、不带历史正文：会话列表要能一次拉全，
// 而每条历史本身可能有 256KiB，把正文塞进列表等于每次刷新都传几 MB。
type SessionInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Turns         int       `json:"turns"`         // 当前留在上下文里的轮数
	ArchivedTurns int       `json:"archivedTurns"` // 已压成摘要/记录的轮数
	UpdatedAt     time.Time `json:"updatedAt"`
	IsDefault     bool      `json:"isDefault"`
}

// info 把会话压成元信息。
func (s *session) info(id string) SessionInfo {
	return SessionInfo{
		ID:            id,
		Name:          s.name,
		Turns:         len(s.turns),
		ArchivedTurns: s.archivedTurns,
		UpdatedAt:     s.updatedAt,
		IsDefault:     id == DefaultSessionID,
	}
}

// ListSessions 列出某台主机的全部会话。默认会话**总是**在列表里。
func (a *Agent) ListSessions(hostID string) []SessionInfo {
	a.mu.Lock()
	defer a.mu.Unlock()

	m := a.sessions[hostID]
	out := make([]SessionInfo, 0, len(m)+1)
	for id, s := range m {
		out = append(out, s.info(id))
	}
	if _, ok := m[DefaultSessionID]; !ok {
		// 默认会话可能还没被写过（不在 map 里），但它确实存在。
		// 列表里必须补上它 —— 否则会出现这种事：用户第一次提问的内容
		// 进了默认会话，而列表里根本没有那一项，他在界面上选不到它，
		// 也就永远看不到、清不掉自己的第一段对话。
		out = append(out, SessionInfo{ID: DefaultSessionID, IsDefault: true})
	}

	sort.Slice(out, func(i, j int) bool {
		// 默认会话永远排第一：它是兜底项，位置稳定用户才找得到。
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		// 其余的按最近使用倒序 —— 刚用过的排在前面，符合选择器的直觉。
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		// 时间相同（同一毫秒内建的）时按 ID 定序，避免每次调用顺序都不同：
		// 顺序不稳的列表在界面上会「自己跳动」，看起来像 bug。
		return out[i].ID < out[j].ID
	})
	return out
}

// CreateSession 新建一条空会话。
func (a *Agent) CreateSession(hostID, name string) (SessionInfo, error) {
	clean, err := normalizeSessionName(name)
	if err != nil {
		return SessionInfo{}, err
	}

	a.mu.Lock()
	m := a.sessions[hostID]
	if m == nil {
		m = map[string]*session{}
		a.sessions[hostID] = m
	}
	// 默认会话可能还没被写过（不在 map 里），但它确实占一个位置。
	// 不把它算进去的话，用户能建出「上限条具名会话 + 1 条默认」，
	// 实际比上限多一条。
	used := len(m)
	if _, ok := m[DefaultSessionID]; !ok {
		used++
	}
	if used >= maxSessionsPerHost {
		a.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("这台主机最多只能有 %d 条会话，先删掉一些再建", maxSessionsPerHost)
	}

	id := newSessionID(m)
	s := &session{name: clean, updatedAt: time.Now()}
	m[id] = s
	info := s.info(id)
	snap := a.snapshotLocked()
	a.mu.Unlock()

	// 必须立刻落盘：否则「新建 → 重启」之后那条会话会消失，
	// 用户会以为新建没生效（而它确实在列表里出现过一瞬）。
	a.persist(snap)
	return info, nil
}

// RenameSession 改会话名。
//
// 默认会话**可以**改名：它是由 ID 认定的，不是由名字认定的。
// 用户把「默认」改成「nginx 排查」之后，兜底的那条还是它。
func (a *Agent) RenameSession(hostID, sessionID, name string) error {
	sessionID = normalizeSessionID(sessionID)
	clean, err := normalizeSessionName(name)
	if err != nil {
		return err
	}

	a.mu.Lock()
	s := a.sessionOf(hostID, sessionID)
	if s == nil {
		a.mu.Unlock()
		return fmt.Errorf("这条会话已经不存在了（可能在别处被删掉），请刷新后重试")
	}
	s.name = clean
	s.updatedAt = time.Now()
	snap := a.snapshotLocked()
	a.mu.Unlock()

	a.persist(snap)
	return nil
}

// DeleteSession 删掉一条会话。
//
// 默认会话不能删：它是「不带会话 ID 地跑一轮」的落点（见 DefaultSessionID）。
// 想让它空掉用 ClearSession —— 那才是用户真正想要的效果。
func (a *Agent) DeleteSession(hostID, sessionID string) error {
	sessionID = normalizeSessionID(sessionID)
	if sessionID == DefaultSessionID {
		return fmt.Errorf("默认会话不能删除，用「清空上下文」把它清空即可")
	}

	a.mu.Lock()
	m := a.sessions[hostID]
	if m == nil {
		a.mu.Unlock()
		return fmt.Errorf("这条会话已经不存在了（可能在别处被删掉），请刷新后重试")
	}
	if _, ok := m[sessionID]; !ok {
		a.mu.Unlock()
		return fmt.Errorf("这条会话已经不存在了（可能在别处被删掉），请刷新后重试")
	}
	delete(m, sessionID)
	if len(m) == 0 {
		delete(a.sessions, hostID)
	}
	snap := a.snapshotLocked()
	a.mu.Unlock()

	// 必须立刻落盘：不落的话重启后它会**复活**，
	// 用户看到刚删掉的会话又回来了，只会以为删除按钮坏了。
	a.persist(snap)
	return nil
}

// normalizeSessionName 清洗用户输入的会话名。
//
// 会话名不进 LLM 上下文（它只是界面上的一个标签），所以这里防的**不是**
// 提示注入，而是三件会让界面出问题的事：
//   - 控制字符（换行、制表、ANSI 转义）会把列表项撑破或让文字错位；
//   - 全空白等于没起名，列表里会出现一个看不出内容的空条目；
//   - 过长会把选择器挤爆。
func normalizeSessionName(name string) (string, error) {
	// 先把控制字符换成空格再折叠连续空白。
	// 直接删掉的话 "a\nb" 会变成 "ab"，看起来像是用户自己少打了字 ——
	// 而用户看到的列表名与他输入的不一致时，他会怀疑是程序改错了名字。
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	if cleaned == "" {
		return "", fmt.Errorf("会话名不能为空")
	}
	// 超长按字符截断而不是报错：用户多半只是粘贴了一长串，
	// 让他自己回去数到第 41 个字更烦人。
	if runes := []rune(cleaned); len(runes) > maxSessionNameRunes {
		cleaned = string(runes[:maxSessionNameRunes])
	}
	return cleaned, nil
}

// newSessionID 生成一个不与现有会话冲突的 ID。
//
// 用随机而不是递增序号：序号会随删除被复用，而复用意味着
// 「一条已删会话的落盘记录可能被新会话认领」—— 一旦哪天有别的路径
// 按 ID 缓存过会话，那就是两台不同的对话串在一起。
func newSessionID(existing map[string]*session) string {
	for i := 0; i < 8; i++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			break // 随机源不可用，走下面的兜底
		}
		id := "s" + hex.EncodeToString(b[:])
		if _, taken := existing[id]; !taken {
			return id
		}
	}
	// 兜底：用纳秒时间戳，并在冲突时继续往后挪。
	// 循环一定会结束 —— 每次挪一格，而 existing 是有限的。
	for i := int64(0); ; i++ {
		id := fmt.Sprintf("s%d", time.Now().UnixNano()+i)
		if _, taken := existing[id]; !taken {
			return id
		}
	}
}
