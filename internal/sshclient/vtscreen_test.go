package sshclient

import (
	"strconv"
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

func TestScreenScrollbackSurvivesViewport(t *testing.T) {
	s := NewScreen(20, 3)
	var b strings.Builder
	for i := 1; i <= 8; i++ {
		b.WriteString("line-" + strconv.Itoa(i) + "\r\n")
	}
	s.Feed([]byte(b.String()))
	vis := s.Text()
	if strings.Contains(vis, "line-1") {
		t.Fatalf("可视区不应再含已滚走的 line-1：%q", vis)
	}
	tail := s.TextTail(20)
	if !strings.Contains(tail, "line-1") || !strings.Contains(tail, "line-8") {
		t.Fatalf("回滚应保留滚出可视区的行，实得 %q", tail)
	}
	short := s.TextTail(2)
	if strings.Contains(short, "line-1") {
		t.Fatalf("TextTail 应按行数截尾，实得 %q", short)
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

func TestSnapshotterAbandonSuppressesFinal(t *testing.T) {
	s := NewScreen(20, 3)
	s.Feed([]byte("Welcome to Ubuntu\n"))
	var n int
	sn := NewSnapshotter(s, 0, func(string, bool) { n++ })
	sn.Abandon()
	sn.Final() // 主动关闭路径：不应把 MOTD 当结束画面送出
	sn.Flush()
	if n != 0 {
		t.Fatalf("Abandon 后 Final/Flush 不该送帧，实得 %d 次", n)
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

// CmdActive 是前端「命令运行中输入归命令」的后端信号（OSC 133;C..D）。
//
// 与 InTUI 分开锁：非全屏的交互程序（bt 菜单/read/REPL/sudo 口令）不发备用
// 屏幕、也不重绘，只能靠这一档让路 —— 漏掉时用户答菜单的 `14` 会被本地
// 行编辑当「数字编号回复」路由给 LLM，而菜单一直等不到输入（截图 bug）。
func TestScreenCmdActive(t *testing.T) {
	s := NewScreen(20, 3)
	if s.CmdActive() {
		t.Fatal("空屏幕不该判为命令运行中")
	}

	s.Feed([]byte("\x1b]133;C\x07")) // 命令开始
	if !s.CmdActive() {
		t.Fatal("133;C 后应报告命令运行中")
	}
	// 命令期间的普通输出（菜单文本）不改变状态 —— bt 菜单既不重绘也不切屏。
	s.Feed([]byte("Please enter command number: "))
	if !s.CmdActive() {
		t.Fatal("命令期间的普通输出不该让出")
	}
	var gotTUI bool
	var gotCode int
	var gotKnown bool
	var gotCalls int
	s.SetTriggers(func(tui bool, code int, known bool) {
		gotCalls++
		gotTUI, gotCode, gotKnown = tui, code, known
	}, nil)
	s.Feed([]byte("syntax error near unexpected token\r\n"))
	s.Feed([]byte("\x1b]133;D;2\x07")) // 命令结束（带退出码）
	if s.CmdActive() {
		t.Fatal("133;D 后应让出")
	}
	if gotCalls != 1 || gotTUI || !gotKnown || gotCode != 2 {
		t.Fatalf("失败命令应回报退出码 2，实得 calls=%d tui=%v known=%v code=%d", gotCalls, gotTUI, gotKnown, gotCode)
	}

	// 无配对 133;C 的 133;D 忽略（注入期首屏可能出现）；重复 133;C 只算一次。
	s.Feed([]byte("\x1b]133;D\x07"))
	if s.CmdActive() {
		t.Fatal("无配对 133;C 的 133;D 不该改变状态")
	}
	s.Feed([]byte("\x1b]133;C\x07"))
	s.Feed([]byte("\x1b]133;C\x07"))
	s.Feed([]byte("\x1b]133;D\x07"))
	if s.CmdActive() {
		t.Fatal("重复 133;C 之后一个 133;D 也应让出")
	}

	// 备用屏幕本身不是「命令运行中」——那是 InTUI 的档位，两档信号分开。
	alt := NewScreen(20, 3)
	alt.Feed([]byte("\x1b[?1049h"))
	if alt.CmdActive() {
		t.Fatal("只有备用屏幕（没有命令边界）不该判为命令运行中")
	}
}

// OSC 7 是远端 shell 上报当前目录的标准序列，也是文件管理弹窗
// 「默认定位到控制台当前目录」的唯一数据来源。这里锁死三件事：
// 解析正确、不污染屏幕、以及含转义字符的路径能还原。
func TestScreenOSC7Cwd(t *testing.T) {
	s := NewScreen(40, 3)
	if got := s.Cwd(); got != "" {
		t.Fatalf("还没收到 OSC 7 时应为空，实得 %q", got)
	}

	// 基本形式：file://host/path
	s.Feed([]byte("\x1b]7;file://myhost/var/log\x07"))
	if got := s.Cwd(); got != "/var/log" {
		t.Fatalf("应解析出 /var/log，实得 %q", got)
	}

	// host 允许为空（file:///root）。
	s.Feed([]byte("\x1b]7;file:///root\x07"))
	if got := s.Cwd(); got != "/root" {
		t.Fatalf("空 host 应解析出 /root，实得 %q", got)
	}

	// 根目录必须能表达 —— 它是最容易被「空串当没收到」的健忘逻辑吃掉的边界值。
	s.Feed([]byte("\x1b]7;file://myhost/\x07"))
	if got := s.Cwd(); got != "/" {
		t.Fatalf("根目录应解析出 /，实得 %q", got)
	}

	// 含空格与中文的目录会被转义，必须还原 —— 否则界面显示的是
	// %E4%B8%AD 这种串，用户拿去当路径用会找不到目录。
	s.Feed([]byte("\x1b]7;file://myhost/a%20b/%E4%B8%AD%E6%96%87\x07"))
	if got := s.Cwd(); got != "/a b/中文" {
		t.Fatalf("转义路径应还原为 /a b/中文，实得 %q", got)
	}

	// OSC 7 是控制序列，绝不能在屏幕上留下任何字符。
	if txt := s.Text(); strings.Contains(txt, "file://") || strings.Contains(txt, "7;") {
		t.Fatalf("OSC 7 不该被印到屏幕上：%q", txt)
	}

	// 非 file:// 的 OSC 7（罕见）应被忽略而不是解析出垃圾。
	before := s.Cwd()
	s.Feed([]byte("\x1b]7;http://example.com/x\x07"))
	if got := s.Cwd(); got != before {
		t.Fatalf("非 file scheme 不该改动 cwd，实得 %q", got)
	}

	// ST 结尾（ESC \）与 BEL 等价，也要认。
	s.Feed([]byte("\x1b]7;file://myhost/etc\x1b\\"))
	if got := s.Cwd(); got != "/etc" {
		t.Fatalf("ST 结尾应同样解析出 /etc，实得 %q", got)
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

// screenTUI 是「无备用屏、无 133 的全屏程序」（busybox vi、传统 vi）的
// 兜底识别：CUP/ED 重绘特征置位、纯文本滚动清零、静默期保持（等输入的
// 程序恰恰不发输出）。缺了它 vi 画完第一屏后按键会被前端本地行编辑吞
// 掉 —— 用户看到的就是「vi 直接卡死」。
func TestScreenScreenTUI(t *testing.T) {
	s := NewScreen(20, 3)
	if s.InTUI() {
		t.Fatal("空屏幕不该判为 TUI 接管")
	}

	// busybox vi 启动：清屏 + 定位重绘（既无 ?1049 也无 133）
	s.Feed([]byte("\x1b[H\x1b[J~\r\n~\r\n~"))
	if !s.InTUI() {
		t.Fatal("vi 主屏重绘特征出现后 InTUI 应为真（busybox vi）")
	}

	// 静默任意久：vi 在等用户输入 —— 特征必须保持，否则 6 秒后按键
	// 被本地行编辑吞掉、vi 冻死
	s.Feed(nil)
	if !s.InTUI() {
		t.Fatal("静默期 screenTUI 必须保持（vi 等输入不发输出）")
	}

	// vi 退出：擦屏重绘（此时 vi 已死但特征还在）、随后 shell 打印提示
	// 符 —— 真实链路里 vi 退出与 bash 醒来是两包数据，提示符文本块
	// （纯文本、无重绘）把特征清掉
	s.Feed([]byte("\x1b[H\x1b[2J"))
	s.Feed([]byte("root@web:~# "))
	if s.InTUI() {
		t.Fatal("vi 退出后的提示符文本应清掉 screenTUI，InTUI 为假")
	}

	// 普通命令的流式输出（cat/ls）不该被误判为 TUI —— 只有定位/擦屏算
	s2 := NewScreen(20, 3)
	s2.Feed([]byte("total 0\r\ndrwxr-xr-x 2 root root .\r\n"))
	if s2.InTUI() {
		t.Fatal("纯文本滚动输出不该置 screenTUI")
	}
	// SGR 颜色（ls --color）同样不算重绘
	s2.Feed([]byte("\x1b[1;32mfile.txt\x1b[0m\r\n"))
	if s2.InTUI() {
		t.Fatal("SGR 着色不算重绘特征")
	}

	// 命令边界优先于重绘残留：procps top（133;C 期间重绘）结束后必须让出
	s3 := NewScreen(20, 3)
	s3.Feed([]byte("\x1b]133;C\x07\x1b[H\x1b[2Jtop"))
	if !s3.InTUI() {
		t.Fatal("top 运行中应判为 TUI")
	}
	s3.Feed([]byte("\x1b]133;D\x07"))
	if s3.InTUI() {
		t.Fatal("133;D 命令结束应让出，screenTUI 不能越界残留")
	}

	// 备用屏是更高档的权威信号：alt 激活期间 screenTUI 恒被压住，
	// vim 退出备用屏必须立刻脱离 TUI 态（不能被 alt 期间的重绘拖住）
	s4 := NewScreen(20, 3)
	s4.Feed([]byte("\x1b[?1049h\x1b[Hvim"))
	if !s4.InTUI() {
		t.Fatal("备用屏激活应判为 TUI（vim）")
	}
	s4.Feed([]byte("\x1b[?1049lroot@web:~# "))
	if s4.InTUI() {
		t.Fatal("退出备用屏后 InTUI 应为假")
	}
}

// clear/reset 的整屏擦除不该误判 TUI：清屏序列与 echo、提示符文本几乎必然
// 同块到达（PTY 一次读的边界与远端 write 边界无关），ED2 若算重绘证据，
// redraw 会压过提示符文本 → InTUI 误真 → 前端把按键直灌 PTY，路由与硬拒绝
// 全被绕过，且 shell 静默等输入后特征再无输出可清 —— clear 之后中文提问
// 直进 bash 报 command not found。
func TestScreenClearNotTUI(t *testing.T) {
	s := NewScreen(20, 3)
	// echo + clear(\E[H\E[2J) + 提示符，一个 Feed 内到达
	s.Feed([]byte("clear\r\n\x1b[H\x1b[2Jroot@web:~# "))
	if s.InTUI() {
		t.Fatal("clear + 提示符同块：不该判为 TUI")
	}
	// reset（RIS）同理：全复位是「擦干净等提示符」
	s2 := NewScreen(20, 3)
	s2.Feed([]byte("reset\r\n\x1bcroot@web:~# "))
	if s2.InTUI() {
		t.Fatal("RIS 复位不算重绘特征")
	}
	// 部分变体在同块先发 CUP 再发 RIS：RIS 也要撤销本块已见的重绘证据，
	// 否则 feedRedraw 残留 → reset 误判 TUI 接管（与 ED2/ED3 同类事故）
	s2b := NewScreen(20, 3)
	s2b.Feed([]byte("reset\r\n\x1b[H\x1bcroot@web:~# "))
	if s2b.InTUI() {
		t.Fatal("CUP+RIS 同块：RIS 应撤销本块重绘证据，不判 TUI")
	}
	// procps top 这类首帧 clear 后紧跟成排 CUP 的仍要识别（top 无 133 场景）
	s3 := NewScreen(20, 3)
	s3.Feed([]byte("\x1b[H\x1b[2J\x1b[1;1Htop\x1b[2;1Hload: 0.1"))
	if !s3.InTUI() {
		t.Fatal("清屏后持续定位重绘（top）仍应判为 TUI")
	}
}

// 首帧断在多字节字符边界：本块的重绘证据必须存活，补完字符的续块（纯文本）
// 也不能把刚置位的 screenTUI 清掉 —— 否则 vi 首帧尾恰是汉字时，按键被本地
// 行编辑吞掉、vi 冻死。
func TestScreenTUISplitRune(t *testing.T) {
	s := NewScreen(20, 3)
	// busybox vi 首帧：CUP+ED0 + 文本，末尾断在「你」(E4BDA0) 的前两字节
	s.Feed([]byte("\x1b[H\x1b[J~\r\n~\r\n\xe4\xbd"))
	if !s.InTUI() {
		t.Fatal("断在半字符上的块，其重绘证据不得丢失（提前收敛）")
	}
	// 补完「你」的续块是纯文本：属于同一逻辑帧，不该当提示符证据清零
	s.Feed([]byte("\xa0"))
	if !s.InTUI() {
		t.Fatal("补完半字符的纯文本续块不该清掉 screenTUI")
	}
	// 提示符自身断在半字符（prompt 含中文）也不影响非 TUI 会话保持非 TUI
	s2 := NewScreen(20, 3)
	s2.Feed([]byte("root@web:~# \xe4\xbd"))
	s2.Feed([]byte("\xa0# "))
	if s2.InTUI() {
		t.Fatal("提示符跨块补完不该误判 TUI")
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
