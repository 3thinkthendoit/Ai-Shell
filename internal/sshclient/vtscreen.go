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
	"net/url"
	"strconv"
	"strings"
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
	// pendExit 是刚结束那条命令的退出码（OSC 133;D;<code>）。
	// pendExitKnown 为假表示 D 没带码，调用方不要当成失败。
	pendExit      int
	pendExitKnown bool
	// scrollback 是滚出可视区的行（非全屏重绘）。read_console 靠它看到
	// 已经不在当前 24 行里的报错。上限 screenScrollbackLines，超出丢最旧的。
	scrollback []string
	// cwd 是远端 shell 上报的当前工作目录（OSC 7）。空表示还没收到过。
	//
	// 为什么用 OSC 7 而不是「跑一条 pwd 去问」：SSH 的 Exec 是**另开一条连接**，
	// 它的 cwd 恒为家目录，问不出用户此刻在哪。只有常驻的那条 shell 自己知道，
	// 而它每次打印提示符都会经过 PROMPT_COMMAND —— 在那里顺手用 $PWD（bash 内建，
	// 不起子进程）报一次，客户端就**始终**知道当前目录，不需要任何额外往返，
	// 也不会在用户屏幕上闪一行命令回显。
	cwd string
	// 定格钩子：onCmdEnd 在 OSC 133;D（命令结束）时调。
	// tui=这条命令是否全屏重绘过；exitCode/exitKnown 来自 133;D;<code>。
	// onAltOff 在备用屏幕释放瞬间调。均为 nil 安全。
	onCmdEnd func(tui bool, exitCode int, exitKnown bool)
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

// CmdActive 报告当前是否有一条命令正在跑（OSC 133;C 与 133;D 之间）。
//
// 与 InTUI 是**两档**信号，别合并：InTUI 只认全屏程序接管，而 bt 菜单、
// read、REPL、sudo 口令这类非全屏的交互程序不发备用屏幕、也不重绘 ——
// 它们同样随时会读 stdin，按键必须归它们。前端据此把命令运行期间的输入
// 原样透传给 PTY（漏掉这一档的后果：用户答菜单的 `14` 被当自然语言
// 发给 LLM，而菜单一直等不到输入）。
func (s *Screen) CmdActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdActive
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
func (s *Screen) SetTriggers(onCmdEnd func(tui bool, exitCode int, exitKnown bool), onAltOff func()) {
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
	exitCode, exitKnown := s.pendExit, s.pendExitKnown
	s.pendCmdEnd, s.pendCmdEndTUI, s.pendAltOff = false, false, false
	s.pendExit, s.pendExitKnown = 0, false
	cmdEndFn, altOffFn := s.onCmdEnd, s.onAltOff
	s.mu.Unlock()
	// 钩子在锁外调：它们会回头读 Text()（拿最后一帧），持锁调必自锁。
	if cmdEnd && cmdEndFn != nil {
		cmdEndFn(cmdEndTUI, exitCode, exitKnown)
	}
	if altOff && altOffFn != nil {
		altOffFn()
	}
}

// oscFinal 处理一条完整的 OSC payload。
//
// 认两类，其余（窗口标题、超链接等）对屏幕模型无意义，直接丢弃：
//   - 133 系列：shell integration 的命令边界（C=开始，D;退出码=结束）；
//   - 7：当前工作目录，格式 `7;file://<host><path>`（RFC 8089 风格）。
//
// 两类都不会在屏幕上留下任何字符 —— 它们是控制序列，这正是选它们的原因：
// 上报目录不该让用户看到多余输出。
func (s *Screen) oscFinal() {
	p := string(s.osc)
	s.osc = s.osc[:0]
	if len(p) < 2 {
		return
	}
	switch p[0] {
	case '7':
		if cwd, ok := parseOSC7(p); ok {
			s.cwd = cwd
		}
		return
	case '1':
		if len(p) < 5 || p[:4] != "133;" {
			return
		}
	default:
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
			s.pendExit, s.pendExitKnown = parseOSC133Exit(p)
		}
	}
}

// parseOSC133Exit 从 `133;D` / `133;D;<code>` 取出退出码。
// 没带码或不是整数时 known=false，调用方不要当成失败命令。
func parseOSC133Exit(p string) (code int, known bool) {
	// p 形如 133;D 或 133;D;2 或 133;D;2;...
	rest := strings.TrimPrefix(p, "133;D")
	if rest == "" {
		return 0, false
	}
	if !strings.HasPrefix(rest, ";") {
		return 0, false
	}
	rest = rest[1:]
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseOSC7 从 `7;file://host/path` 里取出 path。
//
// 路径需要百分号解码：含空格或中文的目录会被远端 shell 转义成 %20 / %E4%B8%AD
// 之类。不解码的话，界面里显示的是转义串，用户拿去当路径用会找不到目录。
// 解码失败不算错 —— 退回原始串，至少信息还在。
func parseOSC7(p string) (string, bool) {
	rest := strings.TrimPrefix(p, "7;")
	// 只处理 file:// 形式；其它 scheme（rare）忽略。
	if !strings.HasPrefix(rest, "file://") {
		return "", false
	}
	rest = strings.TrimPrefix(rest, "file://")
	// 跳过 host 部分：到第一个 '/' 为止。host 允许为空（file:///path）。
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", false
	}
	path := rest[i:]
	// 空路径说明这条 OSC 7 没带目录（只有 file://host 而没有尾巴），
	// 当作没收到 —— 拿空串去列目录只会得到一个无意义的报错。
	if path == "" {
		return "", false
	}
	if dec, err := url.PathUnescape(path); err == nil {
		path = dec
	}
	return path, true
}

// Cwd 返回远端 shell 上报的当前目录；没收到过 OSC 7 时返回空串。
func (s *Screen) Cwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
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

// screenScrollbackLines 是滚出可视区后仍保留的行数。
// 与 read_console 的最大行数对齐：工具声称能读到滚走的输出。
const screenScrollbackLines = 200

func (s *Screen) scrollUp() {
	// 全屏重绘（top/vim）每帧都滚，记进回滚只会灌一堆半帧。
	// 普通流式输出（报错、日志）才值得留下。
	if !s.alt && !s.cmdTUI {
		s.pushScrollback(trimRightSpaces(s.cells[0]))
	}
	copy(s.cells, s.cells[1:])
	s.cells[s.rows-1] = blankLine(s.cols)
}

func (s *Screen) pushScrollback(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	s.scrollback = append(s.scrollback, line)
	if extra := len(s.scrollback) - screenScrollbackLines; extra > 0 {
		s.scrollback = append([]string(nil), s.scrollback[extra:]...)
	}
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
// TextTail 返回回滚缓冲加上当前可视区，再从尾部截到 maxLines 行。
// maxLines<=0 表示不截。回滚行没有光标前缀；可视区仍用 Text 的 "> " 标记。
func (s *Screen) TextTail(maxLines int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := make([]string, 0, len(s.scrollback)+s.rows)
	lines = append(lines, s.scrollback...)
	vis := strings.TrimRight(s.textLocked(), "\n")
	if vis != "" {
		lines = append(lines, strings.Split(vis, "\n")...)
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func (s *Screen) Text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.textLocked()
}

func (s *Screen) textLocked() string {
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
// 除静止触发外还有三个立即送出的入口（不等静止）：
//   - Capture：普通命令非 0 退出 —— 可见屏上就是报错，「解释刚才的错误」靠它衔接；
//   - Flush：TUI 退出备用屏幕 —— top 按了 q，最后一帧就是答案本身；
//   - Final：PTY 退出 —— 最后机会，不送就永远对不上了。
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
	// abandoned：用户主动关终端 / 切会话时置位。此后 Final 不再送帧 ——
	// 有意义的 TUI 画面在退出全屏时已 Flush；再定格只会把登录 MOTD
	// 灌进对话，纯噪音。远端自己 exit / 断连仍走 Final（未 Abandon）。
	abandoned bool
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
	if sn.abandoned || (sn.stopped && !final) {
		// 静止触发在停表后作废；Final 走 final=true，停表后仍可送。
		// Abandon 之后连定格帧也不送：主动关闭不该再把屏幕送出去。
		sn.mu.Unlock()
		return
	}
	sn.mu.Unlock()

	text := sn.screen.Text()
	sum := fnvSum(text)

	sn.mu.Lock()
	if sn.abandoned || (sn.stopped && !final) {
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

// Capture 把当前可见屏幕作为过程快照送出（final=false）。
// 失败的普通命令用它：报错还在这一屏上，但终端并没有结束。
func (sn *Snapshotter) Capture() {
	sn.mu.Lock()
	stopped := sn.stopped
	sn.mu.Unlock()
	if stopped {
		return
	}
	sn.fire(false)
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
// 自动去重，不会重复进上下文。已 Abandon 的不送。
func (sn *Snapshotter) Final() {
	sn.Stop()
	sn.mu.Lock()
	abandoned := sn.abandoned
	sn.mu.Unlock()
	if abandoned {
		return
	}
	sn.fire(true)
}

// Abandon 停表并放弃定格：主动关闭 / 切会话时调用，阻止后续 Final。
func (sn *Snapshotter) Abandon() {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	sn.stopped = true
	sn.abandoned = true
	if sn.timer != nil {
		sn.timer.Stop()
		sn.timer = nil
	}
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
