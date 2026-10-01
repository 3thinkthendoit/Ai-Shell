package sshclient

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 屏幕模型是「模型看 TUI 画面」的唯一来源：解析错一个序列，
// 进上下文的就是乱码或错行。这里按 top/vim 实际会发的序列逐类锁死。

func TestScreenPlainTextAndCursorMarker(t *testing.T) {
	s := NewScreen(20, 5)
	s.Feed([]byte("hello\r\nworld"))
	out := s.Text()
	// 第一行非光标行：两空格前缀；光标停在第二行行尾："> " 前缀
	if !strings.Contains(out, "  hello\n") {
		t.Fatalf("首行应带两空格前缀，实得 %q", out)
	}
	if !strings.Contains(out, "> world\n") {
		t.Fatalf("光标行应带 > 前缀，实得 %q", out)
	}
	// 尾部全空行裁掉：5 行的屏幕只该输出 2 行
	if got := strings.Count(out, "\n"); got != 2 {
		t.Fatalf("尾部空行应裁掉，期望 2 行实得 %d 行：%q", got, out)
	}
}

func TestScreenCUPAndErase(t *testing.T) {
	s := NewScreen(10, 3)
	s.Feed([]byte("aaaaa\r\nbbbbb\r\nccccc"))
	// 光标定位到 (2,3)（1 基）改写
	s.Feed([]byte("\x1b[2;3HXY"))
	out := s.Text()
	if !strings.Contains(out, "bbXYb") {
		t.Fatalf("CUP 定位改写失败：%q", out)
	}
	// 擦行（从光标到行尾）：光标现在 (2,5)
	s.Feed([]byte("\x1b[K"))
	out = s.Text()
	if !strings.Contains(out, "bbXY") || strings.Contains(out, "bbXYb") {
		t.Fatalf("EL 擦行失败：%q", out)
	}
	// 全屏擦除
	s.Feed([]byte("\x1b[2J"))
	if strings.TrimSpace(s.Text()) != "" {
		t.Fatalf("ED 2 应清空屏幕，实得 %q", s.Text())
	}
}

func TestScreenScrollAndWrap(t *testing.T) {
	s := NewScreen(5, 3)
	// 4 行内容进 3 行屏幕：首行被滚掉
	s.Feed([]byte("one\r\ntwo\r\nthree\r\nfour"))
	out := s.Text()
	if strings.Contains(out, "one") {
		t.Fatalf("首行应被滚掉：%q", out)
	}
	if !strings.Contains(out, "two") || !strings.Contains(out, "four") {
		t.Fatalf("滚动后应保留 two/three/four：%q", out)
	}
	// 超宽自动换行：5 列屏幕写 7 个字符
	s2 := NewScreen(5, 3)
	s2.Feed([]byte("1234567"))
	out2 := s2.Text()
	if !strings.Contains(out2, "12345") || !strings.Contains(out2, "67") {
		t.Fatalf("超宽应换行：%q", out2)
	}
}

func TestScreenUTF8AcrossFeedBoundary(t *testing.T) {
	s := NewScreen(20, 3)
	full := []byte("进程 顶部")
	// 把一个三字节汉字切成两块喂：不能出替换字符
	s.Feed(full[:4])
	s.Feed(full[4:])
	out := s.Text()
	if !strings.Contains(out, "进程 顶部") {
		t.Fatalf("跨块 UTF-8 解码失败：%q", out)
	}
	if strings.Contains(out, "\ufffd") {
		t.Fatalf("出现了替换字符：%q", out)
	}
}

func TestScreenPrivateModesAndSGRIgnored(t *testing.T) {
	s := NewScreen(20, 3)
	// 隐藏光标、切备用屏、上色：都不该污染文本
	s.Feed([]byte("\x1b[?25l\x1b[?1049h\x1b[1;32mok\x1b[0m"))
	if !strings.Contains(s.Text(), "ok") {
		t.Fatalf("私有模式/SGR 应被忽略但文本保留：%q", s.Text())
	}
}

func TestScreenTabAndBackspace(t *testing.T) {
	s := NewScreen(20, 3)
	s.Feed([]byte("a\tb"))
	if !strings.Contains(s.Text(), "a       b") {
		t.Fatalf("制表位应为 8 列：%q", s.Text())
	}
	s2 := NewScreen(20, 3)
	s2.Feed([]byte("ab\x08c"))
	if !strings.Contains(s2.Text(), "ac") {
		t.Fatalf("退格应覆盖前一字符：%q", s2.Text())
	}
}

func TestSnapshotterFiresOnIdleAndDedupes(t *testing.T) {
	s := NewScreen(20, 3)
	var mu sync.Mutex
	var snaps []string
	var finals []bool
	sn := NewSnapshotter(s, 30*time.Millisecond, func(text string, final bool) {
		mu.Lock()
		snaps = append(snaps, text)
		finals = append(finals, final)
		mu.Unlock()
	})

	s.Feed([]byte("load: 0.5"))
	sn.Kick()
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	n := len(snaps)
	first := ""
	if n > 0 {
		first = snaps[0]
	}
	mu.Unlock()
	if n != 1 || !strings.Contains(first, "load: 0.5") {
		t.Fatalf("静止后应恰好快照一次，实得 %d 次：%q", n, snaps)
	}
	if finals[0] {
		t.Fatal("静止触发的快照不是定格帧，final 应为 false")
	}

	// 内容不变的重绘：哈希去重，不再进上下文
	s.Feed([]byte("\x1b[H\x1b[2Jload: 0.5"))
	sn.Kick()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	n = len(snaps)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("内容未变的快照应被去重，实得 %d 次", n)
	}

	// 内容变了：再截一次
	s.Feed([]byte("\x1b[H\x1b[2Jload: 9.9"))
	sn.Kick()
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	n = len(snaps)
	second := snaps[n-1]
	mu.Unlock()
	if n != 2 || !strings.Contains(second, "load: 9.9") {
		t.Fatalf("内容变化应再快照一次，实得 %d 次：%q", n, snaps)
	}
}

func TestSnapshotterStopIsFinal(t *testing.T) {
	s := NewScreen(20, 3)
	var mu sync.Mutex
	n := 0
	sn := NewSnapshotter(s, 20*time.Millisecond, func(string, bool) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	sn.Stop()
	s.Feed([]byte("late"))
	sn.Kick()  // Stop 之后的 Kick 是 no-op
	sn.Flush() // Stop 之后的过程定格也不送：块都关了
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n != 0 {
		t.Fatalf("Stop 后不该再快照，实得 %d 次", n)
	}
}

// 备用屏幕开关是「全屏 TUI 接管/让出屏幕」的信号，必须被精确捕获，
// 且私有模式序列本身不能被当字面字符印到屏幕上。
func TestScreenAltScreenDetect(t *testing.T) {
	s := NewScreen(20, 3)
	s.Feed([]byte("\x1b[?1049h")) // top/vim 新式切入
	if !s.AltActive() {
		t.Fatal("?1049h 后应处于备用屏幕")
	}
	s.Feed([]byte("\x1b[?1049l"))
	if s.AltActive() {
		t.Fatal("?1049l 后应退回主屏幕")
	}
	s.Feed([]byte("\x1b[?47h")) // 旧式切入
	if !s.AltActive() {
		t.Fatal("?47h 也应识别为备用屏幕")
	}
	s.Feed([]byte("\x1b[?1047l"))
	if s.AltActive() {
		t.Fatal("?1047l 也应退回主屏幕")
	}
	// 其它私有模式（光标显隐）与 alt 无关，也不能污染屏幕文本
	s.Feed([]byte("\x1b[?25l"))
	if s.AltActive() {
		t.Fatal("?25l（隐藏光标）不是备用屏幕开关")
	}
	if txt := s.Text(); strings.Contains(txt, "1049") || strings.Contains(txt, "?25") {
		t.Fatalf("私有模式序列不该被印到屏幕上：%q", txt)
	}
}

// InTUI 是前端「终端内直接输入」让路的后端信号：接管中整体透传按键，
// 否则 top/vim 里敲的 q、i 会被本地行编辑吞掉。两条识别路径都得锁死：
//   - 备用屏幕（vim/htop，?1049h）；
//   - procps 系 top：不发备用屏幕序列，靠「命令期间见过 CUP/ED 重绘」识别。
func TestScreenInTUI(t *testing.T) {
	s := NewScreen(20, 3)
	if s.InTUI() {
		t.Fatal("空屏幕不该判为 TUI 接管")
	}

	// 路径一：备用屏幕。
	s.Feed([]byte("\x1b[?1049h"))
	if !s.InTUI() {
		t.Fatal("?1049h 后 InTUI 应为真（vim/htop）")
	}
	s.Feed([]byte("\x1b[?1049l"))
	if s.InTUI() {
		t.Fatal("?1049l 后应让出，InTUI 为假")
	}

	// 路径二：procps top —— OSC 133;C 起一条命令，命令期间 CUP 重绘。
	t2 := NewScreen(20, 3)
	t2.Feed([]byte("\x1b]133;C\x07")) // 命令开始
	if t2.InTUI() {
		t.Fatal("命令刚开始、还没重绘，不该判为 TUI")
	}
	t2.Feed([]byte("\x1b[2J\x1b[1;1Htop")) // 清屏 + 定位重绘
	if !t2.InTUI() {
		t.Fatal("命令期间见过 CUP/ED 重绘应判为 TUI（procps top）")
	}
	t2.Feed([]byte("\x1b]133;D\x07")) // 命令结束
	if t2.InTUI() {
		t.Fatal("命令结束后应让出，InTUI 为假")
	}
}

// 全屏展开/收回会重报尺寸：模型必须跟着换几何，否则 top 重绘后
// 快照文本与真屏幕错位。
func TestScreenResize(t *testing.T) {
	s := NewScreen(10, 4)
	s.Feed([]byte("header\r\nline1\r\nline2\r\nline3"))

	s.Resize(12, 6) // 变大：底部补空，已有内容保留
	if txt := s.Text(); !strings.Contains(txt, "line3") {
		t.Fatalf("放大不应丢内容，实得 %q", txt)
	}

	s.Resize(12, 2) // 变小：截断保顶部（top 的关键信息在上端）
	txt := s.Text()
	if !strings.Contains(txt, "header") || strings.Contains(txt, "line2") {
		t.Fatalf("缩小应保留顶部行，实得 %q", txt)
	}

	// 缩小后继续写：不越界、换行按新列宽
	s.Feed([]byte("\x1b[2;1H" + "0123456789AB"))
	if s.Text() == "" {
		t.Fatal("缩小后继续写入应正常")
	}
}

// Flush（TUI 退出备用屏幕）与 Final（PTY 退出）都是立即定格：
// 不等静止计时，且 final=true；内容未变时 Final 与刚送过的帧去重。
func TestSnapshotterFlushAndFinal(t *testing.T) {
	s := NewScreen(20, 3)
	var mu sync.Mutex
	var snaps []string
	var finals []bool
	sn := NewSnapshotter(s, 10*time.Second, func(text string, final bool) {
		mu.Lock()
		snaps = append(snaps, text)
		finals = append(finals, final)
		mu.Unlock()
	})

	s.Feed([]byte("Processes: 42\r\nload: 9.9"))
	sn.Kick()
	sn.Flush() // top 按 q 退出：不等静止，立即定格
	mu.Lock()
	if len(snaps) != 1 || !strings.Contains(snaps[0], "load: 9.9") || !finals[0] {
		mu.Unlock()
		t.Fatalf("Flush 应立即送定格帧，实得 %v %v", snaps, finals)
	}
	mu.Unlock()

	sn.Final() // 内容没变：去重，不重复送
	mu.Lock()
	if len(snaps) != 1 {
		mu.Unlock()
		t.Fatalf("内容未变的 Final 应被去重，实得 %d 次", len(snaps))
	}
	mu.Unlock()

	s.Feed([]byte("\x1b[H\x1b[2JProcesses: 43"))
	sn.Final() // PTY 退出：新内容必须送到（最后机会）
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) != 2 || !strings.Contains(snaps[1], "Processes: 43") || !finals[1] {
		t.Fatalf("PTY 退出的 Final 必须送最后一帧，实得 %v %v", snaps, finals)
	}
}
