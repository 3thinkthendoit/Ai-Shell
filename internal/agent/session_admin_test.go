package agent

import (
	"strings"
	"testing"
	"time"
)

// ---- 会话的增删改查 ----
//
// 这一组用例围绕一条不变式：**每台主机永远至少有一条会话可用**（默认会话）。
// 有了它，Run / ClearSession / CompactSession 都能接受一个空会话 ID，
// 而不必到处特判「这台机器一条会话都没有」那种状态 ——
// 那种状态一旦漏掉一处，表现就是「点了没反应」。

// seedSession 直接往内存里塞一条会话，用来精确控制 updatedAt 与名字。
//
// 不走 remember / CreateSession：那两个都会把 updatedAt 设成 time.Now()，
// 而排序用例要的恰恰是一个确定的顺序。
func seedSession(a *Agent, hostID, sessionID, name string, at time.Time, turns int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.sessions[hostID]
	if m == nil {
		m = map[string]*session{}
		a.sessions[hostID] = m
	}
	s := &session{name: name, updatedAt: at}
	for i := 0; i < turns; i++ {
		t := simpleTurn("问", "答")
		s.turns = append(s.turns, t)
		s.bytes += turnSize(t)
	}
	m[sessionID] = s
}

// 从没聊过的主机，列表里也必须有一条默认会话。
//
// 少了它会出现这种事：用户第一次提问的内容进了默认会话，
// 而列表里根本没有那一项 —— 他在界面上选不到它，也就永远
// 看不到、清不掉自己的第一段对话。
func TestListSessionsAlwaysIncludesDefault(t *testing.T) {
	a := New(nil, nil, noEmit)

	got := a.ListSessions("h1")
	if len(got) != 1 {
		t.Fatalf("空主机也应有一条默认会话，实得 %d 条", len(got))
	}
	if got[0].ID != DefaultSessionID {
		t.Errorf("那条应是默认会话，实得 %q", got[0].ID)
	}
	if !got[0].IsDefault {
		t.Error("IsDefault 应为 true，界面靠它决定「不能删」")
	}
	if got[0].Turns != 0 {
		t.Errorf("还没聊过，轮数应为 0，实得 %d", got[0].Turns)
	}
}

// 默认会话排在第一位，其余的按最近使用倒序。
//
// 顺序在这里不是小事：列表会「自己跳动」看起来就像 bug，
// 而默认会话排在后面的话，用户想找那个兜底的选项得先翻一遍。
func TestListSessionsSortsDefaultFirstThenMostRecent(t *testing.T) {
	a := New(nil, nil, noEmit)
	base := time.Now()
	seedSession(a, "h1", "s-old", "最早用的", base.Add(-3*time.Hour), 1)
	seedSession(a, "h1", "s-new", "刚用过", base, 1)
	seedSession(a, "h1", "s-mid", "中间", base.Add(-1*time.Hour), 1)
	seedSession(a, "h1", DefaultSessionID, "", base.Add(-5*time.Hour), 1)

	got := a.ListSessions("h1")
	want := []string{DefaultSessionID, "s-new", "s-mid", "s-old"}
	if len(got) != len(want) {
		t.Fatalf("应有 %d 条，实得 %d", len(want), len(got))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("第 %d 位应是 %q，实得 %q（完整顺序：%v）", i, id, got[i].ID, idsOf(got))
		}
	}
}

// 更新时间完全相同时按 ID 定序。
//
// 同一毫秒内建出来的会话时间戳会一样（Windows 上尤其常见），
// 不定序的话每次调用的顺序都可能不同 —— 界面上的列表会自己抖动。
func TestListSessionsTieBreaksByID(t *testing.T) {
	a := New(nil, nil, noEmit)
	at := time.Now()
	seedSession(a, "h1", "s-c", "丙", at, 1)
	seedSession(a, "h1", "s-a", "甲", at, 1)
	seedSession(a, "h1", "s-b", "乙", at, 1)

	first := idsOf(a.ListSessions("h1"))
	for i := 0; i < 5; i++ {
		if again := idsOf(a.ListSessions("h1")); !equalStrings(first, again) {
			t.Fatalf("顺序不稳定：第一次 %v，第 %d 次 %v", first, i+2, again)
		}
	}
	want := []string{DefaultSessionID, "s-a", "s-b", "s-c"}
	if !equalStrings(first, want) {
		t.Errorf("时间相同时应按 ID 排序，实得 %v", first)
	}
}

// 新建的会话要出现在列表里，带上名字，且**没有内容**。
func TestCreateSessionAddsNamedEmptySession(t *testing.T) {
	a := New(nil, nil, noEmit)

	info, err := a.CreateSession("h1", "nginx 排查")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "nginx 排查" {
		t.Errorf("名字没设上：%q", info.Name)
	}
	if info.IsDefault {
		t.Error("新建的会话不该被当成默认会话 —— 否则界面会禁止删它")
	}
	if info.Turns != 0 {
		t.Errorf("新建的会话应为空，实得 %d 轮", info.Turns)
	}

	got := a.ListSessions("h1")
	if len(got) != 2 {
		t.Fatalf("应有「默认 + 新建」两条，实得 %d 条", len(got))
	}
	if got[1].ID != info.ID {
		t.Errorf("列表里没找到刚建的会话：%v", idsOf(got))
	}
}

// 空白名字一律拒绝。
//
// 放行的话列表里会出现一个看不出内容的空条目 ——
// 用户既认不出它是什么，也没法通过名字判断该不该删。
func TestCreateSessionRejectsBlankName(t *testing.T) {
	a := New(nil, nil, noEmit)
	for _, name := range []string{"", "   ", "\n", "\t \r\n", "\u00a0\u2003"} {
		if _, err := a.CreateSession("h1", name); err == nil {
			t.Errorf("空白名字 %q 应被拒绝", name)
		}
	}
	if got := a.ListSessions("h1"); len(got) != 1 {
		t.Errorf("被拒绝的创建不该留下痕迹，实得 %d 条", len(got))
	}
}

// 控制字符换成空格、连续空白折叠 —— 而不是直接删掉。
//
// 直接删的话 "a\nb" 会变成 "ab"，看起来像是用户自己少打了字。
// 列表名与他输入的不一致时，他会怀疑是程序改错了名字。
func TestCreateSessionCleansControlChars(t *testing.T) {
	a := New(nil, nil, noEmit)
	cases := []struct{ in, want string }{
		{"a\nb", "a b"},
		{"a\tb", "a b"},
		{"  前后有空白  ", "前后有空白"},
		{"多个   空格", "多个 空格"},
		{"带\x1b[31m颜色的名字", "带 [31m颜色的名字"},
	}
	for _, c := range cases {
		info, err := a.CreateSession("h1", c.in)
		if err != nil {
			t.Errorf("%q 应被接受: %v", c.in, err)
			continue
		}
		if info.Name != c.want {
			t.Errorf("%q 应清洗成 %q，实得 %q", c.in, c.want, info.Name)
		}
	}
}

// 超长按**字符**截断，不是按字节。
//
// 按字节算的话，中文名字会在第 13 个字左右被拦腰砍断 ——
// 砍出来的半个字变成乱码，用户看到的是一个坏掉的名字。
func TestCreateSessionTruncatesByRunesNotBytes(t *testing.T) {
	a := New(nil, nil, noEmit)
	long := strings.Repeat("很", 50) // 50 个汉字 = 150 字节

	info, err := a.CreateSession("h1", long)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(info.Name)); n != maxSessionNameRunes {
		t.Errorf("应截断到 %d 个字符，实得 %d 个字符（%d 字节）",
			maxSessionNameRunes, n, len(info.Name))
	}
	if !strings.HasPrefix(long, info.Name) {
		t.Errorf("截断不该改变内容，实得 %q", info.Name)
	}
}

// 每台主机的会话数有上限，且**还没写过的默认会话也占一个位置**。
//
// 不把默认会话算进去的话，用户实际能建出「上限 + 1」条。
// 而之所以必须有这个闸门：单条会话内存上限 256KiB、落盘总量 8MiB，
// 没有闸门时在一台主机上狂建就能把落盘预算吃光 ——
// 淘汰是按「最旧」丢的，用户会看到**别的**主机的历史莫名消失。
func TestCreateSessionEnforcesPerHostLimit(t *testing.T) {
	a := New(nil, nil, noEmit)

	// 默认会话占掉一个名额，所以具名会话只能建 maxSessionsPerHost-1 条。
	for i := 0; i < maxSessionsPerHost-1; i++ {
		if _, err := a.CreateSession("h1", "会话"+string(rune('A'+i))); err != nil {
			t.Fatalf("第 %d 条就建不动了: %v", i+1, err)
		}
	}
	if _, err := a.CreateSession("h1", "再来一条"); err == nil {
		t.Fatal("超出上限时应报错")
	}
	if got := a.ListSessions("h1"); len(got) != maxSessionsPerHost {
		t.Errorf("总数应停在 %d 条，实得 %d 条", maxSessionsPerHost, len(got))
	}
	// 上限是**每台主机**的：另一台主机不该受影响。
	if _, err := a.CreateSession("h2", "另一台的会话"); err != nil {
		t.Errorf("上限应只作用于单台主机: %v", err)
	}
}

// 默认会话删不掉 —— 它是「不带会话 ID 跑一轮」的落点。
//
// 而且要确认它**没被误删**：只断言「返回了错误」是不够的，
// 万一实现是先删再报错，用户的历史就没了。
func TestDeleteSessionRefusesDefault(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("重要的问", "重要的答"), defaultLimits())

	if err := a.DeleteSession("h1", DefaultSessionID); err == nil {
		t.Fatal("默认会话不该被删掉")
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Fatalf("报错时不该真的删掉内容，实得 %d 条", len(hist))
	}
	if len(a.ListSessions("h1")) != 1 {
		t.Error("列表里仍应只有那条默认会话")
	}
}

// 传空会话 ID 与传默认会话 ID 是一回事。
//
// 前端不必知道默认会话的 ID 叫什么，也就不会出现
// 「前端写死了一个 ID、后端改了常量」这种静默错位。
func TestDeleteSessionEmptyIDMeansDefault(t *testing.T) {
	a := New(nil, nil, noEmit)
	if err := a.DeleteSession("h1", ""); err == nil {
		t.Fatal("空 ID 应被当成默认会话，因而同样删不掉")
	}
}

// 删一条不影响别的会话，也不影响别的主机。
func TestDeleteSessionRemovesOnlyThatOne(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("默认的问", "默认的答"), defaultLimits())
	rememberDefault(a, "h2", simpleTurn("另一台的问", "另一台的答"), defaultLimits())
	info, err := a.CreateSession("h1", "待删除")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.DeleteSession("h1", info.ID); err != nil {
		t.Fatal(err)
	}

	if got := a.ListSessions("h1"); len(got) != 1 || got[0].ID != DefaultSessionID {
		t.Errorf("h1 应只剩默认会话，实得 %v", idsOf(got))
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Errorf("默认会话的内容被误伤了，实得 %d 条", len(hist))
	}
	if hist := histDefault(a, "h2"); len(hist) != 2 {
		t.Errorf("另一台主机被误伤了，实得 %d 条", len(hist))
	}
}

// 删一条不存在的会话要报错，而不是静默成功。
//
// 静默成功的话，界面会显示「已删除」而列表里那条还在 ——
// 用户会反复点，然后以为整个功能坏了。
//
// 三种「不存在」走的是**不同分支**，必须分开测：
//   - 主机在这台机器上一条会话都没有（内层 map 根本不存在）；
//   - 主机有别的会话，但指定的那条不在其中；
//   - 主机本身就不存在。
//
// 只测第一和第三种的话，「那条会话在不在」这个判断整个删掉也照样绿 ——
// 变异测试实测抓到了这个漏洞，所以这里把第二种补上。
func TestDeleteSessionUnknownIsError(t *testing.T) {
	a := New(nil, nil, noEmit)

	if err := a.DeleteSession("h-nope", "s-nope"); err == nil {
		t.Fatal("删不存在主机上的会话应报错")
	}
	if err := a.DeleteSession("h1", "s-nope"); err == nil {
		t.Fatal("主机一条会话都没有时，删会话应报错")
	}

	// 关键的一条：主机存在、且有别的会话，只是指定那条不在。
	rememberDefault(a, "h1", simpleTurn("问", "答"), defaultLimits())
	if err := a.DeleteSession("h1", "s-nope"); err == nil {
		t.Fatal("主机有会话但指定的那条不存在时，必须报错而不是静默成功")
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Fatalf("报错时不该动别的会话，实得 %d 条", len(hist))
	}
}

// 默认会话**可以**改名 —— 它由 ID 认定，不由名字认定。
//
// 禁止改名会逼出一个更糟的设计：用户想给兜底的那条起个名字
// （比如「随手问」），却只能新建一条、把默认晾在那儿。
func TestRenameSessionWorksOnDefault(t *testing.T) {
	a := New(nil, nil, noEmit)
	rememberDefault(a, "h1", simpleTurn("问", "答"), defaultLimits())

	if err := a.RenameSession("h1", DefaultSessionID, "随手问"); err != nil {
		t.Fatal(err)
	}

	got := a.ListSessions("h1")
	if len(got) != 1 {
		t.Fatalf("改名不该新建会话，实得 %d 条", len(got))
	}
	if got[0].Name != "随手问" {
		t.Errorf("名字没改上：%q", got[0].Name)
	}
	if !got[0].IsDefault || got[0].ID != DefaultSessionID {
		t.Error("改的只是名字，它的身份仍是默认会话")
	}
	if hist := histDefault(a, "h1"); len(hist) != 2 {
		t.Errorf("改名不该动历史，实得 %d 条", len(hist))
	}
}

// 改名同样要清洗名字，也要拒绝空名字。
func TestRenameSessionValidatesName(t *testing.T) {
	a := New(nil, nil, noEmit)
	info, err := a.CreateSession("h1", "原名")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RenameSession("h1", info.ID, "   "); err == nil {
		t.Fatal("空名字应被拒绝")
	}
	if err := a.RenameSession("h1", info.ID, "改\n名"); err != nil {
		t.Fatal(err)
	}
	got := a.ListSessions("h1")
	for _, s := range got {
		if s.ID == info.ID && s.Name != "改 名" {
			t.Errorf("新名字应被清洗，实得 %q", s.Name)
		}
	}
}

// 改不存在的会话要报错。
func TestRenameSessionUnknownIsError(t *testing.T) {
	a := New(nil, nil, noEmit)
	if err := a.RenameSession("h1", "s-nope", "新名"); err == nil {
		t.Fatal("改不存在的会话应报错")
	}
}

// 清空只丢历史，**保留会话本身和它的名字**。
//
// 这一条容易写反：如果清空把会话整个删掉，用户会看到自己精心起名的
// 「nginx 排查」从列表里消失了 —— 而他的本意只是「把上下文清掉重来」。
func TestClearSessionKeepsSessionAndName(t *testing.T) {
	a := New(nil, nil, noEmit)
	info, err := a.CreateSession("h1", "nginx 排查")
	if err != nil {
		t.Fatal(err)
	}
	a.remember("h1", info.ID, simpleTurn("问", "答"), defaultLimits())

	n, busy := a.ClearSession("h1", info.ID)
	if n != 1 || busy {
		t.Fatalf("清空应返回 (1,false)，实得 (%d,%v)", n, busy)
	}

	got := a.ListSessions("h1")
	var found bool
	for _, s := range got {
		if s.ID == info.ID {
			found = true
			if s.Name != "nginx 排查" {
				t.Errorf("清空不该丢掉名字，实得 %q", s.Name)
			}
			if s.Turns != 0 {
				t.Errorf("清空后轮数应为 0，实得 %d", s.Turns)
			}
		}
	}
	if !found {
		t.Error("清空不该把会话本身删掉 —— 用户只是想清掉上下文")
	}
}

// 会话之间必须完全隔离。
//
// 这是「一台主机多条会话」的全部意义。若哪天有人把 map 的键写错、
// 或让两条会话共用了同一个 *session，表现就是「两条对话串在一起」——
// 用户会看到模型把 nginx 的上下文答到了磁盘的问题上。
func TestSessionsAreIsolatedFromEachOther(t *testing.T) {
	a := New(nil, nil, noEmit)
	a.remember("h1", "s-a", simpleTurn("A 的问", "A 的答"), defaultLimits())
	a.remember("h1", "s-b", simpleTurn("B 的问", "B 的答"), defaultLimits())
	a.remember("h1", DefaultSessionID, simpleTurn("默认的问", "默认的答"), defaultLimits())

	cases := []struct {
		sessionID string
		want      string
	}{
		{"s-a", "A 的问"},
		{"s-b", "B 的问"},
		{DefaultSessionID, "默认的问"},
	}
	for _, c := range cases {
		hist := a.historyFor("h1", c.sessionID)
		if len(hist) != 2 {
			t.Errorf("%s 应有 2 条消息，实得 %d", c.sessionID, len(hist))
			continue
		}
		if hist[0].Content != c.want {
			t.Errorf("%s 读到了别人的内容：%q", c.sessionID, hist[0].Content)
		}
	}

	// 清掉一条，另外两条不受影响。
	a.ClearSession("h1", "s-a")
	if hist := a.historyFor("h1", "s-b"); len(hist) != 2 {
		t.Errorf("清 s-a 误伤了 s-b，实得 %d 条", len(hist))
	}
	if hist := a.historyFor("h1", DefaultSessionID); len(hist) != 2 {
		t.Errorf("清 s-a 误伤了默认会话，实得 %d 条", len(hist))
	}
}

// 空会话 ID 落到默认会话上（不是新建一条无名会话）。
func TestEmptySessionIDMeansDefault(t *testing.T) {
	a := New(nil, nil, noEmit)
	a.remember("h1", "", simpleTurn("问", "答"), defaultLimits())

	if hist := a.historyFor("h1", DefaultSessionID); len(hist) != 2 {
		t.Fatalf("空 ID 应落到默认会话，实得 %d 条", len(hist))
	}
	if got := a.ListSessions("h1"); len(got) != 1 {
		t.Errorf("不该多出一条无名会话，实得 %v", idsOf(got))
	}
}

// 新建与删除都要**立刻落盘** —— 否则重启后它们会复活或消失。
//
// 两种表现都很糟：新建的消失，用户以为「新建没生效」；
// 删除的复活，用户以为「删除按钮坏了」。
func TestSessionCreateAndDeletePersistImmediately(t *testing.T) {
	ss, dir := newTestStore(t)
	a1 := New(nil, nil, noEmit)
	if err := a1.SetSessionStore(ss); err != nil {
		t.Fatal(err)
	}

	info, err := a1.CreateSession("h1", "重启前建的")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a1.CreateSession("h1", "待删除的"); err != nil {
		t.Fatal(err)
	}

	a2 := reloadAgent(t, dir)
	var names []string
	for _, s := range a2.ListSessions("h1") {
		names = append(names, s.Name)
	}
	if !contains(names, "重启前建的") {
		t.Fatalf("新建的会话没跨过重启，实得 %v", names)
	}

	// 删掉之后重启，它必须还是不在。
	a2.DeleteSession("h1", info.ID)
	a3 := reloadAgent(t, dir)
	for _, s := range a3.ListSessions("h1") {
		if s.ID == info.ID {
			t.Fatal("删掉的会话重启后又复活了")
		}
	}
}

// 新建的 ID 不与现有的冲突，且带可辨识的前缀。
//
// 用随机而不是递增序号：序号会随删除被复用，而复用意味着
// 「一条已删会话的落盘记录可能被新会话认领」——
// 一旦哪天有别的路径按 ID 缓存过会话，那就是两段不同的对话串在一起。
func TestNewSessionIDIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]*session{}
	for i := 0; i < 200; i++ {
		id := newSessionID(seen)
		if id == "" {
			t.Fatal("不该返回空 ID")
		}
		if !strings.HasPrefix(id, "s") {
			t.Errorf("ID 应带 s 前缀以便与手写的 ID 区分，实得 %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("ID 重复了：%q", id)
		}
		seen[id] = &session{}
	}
}

func idsOf(list []SessionInfo) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
