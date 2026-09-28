package agent

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"ai-shell/internal/llm"
)

// ---- 落盘格式的版本与迁移 ----
//
// 这一组用例盯的是**最坏的一类失败**：不是报错，而是静默地把用户数据
// 永久覆盖掉。会话文件里装着用户跨重启的排查上下文，以及他花 API 调用
// 换来的压缩摘要 —— 覆盖了就没有第二次机会。
//
// 版本号的引入正是为了这个。文件里必须能区分三种情况：
//   - v1：老版本写的，要**读得懂**（升级不能丢历史）；
//   - v2：当前版本；
//   - 其它：不认识的，要**拒绝读**并且拒绝覆盖。

// v1 文件（每台主机一条会话）必须被读成「该主机的默认会话」。
//
// 这是升级路径的契约：老用户装上新版本，打开就发现上下文还在，
// 只是多了一个「会话」的概念。如果这里读不出来，表现是
// 「升级之后历史全没了」—— 而用户不会想到去翻备份。
func TestSessionStoreReadsV1FileAsDefaultSession(t *testing.T) {
	ss, _ := newTestStore(t)
	at := time.Now().Truncate(time.Second)

	// 刻意手写 JSON 而不是用 sessionFile：那个结构体已经是 v2 的形状了，
	// 用它造不出 v1 的文件。用字面量反而更贴近「老版本真的写下了什么」。
	writeRawJSON(t, ss, map[string]any{
		"version": sessionFileVersionV1,
		"sessions": map[string]any{
			"h1": map[string]any{
				"turns": [][]llm.Message{
					simpleTurn("升级前的问题", "升级前的回答"),
					simpleTurn("升级前的第二问", "升级前的第二答"),
				},
				"updatedAt": at,
			},
		},
	})

	got, err := ss.Load()
	if err != nil {
		t.Fatalf("v1 文件必须能读出来: %v", err)
	}
	if len(got["h1"]) != 1 {
		t.Fatalf("v1 的每台主机应恰好读成 1 条会话，实得 %d 条", len(got["h1"]))
	}
	s := got["h1"][DefaultSessionID]
	if s == nil {
		t.Fatal("v1 的那条会话应落在默认会话上")
	}
	if len(s.turns) != 2 {
		t.Fatalf("应读回 2 轮，实得 %d", len(s.turns))
	}
	if s.turns[0][0].Content != "升级前的问题" {
		t.Errorf("最早的提问丢了：%q", s.turns[0][0].Content)
	}
	if !s.updatedAt.Equal(at) {
		t.Errorf("更新时间应保留，实得 %v", s.updatedAt)
	}
}

// 读出来的 v1 会话在**下次写入**之后要变成 v2，且内容不变。
//
// 只测「读得懂」是不够的：若升级后写回去还是 v1，那么这个文件
// 永远停留在老格式上，而 v1 表达不了「一台主机多条会话」——
// 用户一旦建了第二条会话，它就会在下次读取时凭空消失。
func TestV1FileIsUpgradedToV2OnNextWrite(t *testing.T) {
	ss, _ := newTestStore(t)
	writeRawJSON(t, ss, map[string]any{
		"version": sessionFileVersionV1,
		"sessions": map[string]any{
			"h1": map[string]any{
				"turns":     [][]llm.Message{simpleTurn("老问题", "老回答")},
				"updatedAt": time.Now(),
			},
		},
	})

	a := New(nil, nil, noEmit)
	if err := a.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}
	// 一条新会话 —— 这一步会带着全部会话重新落盘。
	rememberDefault(a, "h1", simpleTurn("新问题", "新回答"), defaultLimits())

	plain, err := ss.sealer.Unseal(mustReadFile(t, ss.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), `"version":2`) {
		t.Errorf("重新写入后应是 v2，实得：%s", plain)
	}

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := got["h1"][DefaultSessionID]
	if s == nil || len(s.turns) != 2 {
		t.Fatalf("升级写入后应保留原有的 2 轮，实得 %+v", s)
	}
	if s.turns[0][0].Content != "老问题" {
		t.Errorf("升级过程中丢了老数据：%q", s.turns[0][0].Content)
	}
}

// 不认识的版本必须**报错**，绝不能当成空文件。
//
// 当成空文件的后果是不可逆的：载入成功 → 用户以为没事 → 下一轮对话
// 结束时就按「只有这几条会话」重新写盘，那份看不懂的数据就此消失。
// 而用户看到的只是一句「载入失败，已从空白开始」。
func TestSessionStoreRejectsUnknownVersion(t *testing.T) {
	ss, _ := newTestStore(t)
	writeRawJSON(t, ss, map[string]any{
		"version":  99,
		"sessions": map[string]any{"h1": map[string]any{}},
	})

	got, err := ss.Load()
	if err == nil {
		t.Fatalf("不认识的版本应报错，实得 (nil, nil) —— 这会让下一次写入覆盖掉它")
	}
	if got != nil {
		t.Error("报错时不该同时返回半份数据")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("错误里应带上那个版本号，方便排查，实得 %q", err.Error())
	}
}

// 读不懂的旧文件在**第一次覆盖它之前**要先留档。
//
// 这是「拒绝覆盖」的最后一公里：Load 报错了，但写入路径照样会写。
// 不留档的话，那次写入就是永久删除 —— 而且是在用户毫不知情的情况下
// （他只看到一句「载入失败」）。
func TestUnreadableFileIsBackedUpBeforeFirstWrite(t *testing.T) {
	ss, _ := newTestStore(t)
	orig := writeRawJSON(t, ss, map[string]any{"version": 99, "sessions": map[string]any{}})

	a := New(nil, nil, noEmit)
	if err := a.SetSessionStore(ss); err == nil {
		t.Fatal("不认识的版本应返回错误供调用方提示")
	}
	// 此刻**还不能**动文件：用户可能只是开了一下应用、什么都没做，
	// 而改名会让他在自己的备份脚本里看到文件莫名消失。
	if _, err := os.Stat(ss.path); err != nil {
		t.Fatalf("在真正写入之前不该动这个文件: %v", err)
	}

	rememberDefault(a, "h1", simpleTurn("新问题", "新回答"), defaultLimits())

	bak := ss.path + ".unreadable"
	kept, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("读不懂的旧文件应被留档而不是直接覆盖: %v", err)
	}
	if !bytes.Equal(kept, orig) {
		t.Error("留档文件与原始文件不一致 —— 那这份留档就没有意义了")
	}
	// 「留档 + 继续写」必须两件都做到。只留档不写新文件的话，
	// 用户会发现历史功能整个失效了。
	got, err := ss.Load()
	if err != nil {
		t.Fatalf("留档之后新写入的文件应可读: %v", err)
	}
	if got["h1"][DefaultSessionID] == nil {
		t.Error("新写入的会话没读回来")
	}
}

// 留档只能做**一次**。
//
// 若每次 Save 都改名，第二次写入会把刚写好的新文件搬去 .unreadable，
// 于是那份原始数据被覆盖掉 —— 正好是留档想避免的事。
func TestUnreadableBackupHappensOnlyOnce(t *testing.T) {
	ss, _ := newTestStore(t)
	writeRawJSON(t, ss, map[string]any{"version": 99, "sessions": map[string]any{}})

	a := New(nil, nil, noEmit)
	_ = a.SetSessionStore(ss)
	rememberDefault(a, "h1", simpleTurn("一", "1"), defaultLimits())

	bak := ss.path + ".unreadable"
	first, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("第一次写入前应留档: %v", err)
	}

	rememberDefault(a, "h1", simpleTurn("二", "2"), defaultLimits())

	second, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("留档文件不该消失: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("第二次写入又改了一次留档 —— 原始数据已经被覆盖，留档失去意义")
	}
}

// 有名字的空会话必须跨重启保留；没有名字也没有内容的则不占地方。
//
// 这两半要一起测，因为它们由**同一个判据**（name == ""）决定。
// 只测一半的话，很容易把判据写成「凡是空的都丢」，于是用户
// 「新建 → 还没来得及用 → 关掉应用」之后会发现那条会话不见了。
func TestEmptySessionKeptOnlyWhenNamed(t *testing.T) {
	ss, _ := newTestStore(t)

	if err := ss.Save(sessionSnapshot{
		"h1": {
			DefaultSessionID: {}, // 没内容也没名字：不该占地方
			"s1":             {Name: "还没用过的会话", UpdatedAt: time.Now()},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got["h1"]["s1"] == nil {
		t.Fatal("有名字的空会话必须保留 —— 否则用户会以为「新建」没生效")
	}
	if got["h1"]["s1"].name != "还没用过的会话" {
		t.Errorf("会话名应原样读回，实得 %q", got["h1"]["s1"].name)
	}
	if got["h1"][DefaultSessionID] != nil {
		t.Error("没有名字也没有内容的会话不该占地方")
	}
}

// v2 的文件里，同一台主机的多条会话要各自独立地往返。
//
// 这是 v2 存在的全部理由。若哪天有人把内层 map 写成了「后写覆盖先写」
// 或者共用了同一个 *session，表现就是「两条会话的内容串在一起」——
// 而那种 bug 在单会话用例里永远测不出来。
func TestMultipleSessionsRoundTripIndependently(t *testing.T) {
	ss, _ := newTestStore(t)
	if err := ss.Save(sessionSnapshot{
		"h1": {
			DefaultSessionID: {Turns: [][]llm.Message{simpleTurn("默认的问", "默认的答")}, UpdatedAt: time.Now()},
			"s1":             {Name: "nginx 排查", Turns: [][]llm.Message{simpleTurn("nginx 的问", "nginx 的答")}, UpdatedAt: time.Now()},
			"s2":             {Name: "磁盘排查", Turns: [][]llm.Message{simpleTurn("磁盘的问", "磁盘的答")}, UpdatedAt: time.Now()},
		},
		"h2": {
			DefaultSessionID: {Turns: [][]llm.Message{simpleTurn("另一台的问", "另一台的答")}, UpdatedAt: time.Now()},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ss.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应读回 2 台主机，实得 %d", len(got))
	}
	if len(got["h1"]) != 3 {
		t.Fatalf("h1 应有 3 条会话，实得 %d", len(got["h1"]))
	}
	cases := []struct{ sessionID, want string }{
		{DefaultSessionID, "默认的问"},
		{"s1", "nginx 的问"},
		{"s2", "磁盘的问"},
	}
	for _, c := range cases {
		s := got["h1"][c.sessionID]
		if s == nil {
			t.Errorf("%s 没读回来", c.sessionID)
			continue
		}
		if len(s.turns) != 1 || s.turns[0][0].Content != c.want {
			t.Errorf("%s 的内容串了：%+v", c.sessionID, s.turns)
		}
	}
	if got["h2"][DefaultSessionID].turns[0][0].Content != "另一台的问" {
		t.Error("h2 的内容串了")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
