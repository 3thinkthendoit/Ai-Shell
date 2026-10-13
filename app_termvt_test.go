package main

import (
	"testing"

	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// TestDropTermVTIdentityGuard 覆盖 dropTermVT 的身份守卫。
//
// 场景：同一台主机「切走再切回」会重开终端，termVT[hostID] 被换成新的一份。
// 旧那条 PTY 的 onExit 可能在之后才到达 —— 它只应清掉自己这份管线，不能按
// hostID 无条件把新管线误删（那会让重开的终端静默失去定格能力：screen 还在
// 喂字节、snapper 已被 Stop，且不报错）。显式「关闭终端」则传 want=nil 无条件清。
func TestDropTermVTIdentityGuard(t *testing.T) {
	a := &App{}
	newVT := func() *termVTState {
		scr := sshclient.NewScreen(80, 24)
		return &termVTState{screen: scr, snapper: sshclient.NewSnapshotter(scr, 0, func(string, bool) {})}
	}
	a.termVT = map[string]*termVTState{}

	vt1 := newVT()
	vt2 := newVT()
	a.termVT["h"] = vt2 // 模拟：vt1 已被 vt2 取代（切回后重开）

	// 晚到的 vt1 onExit：身份不符，不得动当前的 vt2。
	a.dropTermVT("h", vt1)
	if got := a.termVT["h"]; got != vt2 {
		t.Fatalf("晚到的退出误删了新管线：期望仍是 vt2，实际 %v", got)
	}

	// 自己的退出：身份相符，删除。
	a.dropTermVT("h", vt2)
	if _, ok := a.termVT["h"]; ok {
		t.Fatal("身份相符的退出应删掉自己的管线")
	}

	// 显式关闭（want=nil）：无条件清掉当前这条。
	vt3 := newVT()
	a.termVT["h"] = vt3
	a.dropTermVT("h", nil)
	if _, ok := a.termVT["h"]; ok {
		t.Fatal("want=nil 应无条件清掉当前管线")
	}

	// 幂等：主机不在表里时删除不应 panic。
	a.dropTermVT("nope", nil)
	a.dropTermVT("nope", vt3)
}

// CurrentTermCwd 是文件管理弹窗「默认定位到控制台 pwd 目录」的兜底查询：
// terminal:cwd 只在目录变化时推一次，弹窗打开时直接问后端 screen 的最新值。
func TestCurrentTermCwd(t *testing.T) {
	scr := sshclient.NewScreen(40, 3)
	scr.Feed([]byte("\x1b]7;file://myhost/var/log\x07"))

	a := &App{v: vault.New(t.TempDir())}
	a.termVT = map[string]*termVTState{
		termKey("h1", "s1"): {screen: scr, snapper: sshclient.NewSnapshotter(scr, 0, func(string, bool) {})},
	}

	if got := a.CurrentTermCwd("h1", "s1"); got != "/var/log" {
		t.Fatalf("应返回 screen 里最近的 cwd，实得 %q", got)
	}
	if got := a.CurrentTermCwd("h1", "s2"); got != "" {
		t.Fatalf("没有 VT 的任务应返回空串，实得 %q", got)
	}
	if got := a.CurrentTermCwd("h9", "s1"); got != "" {
		t.Fatalf("不存在的主机应返回空串，实得 %q", got)
	}
}
