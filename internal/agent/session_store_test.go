package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"ai-shell/internal/llm"
	"ai-shell/internal/vault"
)

// ---- 会话落盘的测试 ----
//
// 落盘比内存多出两类风险，这组用例主要盯它们：
//   - **文件是加密的**：会话含远端命令输出，明文落盘等于把凭证库的
//     保护等级白白降下来。
//   - **读回来的东西是可信的吗**：文件可能被改坏、被截断、或由将来某个
//     有 bug 的版本写下。半轮对话会让那台主机此后每次请求都 400，
//     而界面上只显示「模型调用失败」—— 所以读的时候必须校验。

// newTestStore 用**真实 vault** 作 Sealer，顺带验证两个包的接口能对接上。
func newTestStore(t testing.TB) (*SessionStore, string) {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	return NewSessionStore(dir, v), dir
}

// oneHost 造一份「某台主机只有一条默认会话」的快照。
//
// 落盘格式升到 v2（每台主机若干条会话）之后，绝大多数用例仍然只关心
// 「一条会话能不能正确往返」，用这个帮手把嵌套那层收起来 ——
// 否则每个用例都要多写一层 map 字面量，读起来全是括号，
// 而多出来的那层跟被测行为毫无关系。
func oneHost(hostID string, w sessionWire) sessionSnapshot {
	return sessionSnapshot{hostID: {DefaultSessionID: w}}
}

func TestSessionStoreRoundTrip(t *testing.T) {
	ss, _ := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	snap := oneHost("h1", sessionWire{
		Turns: [][]llm.Message{
			simpleTurn("第一问", "第一答"),
			toolTurn("第二问", "c1", "<<<UNTRUSTED_REMOTE_OUTPUT>>>\n[exit=3, 9ms]\nboom"),
		},
		Archived:      []string{"· 用户：更早的提问\n"},
		ArchivedTurns: 5,
		Deep:          "压缩摘要正文",
		DeepTurns:     3,
		UpdatedAt:     now,
	})
	if err := ss.Save(snap); err != nil {
		t.Fatal(err)
	}

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := got["h1"][DefaultSessionID]
	if s == nil {
		t.Fatal("h1 的默认会话没读回来")
	}
	if len(s.turns) != 2 {
		t.Fatalf("应读回 2 轮，实得 %d", len(s.turns))
	}
	if s.turns[0][0].Content != "第一问" {
		t.Errorf("第一轮的提问丢了：%q", s.turns[0][0].Content)
	}
	// 派生值必须重算出来，否则下一轮 trim 会按 0 字节判断，永不裁剪。
	if s.bytes <= 0 {
		t.Errorf("bytes 应被重算为正数，实得 %d", s.bytes)
	}
	if s.archivedBytes != len(s.archived[0]) {
		t.Errorf("archivedBytes 应被重算，实得 %d", s.archivedBytes)
	}
	if s.archivedTurns != 5 || s.deep != "压缩摘要正文" || s.deepTurns != 3 {
		t.Errorf("归档与压缩字段没完整读回：%+v", s)
	}
	if !s.updatedAt.Equal(now) {
		t.Errorf("更新时间应保留（淘汰策略靠它排序），实得 %v", s.updatedAt)
	}
}

// 落盘文件必须是密文。会话里有远端命令输出，明文落盘等于把
// 「与凭证库同级保护」这句话作废。
func TestSessionStoreFileIsEncrypted(t *testing.T) {
	ss, _ := newTestStore(t)
	secret := "s3cr3t-from-remote-host"
	if err := ss.Save(oneHost("h1", sessionWire{
		Turns: [][]llm.Message{simpleTurn("问题", secret)}, UpdatedAt: time.Now(),
	})); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(ss.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("会话明文出现在了落盘文件里")
	}
	if strings.Contains(string(raw), "sessions") {
		t.Fatal("连字段名都不该以明文出现（说明根本没加密）")
	}

	// 权限断言只在类 Unix 上有意义。
	//
	// Windows 的 os.WriteFile 不映射 POSIX 位：实测无论传 0600 还是 0666，
	// Stat 都报 -rw-rw-rw-。这不是缺陷也不是遗漏 —— Windows 上的保护来自
	// 「主密钥托管在系统凭据管理器」，密文本身可以公开，这正是
	// 「加密静态存储」的含义。这条断言只是额外确认 Unix 上多了一层。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(ss.Path())
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("落盘文件权限应为 0600，实得 %v", info.Mode().Perm())
		}
	}
}

// 首次运行没有文件，这是正常情况而不是错误。
//
// 若这里返回错误，App.startup 会把「第一次装应用」报成故障。
func TestSessionStoreLoadMissingFileIsNotError(t *testing.T) {
	ss, _ := newTestStore(t)
	got, err := ss.Load()
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应返回空集合，实得 %d 条", len(got))
	}
}

// 文件损坏要报错，但不能崩 —— 由调用方决定降级。
func TestSessionStoreLoadCorruptFileReportsError(t *testing.T) {
	ss, _ := newTestStore(t)
	if err := os.WriteFile(ss.Path(), []byte("这不是密文"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Load(); err == nil {
		t.Fatal("密文损坏时应报错，而不是静默当成空历史")
	}
}

// 文件被改成合法 JSON 但内容不对（比如手改、或将来某个有 bug 的版本写下的）
// 也不能崩，且半轮对话必须被丢掉。
func TestSessionStoreDropsIncompleteTurnsOnLoad(t *testing.T) {
	ss, _ := newTestStore(t)
	// 直接构造「解密后」的内容再加密写盘，绕开 Save 的校验。
	half := []llm.Message{
		{Role: "user", Content: "只有提问"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "run_command", Arguments: `{}`},
		}}},
	}
	writeRawSessionFile(t, ss, sessionFile{
		Version: sessionFileVersion,
		Sessions: map[string]map[string]sessionWire{
			"h1": {DefaultSessionID: {
				Turns:     [][]llm.Message{simpleTurn("完整的一轮", "答"), half},
				UpdatedAt: time.Now(),
			}},
		},
	})

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := got["h1"][DefaultSessionID]
	if s == nil {
		t.Fatal("应至少读回一条会话")
	}
	if len(s.turns) != 1 {
		t.Fatalf("半轮对话必须被丢弃，实得 %d 轮", len(s.turns))
	}
	if s.turns[0][0].Content != "完整的一轮" {
		t.Errorf("完整的那一轮应保留，实得 %q", s.turns[0][0].Content)
	}
}

// 超预算时按「最近更新时间」从旧到新丢**整条**会话。
//
// 丢整条而不是截断某一条：截断会破坏消息链结构（tool 消息前面
// 没有对应的 assistant），那台主机此后每次请求都 400。
//
// 这里特意把「最旧」和「最新」放在**同一台主机**上：淘汰的粒度是会话，
// 不是主机。若哪天有人把它写成「按主机丢」，那么 h-a 会因为有一条旧会话
// 而被整台丢掉，连带刚聊过的 new 一起消失 —— 断言就是冲着这个来的。
func TestSessionStoreEvictsOldestWhenOverBudget(t *testing.T) {
	ss, _ := newTestStore(t)
	old := maxPersistedBytes
	maxPersistedBytes = 900 // 压到几百字节，够装两条、装不下三条
	t.Cleanup(func() { maxPersistedBytes = old })

	base := time.Now().Truncate(time.Second)
	mk := func(q string, at time.Time) sessionWire {
		return sessionWire{
			Turns:     [][]llm.Message{simpleTurn(q, strings.Repeat("答", 100))},
			UpdatedAt: at,
		}
	}
	// h-a 上一条最旧、一条最新；h-b 上一条居中。
	err := ss.Save(sessionSnapshot{
		"h-a": {
			"old": mk("最早", base.Add(-2*time.Hour)),
			"new": mk("最新", base),
		},
		"h-b": {
			"mid": mk("居中", base.Add(-1*time.Hour)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(ss.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxPersistedBytes*4 {
		// 密文有 base64/头部开销，宽一点；但明显失控就说明淘汰没生效。
		t.Fatalf("落盘 %d 字节，淘汰策略似乎没生效（预算 %d）", len(raw), maxPersistedBytes)
	}

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got["h-a"]["new"] == nil {
		t.Error("最新的会话必须保留 —— 哪怕它和一条旧会话同主机")
	}
	if got["h-a"]["old"] != nil {
		t.Error("最旧的会话应被淘汰")
	}
	if got["h-b"]["mid"] == nil {
		t.Error("居中的会话不该被淘汰")
	}
	if n := len(got["h-a"]) + len(got["h-b"]); n >= 3 {
		t.Errorf("应至少淘汰一条，实得 %d 条", n)
	}
}

// 快照不能与内存共享底层数组。
//
// trim 会执行 `s.turns[0] = nil` 再 `s.turns = s.turns[1:]`，前者写的是
// **底层数组**。若快照共享同一个数组，锁释放后另一次裁剪就能让快照里
// 冒出一个 nil 轮次 —— 表现为落盘文件里多一条空记录。
//
// 这里**直接调 trim**，而不是再走一次 remember：remember 内部会先 append，
// 而 append 在容量不足时会重新分配数组，那样反而意外地保护了快照 ——
// 第一版就是这么写的，变异测试发现「去掉复制后用例仍绿」才暴露出来。
func TestSnapshotDoesNotAliasSessionTurns(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())
	rememberDefault(a, "h1", simpleTurn("二", "2"), defaultLimits())

	a.mu.Lock()
	snap := a.snapshotLocked()
	// 模拟锁释放后另一条路径触发的裁剪。trim 只做「置 nil + 前移」，
	// 不涉及 append，所以必然写到共享的那个数组上。
	a.sessions["h1"][DefaultSessionID].trim(limitsWith(func(l *sessionLimits) { l.turns = 1 }))
	a.mu.Unlock()

	w := snap["h1"][DefaultSessionID]
	if len(w.Turns) != 2 {
		t.Fatalf("快照应固定为取快照那一刻的 2 轮，实得 %d", len(w.Turns))
	}
	for i, tr := range w.Turns {
		if tr == nil {
			t.Fatalf("快照第 %d 轮变成了 nil —— 说明与内存共享了底层数组", i)
		}
		if len(tr) == 0 {
			t.Fatalf("快照第 %d 轮是空的", i)
		}
	}
}

// ---- Agent 侧的接线 ----

// 记住一轮 → 换一个 Agent 用同一个目录 → 历史必须还在。
// 这是整个落盘功能存在的意义。
func TestAgentReloadsSessionAfterRestart(t *testing.T) {
	ss, dir := newTestStore(t)

	a1 := New(nil, nil, noEmit)
	if err := a1.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}
	rememberDefault(a1, "h1", simpleTurn("重启前的问题", "重启前的回答"), defaultLimits())
	rememberDefault(a1, "h1", simpleTurn("第二问", "第二答"), defaultLimits())

	// 模拟重启：新建一个 Sealer + Agent，指向同一个目录。
	t.Setenv("AISHELL_KEYFILE", "1")
	v2 := vault.New(dir)
	if err := v2.Open(); err != nil {
		t.Fatal(err)
	}
	a2 := New(nil, nil, noEmit)
	if err := a2.SetSessionStore(NewSessionStore(dir, v2)); err != nil {
		t.Fatal(err)
	}

	hist := histDefault(a2, "h1")
	if len(hist) != 4 {
		t.Fatalf("重启后应读回 4 条消息（2 轮），实得 %d: %v", len(hist), hist)
	}
	if hist[0].Content != "重启前的问题" {
		t.Errorf("最早的一条丢了：%q", hist[0].Content)
	}
}

// 清空上下文之后必须落盘 —— 否则重启后被清掉的会话会「复活」，
// 用户看到模型又记得了，只会以为清空按钮没生效。
func TestClearSessionDoesNotResurrectAfterRestart(t *testing.T) {
	ss, dir := newTestStore(t)
	a1 := New(nil, nil, noEmit)
	if err := a1.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}
	rememberDefault(a1, "h1", simpleTurn("问题", "回答"), defaultLimits())

	if n, busy := a1.ClearSession("h1", DefaultSessionID); n != 1 || busy {
		t.Fatalf("清空应返回 (1,false)，实得 (%d,%v)", n, busy)
	}

	a2 := reloadAgent(t, dir)
	if hist := histDefault(a2, "h1"); len(hist) != 0 {
		t.Fatalf("重启后会话不该复活，实得 %d 条: %v", len(hist), hist)
	}
}

// 删除主机后同样不能复活。
func TestForgetDoesNotResurrectAfterRestart(t *testing.T) {
	ss, dir := newTestStore(t)
	a1 := New(nil, nil, noEmit)
	if err := a1.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}
	rememberDefault(a1, "h1", simpleTurn("问题", "回答"), defaultLimits())
	a1.Forget("h1")

	a2 := reloadAgent(t, dir)
	if hist := histDefault(a2, "h1"); len(hist) != 0 {
		t.Fatalf("删除主机后会话不该复活，实得 %d 条", len(hist))
	}
}

// 压缩结果也要落盘 —— 那是用户花了一次 API 调用换来的东西，
// 重启就丢等于白花。
func TestCompactedSummarySurvivesRestart(t *testing.T) {
	ss, dir := newTestStore(t)
	a1 := New(nil, nil, noEmit)
	if err := a1.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}
	rememberDefault(a1, "h1", simpleTurn("问题", "回答"), defaultLimits())

	// 直接写入 deep 字段，绕开需要 LLM 的 CompactSession。
	a1.mu.Lock()
	s := a1.sessions["h1"][DefaultSessionID]
	s.deep = "压缩摘要：磁盘已排查。"
	s.deepTurns = 1
	s.turns = nil
	s.bytes = 0
	s.updatedAt = time.Now()
	snap := a1.snapshotLocked()
	a1.mu.Unlock()
	a1.persist(snap)

	a2 := reloadAgent(t, dir)
	sum := a2.sessionSummaryFor("h1", DefaultSessionID)
	if !strings.Contains(sum, "磁盘已排查") {
		t.Fatalf("压缩摘要应跨重启保留，实得:\n%s", sum)
	}
}

// 落盘失败只报**一次**。
//
// 磁盘满这类问题会每轮都失败；每轮都喊一遍会把真正有用的告警淹掉，
// 而用户对反复出现的红色错误很快就会脱敏。
func TestPersistFailureReportedOnlyOnce(t *testing.T) {
	var got []string
	emit := func(ev string, payload any) {
		if ev == EvError {
			if m, ok := payload.(map[string]string); ok {
				got = append(got, m["message"])
			}
		}
	}
	a := New(nil, nil, emit)

	// 造一个必然写不进去的路径：把「文件」当目录用。
	// 比「不存在的目录」可靠 —— 后者在有的实现里会被顺手创建出来。
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.store = &SessionStore{path: filepath.Join(blocker, "sessions.enc"), sealer: okSealer{}}

	a.persist(sessionSnapshot{})
	a.persist(sessionSnapshot{})
	a.persist(sessionSnapshot{})

	if len(got) != 1 {
		t.Fatalf("落盘连续失败三次只应报一次，实得 %d 次: %v", len(got), got)
	}
	if !strings.Contains(got[0], "落盘失败") {
		t.Errorf("告警文案应说明是落盘失败，实得 %q", got[0])
	}
}

// 没有注入 store 时（单元测试、vault 未解锁）落盘必须是**静默空操作**，
// 不能报错也不能崩。
func TestPersistIsNoopWithoutStore(t *testing.T) {
	var got []string
	emit := func(ev string, _ any) { got = append(got, ev) }
	a := New(nil, nil, emit)
	a.persist(sessionSnapshot{"h1": {}})
	if len(got) != 0 {
		t.Fatalf("未启用落盘时不该发任何事件，实得 %v", got)
	}
}

// 载入失败不能让 SetSessionStore 之后的写入也失效 ——
// 文件坏了不该变成「以后也存不进去」。
func TestStoreStillUsableAfterLoadFailure(t *testing.T) {
	ss, _ := newTestStore(t)
	if err := os.WriteFile(ss.Path(), []byte("坏掉的密文"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := New(nil, nil, noEmit)
	if err := a.SetSessionStore(ss); err == nil {
		t.Fatal("载入损坏文件应返回错误供调用方提示")
	}
	// 关键：store 仍然装着，后续写入要能覆盖掉那个坏文件。
	rememberDefault(a, "h1", simpleTurn("问题", "回答"), defaultLimits())
	if _, err := ss.Load(); err != nil {
		t.Fatalf("一次成功写入后文件应恢复可读: %v", err)
	}
}

// ---- 辅助 ----

// reloadAgent 模拟「重启应用」：用同一个目录新建 vault 与 Agent。
func reloadAgent(t *testing.T, dir string) *Agent {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil, noEmit)
	if err := a.SetSessionStore(NewSessionStore(dir, v)); err != nil {
		t.Fatal(err)
	}
	return a
}

// writeRawSessionFile 把「解密后」的结构加密写盘，用来构造 Save 造不出的状态。
func writeRawSessionFile(t *testing.T, ss *SessionStore, f sessionFile) {
	t.Helper()
	writeRawJSON(t, ss, f)
}

// writeRawJSON 把任意结构加密写盘，并返回写下去的密文。
//
// 比 writeRawSessionFile 更宽：构造「来自更新版本的文件」时
// 需要写出一个 sessionFile 表达不了的结构（比如 version=99），
// 而那种文件恰恰是最需要验证的一条路径 —— 它的失败方式是
// 「静默覆盖，用户数据永久丢失」。
func writeRawJSON(t *testing.T, ss *SessionStore, v any) []byte {
	t.Helper()
	plain, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := ss.sealer.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(ss.path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return blob
}

// okSealer 是恒等 Sealer，只用于「落盘路径本身写不进去」的用例 ——
// 那里要考察的是错误处理，不是加密。
type okSealer struct{}

func (okSealer) Seal(p []byte) ([]byte, error)   { return p, nil }
func (okSealer) Unseal(b []byte) ([]byte, error) { return b, nil }

// persist 在**每一轮对话结束时**跑一次（remember 里），所以它的开销
// 直接叠在用户等待上。加个基准让「以后往会话里加字段」的代价可见 ——
// 这个包里的脱敏层与摘要层都有基准，落盘层不该是盲区。
//
// 满负荷场景：10 台主机、每台都顶到 256KiB 上限 —— 也就是最坏情况。
func BenchmarkSessionPersist(b *testing.B) {
	ss, _ := newTestStore(b)
	snap := make(sessionSnapshot, 10)
	payload := strings.Repeat("远端命令输出的一行内容，用来把会话撑到上限。\n", 4000) // ≈256KiB
	for i := 0; i < 10; i++ {
		id := "h-" + strconv.Itoa(i)
		turns := make([][]llm.Message, 0, 8)
		for j := 0; j < 8; j++ {
			turns = append(turns, []llm.Message{
				{Role: "user", Content: "第 " + strconv.Itoa(j) + " 问"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{
					ID: "c", Type: "function",
					Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"ls"}`},
				}}},
				{Role: "tool", ToolCallID: "c", Name: "run_command",
					Content: payload[:len(payload)/8]},
				{Role: "assistant", Content: "结论。"},
			})
		}
		snap[id] = map[string]sessionWire{DefaultSessionID: {Turns: turns, UpdatedAt: time.Now()}}
	}

	// 先确认样本确实接近满负荷，免得基准在测一个空对象。
	if plain, err := ss.encode(snap); err != nil || len(plain) < 2<<20 {
		b.Fatalf("基准样本太小（%d 字节），没测到满负荷路径", len(plain))
	}

	b.SetBytes(int64(10 * 256 << 10))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ss.Save(snap); err != nil {
			b.Fatal(err)
		}
	}
}
