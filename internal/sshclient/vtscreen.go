package sshclient

// 最小 VT100/xterm 屏幕模型 + 静止快照器。
//
// 为什么需要它：top/htop/vim 这类程序的输出是「持续重绘的屏幕」，不是
// 一段文本 —— 直接喂给 LLM 等于喂乱码。但完全不看也不对：Agent 自动
// 打开内联终端之后，模型对那块屏幕里发生了什么一无所知。
//
// 折中：后端按 PTY 字节流维护一份屏幕模型（人眼看 xterm 的实时渲染，
// 模型看这份模型的「静止快照」—— 画面停止刷新 2 秒后，把可见屏幕
// 序列化成纯文本送进上下文）。快照走完整安全管线（脱敏/注入/审计），
// 且是**瞬态上下文**：不进会话历史，只在下一轮/下一步请求里出现。
//
// 解析器只覆盖 TUI 程序实际高频使用的子集：光标移动/定位、擦除、
// 滚动、制表、UTF-8。SGR 颜色直接忽略（文本快照不需要颜色）。
// 没覆盖到的序列会被安全地跳过，不会把垃圾写进屏幕模型。

import (
	"hash/fnv"
	"sync"
	"time"
	"unicode/utf8"
)

// Screen 是一块 cols×rows 的字符屏幕，带光标与极简 ANSI 解析器。
// 所有方法 goroutine 安全：Feed 来自 PTY 读 goroutine，Text 来自快照定时器。
type Screen struct {
	mu   sync.Mutex
	cols int
	rows int
	// 单元格按行存储；行内 rune 切片长度恒为 cols（空格填充）。
	cells [][]rune
	cx    int
	cy    int
	// ESC 7/8 保存的光标
	sx, sy int
	// alt：远端程序是否切换到了备用屏幕（?1049h/?47h/?1047h）。
	// top/vim/htop 这类全屏 TUI 启动时切过去、退出时切回来 —— 它是
	// 「程序接管了整块屏幕」的精确信号，比任何命令名启发式都可靠。
	alt bool
	// shell integration（OSC 133）跟踪的命令边界与 TUI 特征：
	//   cmdActive：当前有一张命令在跑（133;C 与 133;D 之间）；
	//   cmdTUI：命令期间见过 CUP/ED 清屏重绘 —— top 这类不发备用屏幕
	//     序列的程序靠它识别（正常命令的流式输出不会定位光标）；
	//   altDuringCmd：命令期间备用屏幕曾激活（vim/htop 这类）。
	// 三者合起来是「上一条命令是全屏 TUI」的判定，退出时据此定格最后一帧。
	cmdActive    bool
	cmdTUI       bool
	altDuringCmd bool
	// 解析期间挂起的触发（Feed 持锁解析，钩子要在解锁后才调，
	// 否则钩子里读 Text() 会自锁）。
	pendCmdEnd, pendCmdEndTUI, pendAltOff bool
	// 定格钩子：onCmdEnd 在 OSC 133;D（命令结束）时调，参数是
	// 「这条命令是否全屏重绘过」；onAltOff 在备用屏幕释放瞬间调。
	// 均为 nil 安全。
	onCmdEnd func(tui bool)
	onAltOff func()
	// 解析器状态：0=ground 1=esc 2=csi 3=osc
	state   int
	csi     []byte
	osc     []byte
	pending []byte // 跨 Feed 的不完整 UTF-8 字节
}

// NewScreen 建一块屏幕。尺寸不合法时回退 80×24 —— 与 PTY 侧的
// 归一化同源思想：宁可默认值，不要 0 行列（0 行的屏幕模型 Text 恒空）。
func NewScreen(cols, rows int) *Screen {
	if cols < 2 || cols > 512 {
		cols = 80
	}
	if rows < 2 || rows > 512 {
		rows = 24
	}
	s := &Screen{cols: cols, rows: rows}
	s.cells = make([][]rune, rows)
	for i := range s.cells {
		s.cells[i] = blankLine(cols)
	}
	return s
}

// InTUI 报告远端当前是否被全屏程序接管：备用屏幕已激活（vim/htop），
// 或「命令期间见过全屏重绘」（top 这类不发备用屏幕序列的，靠 CUP/ED 识别）。
// 前端终端内直接输入据此让路：接管中不本地缓冲按键，原样透传给程序，避免吞键。
func (s *Screen) InTUI() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alt || (s.cmdActive && s.cmdTUI)
}

func blankLine(cols int) []rune {
	line := make([]rune, cols)
	for i := range line {
		line[i] = ' '
	}
	return line
}

// AltActive 报告远端程序当前是否占用着备用屏幕（全屏 TUI 运行中）。
// 调用方（app 层）在 Feed 前后各读一次，比较结果就知道要不要切全屏。
func (s *Screen) AltActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alt
}

// SetTriggers 登记定格钩子（常驻终端用：命令结束/备用屏幕释放时
// 把最后一帧送进快照管线）。传 nil 即摘掉。
func (s *Screen) SetTriggers(onCmdEnd func(tui bool), onAltOff func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCmdEnd, s.onAltOff = onCmdEnd, onAltOff
}

// Resize 调整屏幕几何（窗口变化/全屏切换时前端会重报尺寸）。
// 已有内容不重排：行多则底部补空，行少则截断保留顶部（top/htop 的
// 关键信息在屏幕上端）；列宽变化只影响后续写入的换行与擦除。
func (s *Screen) Resize(cols, rows int) {
	if cols < 2 || cols > 512 || rows < 2 || rows > 512 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rows > len(s.cells) {
		for len(s.cells) < rows {
			s.cells = append(s.cells, blankLine(cols))
		}
	} else if rows < len(s.cells) {
		s.cells = s.cells[:rows]
	}
	for i, line := range s.cells {
		if len(line) != cols {
			nl := blankLine(cols)
			copy(nl, line)
			s.cells[i] = nl
		}
	}
	s.cols, s.rows = cols, rows
	if s.cy >= rows {
		s.cy = rows - 1
	}
	if s.cx >= cols {
		s.cx = cols - 1
	}
}

// Feed 吞一段 PTY 输出。可以任意切块（UTF-8 半字跨块也安全）。
func (s *Screen) Feed(p []byte) {
	s.mu.Lock()
	buf := p
	if len(s.pending) > 0 {
		s.pending = append(s.pending, p...)
		buf = s.pending
		s.pending = nil
	}
	for len(buf) > 0 {
		b := buf[0]
		switch s.state {
		case 0: // ground
			if b >= 0x20 && b != 0x7f {
				r, size := utf8.DecodeRune(buf)
				if r == utf8.RuneError && size == 1 && !utf8.FullRune(buf) {
					// 半个汉字：留到下一块。此处提前返回，必须先解锁 ——
					// Feed 用的是尾部显式 Unlock（为了锁外调钩子），不是 defer，
					// 漏解锁会让下一次 Feed 死锁。这条路径上没有完成的 OSC，
					// 不会有待触发的钩子，直接解锁返回即可。
					s.pending = append(s.pending[:0], buf...)
					s.mu.Unlock()
					return
				}
				s.put(r)
				buf = buf[size:]
				continue
			}
			switch b {
			case 0x1b:
				s.state = 1
			case 0x07: // bell
			case 0x08: // backspace
				if s.cx > 0 {
					s.cx--
				}
			case 0x09: // tab 到下一个 8 列制表位
				s.cx = (s.cx/8 + 1) * 8
				if s.cx >= s.cols {
					s.cx = s.cols - 1
				}
			case 0x0a, 0x0b, 0x0c:
				s.lineDown()
			case 0x0d:
				s.cx = 0
			}
			buf = buf[1:]
		case 1: // esc
			switch {
			case b == '[':
				s.state = 2
				s.csi = s.csi[:0]
				buf = buf[1:]
				continue // 进 CSI 态：不能被下面的回落地拉回 ground
			case b == ']':
				s.state = 3
				buf = buf[1:]
				continue // 进 OSC 态：同理
			case b == '7':
				s.sx, s.sy = s.cx, s.cy
			case b == '8':
				s.cx, s.cy = s.sx, s.sy
			case b == 'c':
				s.clearAll()
			case b == 'M': // 反向索引：光标上移，顶行则整体下滚
				if s.cy == 0 {
					s.scrollDown()
				} else {
					s.cy--
				}
			default:
				// 其它 ESC 序列（字符集选择等）：忽略
			}
			s.state = 0
			buf = buf[1:]
		case 2: // csi：收参数直到最终字节
			switch {
			case b >= 0x30 && b <= 0x3f:
				s.csi = append(s.csi, b)
				buf = buf[1:]
			case b >= 0x40 && b <= 0x7e:
				s.csiFinal(b)
				s.state = 0
				buf = buf[1:]
			default:
				// 非法中间字节：丢掉重来，别卡死在 csi 态
				s.state = 0
				buf = buf[1:]
			}
		case 3: // osc：收 payload 直到 BEL 或 ST（ESC \\），133 系列要解析
			switch {
			case b == 0x07:
				s.oscFinal()
				s.state = 0
				buf = buf[1:]
			case b == 0x1b && len(buf) > 1 && buf[1] == '\\':
				s.oscFinal()
				s.state = 0
				buf = buf[2:]
			default:
				s.osc = append(s.osc, b)
				buf = buf[1:]
			}
		}
	}
	cmdEnd, cmdEndTUI, altOff := s.pendCmdEnd, s.pendCmdEndTUI, s.pendAltOff
	s.pendCmdEnd, s.pendCmdEndTUI, s.pendAltOff = false, false, false
	cmdEndFn, altOffFn := s.onCmdEnd, s.onAltOff
	s.mu.Unlock()
	// 钩子在锁外调：它们会回头读 Text()（拿最后一帧），持锁调必自锁。
	if cmdEnd && cmdEndFn != nil {
		cmdEndFn(cmdEndTUI)
	}
	if altOff && altOffFn != nil {
		altOffFn()
	}
}

// oscFinal 处理一条完整的 OSC payload。只认 shell integration 的
// 133 系列（命令边界）：其余（标题、超链接等）对屏幕模型无意义。
func (s *Screen) oscFinal() {
	p := string(s.osc)
	s.osc = s.osc[:0]
	if len(p) < 5 || p[:4] != "133;" {
		return
	}
	switch p[4] {
	case 'C': // 命令开始：重置本条命令的 TUI 特征
		s.cmdActive, s.cmdTUI = true, false
		s.altDuringCmd = s.alt
	case 'D': // 命令结束：特征成立就挂起一次定格触发
		if s.cmdActive {
			s.cmdActive = false
			s.pendCmdEnd, s.pendCmdEndTUI = true, s.cmdTUI || s.altDuringCmd
		}
	}
}

func (s *Screen) put(r rune) {
	if s.cx >= s.cols {
		s.cx = 0
		s.lineDown()
	}
	s.cells[s.cy][s.cx] = r
	s.cx++
}

func (s *Screen) lineDown() {
	if s.cy == s.rows-1 {
		s.scrollUp()
	} else {
		s.cy++
	}
}

func (s *Screen) scrollUp() {
	copy(s.cells, s.cells[1:])
	s.cells[s.rows-1] = blankLine(s.cols)
}

func (s *Screen) scrollDown() {
	copy(s.cells[1:], s.cells[:s.rows-1])
	s.cells[0] = blankLine(s.cols)
}

func (s *Screen) clearAll() {
	for i := range s.cells {
		s.cells[i] = blankLine(s.cols)
	}
	s.cx, s.cy = 0, 0
}

// csiParams 解析参数串（如 "12;34"）为整数列表，缺省值由调用方补。
func (s *Screen) csiParams(def int) []int {
	// 私有模式（?25l、?1049h 等）：参数段以 '?' 开头，一律忽略
	if len(s.csi) > 0 && s.csi[0] == '?' {
		return nil
	}
	var out []int
	cur, has := -1, false
	for _, c := range s.csi {
		switch {
		case c >= '0' && c <= '9':
			if !has {
				cur, has = 0, true
			}
			cur = cur*10 + int(c-'0')
		case c == ';':
			if !has {
				cur = def
			}
			out = append(out, cur)
			cur, has = -1, false
		}
	}
	if has {
		out = append(out, cur)
	}
	return out
}

func paramAt(list []int, i, def int) int {
	if i < len(list) && list[i] > 0 {
		return list[i]
	}
	return def
}

// isAltModeParam 判断私有模式参数是否是备用屏幕开关
// （1049 = xterm 新式；47/1047 = 旧式。三家都认，兼容性最好）。
func isAltModeParam(p []byte) bool {
	return string(p) == "1049" || string(p) == "47" || string(p) == "1047"
}

func (s *Screen) csiFinal(final byte) {
	// 私有模式设置/复位（CSI ?…h / CSI ?…l）：只关心备用屏幕开关。
	// 它是全屏 TUI 接管/让出屏幕的信号，必须如实记录；其余私有模式
	// （光标显隐 ?25、鼠标追踪 ?1000 等）对文本快照无意义，忽略。
	if len(s.csi) > 0 && s.csi[0] == '?' {
		switch final {
		case 'h':
			if isAltModeParam(s.csi[1:]) {
				s.alt = true
				if s.cmdActive {
					s.altDuringCmd = true
				}
			}
		case 'l':
			if isAltModeParam(s.csi[1:]) && s.alt {
				s.alt = false
				s.pendAltOff = true
			}
		}
		return
	}
	p := s.csiParams(1)
	switch final {
	case 'A': // 上
		s.cy -= paramAt(p, 0, 1)
		if s.cy < 0 {
			s.cy = 0
		}
	case 'B': // 下
		s.cy += paramAt(p, 0, 1)
		if s.cy >= s.rows {
			s.cy = s.rows - 1
		}
	case 'C': // 右
		s.cx += paramAt(p, 0, 1)
		if s.cx >= s.cols {
			s.cx = s.cols - 1
		}
	case 'D': // 左
		s.cx -= paramAt(p, 0, 1)
		if s.cx < 0 {
			s.cx = 0
		}
	case 'G': // 水平绝对列（1 基）
		s.cx = paramAt(p, 0, 1) - 1
		if s.cx < 0 {
			s.cx = 0
		}
		if s.cx >= s.cols {
			s.cx = s.cols - 1
		}
	case 'H', 'f': // 定位（1 基）
		if s.cmdActive {
			s.cmdTUI = true // 流式输出不会定位光标：这是全屏重绘的铁证
		}
		s.cy = paramAt(p, 0, 1) - 1
		if s.cy < 0 {
			s.cy = 0
		}
		if s.cy >= s.rows {
			s.cy = s.rows - 1
		}
		s.cx = paramAt(p, 1, 1) - 1
		if s.cx < 0 {
			s.cx = 0
		}
		if s.cx >= s.cols {
			s.cx = s.cols - 1
		}
	case 'J': // 擦屏
		if s.cmdActive {
			s.cmdTUI = true // 清屏重绘同样是 TUI 特征
		}
		switch paramAt(p, 0, 0) {
		case 0:
			s.eraseLineFrom(s.cx)
			for y := s.cy + 1; y < s.rows; y++ {
				s.cells[y] = blankLine(s.cols)
			}
		case 1:
			s.eraseLineTo(s.cx)
			for y := 0; y < s.cy; y++ {
				s.cells[y] = blankLine(s.cols)
			}
		default:
			s.clearAll()
		}
	case 'K': // 擦行
		switch paramAt(p, 0, 0) {
		case 1:
			s.eraseLineTo(s.cx)
		case 2:
			s.cells[s.cy] = blankLine(s.cols)
		default:
			s.eraseLineFrom(s.cx)
		}
	case 'P': // 删除 n 个字符（左移补齐）
		n := paramAt(p, 0, 1)
		line := s.cells[s.cy]
		if s.cx+n <= s.cols {
			copy(line[s.cx:], line[s.cx+n:])
			for i := s.cols - n; i < s.cols; i++ {
				line[i] = ' '
			}
		} else {
			s.eraseLineFrom(s.cx)
		}
		// 'm'(颜色)、'r'(滚动区)、'L'(插行) 等：文本快照不需要，安全忽略
	}
}

func (s *Screen) eraseLineFrom(x int) {
	for i := x; i < s.cols; i++ {
		s.cells[s.cy][i] = ' '
	}
}

func (s *Screen) eraseLineTo(x int) {
	for i := 0; i <= x && i < s.cols; i++ {
		s.cells[s.cy][i] = ' '
	}
}

// Text 把可见屏幕序列化成纯文本：
//   - 每行去行尾空格（TUI 屏幕满屏空格，不去的话上下文全是噪音）；
//   - 光标所在行加 "> " 前缀，其余行加两空格 —— 列对齐不被破坏，
//     模型又能知道焦点在哪行（vim 的光标位置、top 的选中行都靠它）；
//   - 尾部全空行裁掉（top 只占半屏时不送半屏空行）。
func (s *Screen) Text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, s.rows)
	last := -1
	for y := 0; y < s.rows; y++ {
		trimmed := trimRightSpaces(s.cells[y])
		if trimmed != "" {
			last = y
		}
		prefix := "  "
		if y == s.cy {
			prefix = "> "
		}
		lines[y] = prefix + trimmed
	}
	out := ""
	for y := 0; y <= last; y++ {
		out += lines[y] + "\n"
	}
	return out
}

func trimRightSpaces(line []rune) string {
	end := len(line)
	for end > 0 && line[end-1] == ' ' {
		end--
	}
	return string(line[:end])
}

// ---- 静止快照器 ----

// Snapshotter 在屏幕「静止」（idle 内没有新字节）后取一次快照。
//
// 为什么是静止触发而不是定时轮询：TUI 程序刷新时屏幕是半成品，
// 截到半帧比不截更糟；停止刷新 2 秒意味着这一屏是「给人看稳了」的画面。
// 哈希去重：top 每 3 秒重绘一次但内容可能没变（空闲机器），重复进上下文
// 是纯浪费。
//
// 除静止触发外还有两个「定格」入口（都不等静止，因为语义上就该立即送）：
//   - Flush：TUI 退出备用屏幕那一刻 —— top 按了 q，最后一帧就是答案本身，
//     多等 2 秒只会让 prompt 输出把它冲掉；
//   - Final：PTY 退出 —— 最后机会，不送就永远迷不上了。
type Snapshotter struct {
	screen *Screen
	idle   time.Duration
	// onSnap 的 final 参数区分「静止快照」与「定格快照」：后者在时间线上
	// 语义不同（「结束画面」而非「过程快照」），模型侧的头部说明也不同。
	onSnap func(text string, final bool)

	mu      sync.Mutex
	timer   *time.Timer
	lastSum uint64
	stopped bool
}

func NewSnapshotter(screen *Screen, idle time.Duration, onSnap func(text string, final bool)) *Snapshotter {
	return &Snapshotter{screen: screen, idle: idle, onSnap: onSnap}
}

// Kick 在每次收到 PTY 字节时调用：重置静止计时。
func (sn *Snapshotter) Kick() {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if sn.stopped {
		return
	}
	if sn.timer != nil {
		sn.timer.Stop()
	}
	sn.timer = time.AfterFunc(sn.idle, func() { sn.fire(false) })
}

// fire 取一帧快照并送出（内容与上次相同则静默跳过）。
// final=true 表示这是定格帧（TUI 退出/PTY 结束），会原样传给 onSnap。
func (sn *Snapshotter) fire(final bool) {
	sn.mu.Lock()
	if sn.stopped && !final {
		// 静止触发在停表后作废；但 Final 定格是「最后机会」，
		// 即使已 Stop 也要送 —— 所以它走 final=true 且绕过这里的拦截。
		sn.mu.Unlock()
		return
	}
	sn.mu.Unlock()

	text := sn.screen.Text()
	sum := fnvSum(text)

	sn.mu.Lock()
	if sn.stopped && !final {
		sn.mu.Unlock()
		return
	}
	if sum == sn.lastSum {
		sn.mu.Unlock()
		return
	}
	sn.lastSum = sum
	fn := sn.onSnap
	sn.mu.Unlock()

	if fn != nil {
		fn(text, final)
	}
}

func fnvSum(text string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return h.Sum64()
}

// Flush 立即定格送出当前画面（TUI 退出备用屏幕时调用）。
// 已停表则不送：块都关了，过程帧没有意义（结束帧走 Final）。
func (sn *Snapshotter) Flush() {
	sn.mu.Lock()
	stopped := sn.stopped
	sn.mu.Unlock()
	if stopped {
		return
	}
	sn.fire(true)
}

// Final 停表并送出最后一帧（PTY 退出时调用）。哪怕之前已 Stop 也送：
// 这是把「死亡现场」留给模型和用户的最后机会。内容与上次相同时
// 自动去重，不会重复进上下文。
func (sn *Snapshotter) Final() {
	sn.Stop()
	sn.fire(true)
}

// Stop 停掉计时器（块关闭或复用让位时调用）。幂等。
func (sn *Snapshotter) Stop() {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	sn.stopped = true
	if sn.timer != nil {
		sn.timer.Stop()
		sn.timer = nil
	}
}
