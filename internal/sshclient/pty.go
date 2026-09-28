package sshclient

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
)

// MaxPTYTotal 是同时打开的交互终端数量上限。
//
// 每个终端占一条**独占的** SSH 连接（见 OpenPTY 的注释），所以这个数字
// 直接等于常驻连接数。不设上限的话，用户在主机列表里来回点几下就能
// 攒出几十条连接，把本地和远端的 MaxSessions 都占满 —— 表现是
// 「别的命令突然连不上了」，而原因离现场很远。
const MaxPTYTotal = 8

// TerminalSize 是一次终端窗口的尺寸。
type TerminalSize struct {
	Cols int
	Rows int
}

// normalized 把非法尺寸退化成一个能用的默认值。
//
// 前端的 FitAddon 在容器还没布局完时可能算出 0 —— 那不是错误，
// 只是「还不知道」，用 80x24 兜住即可。传 0 给远端会让 top 之类的
// 程序按 0 列排版，画面直接崩掉。
func (s TerminalSize) normalized() TerminalSize {
	if s.Cols <= 0 {
		s.Cols = 80
	}
	if s.Rows <= 0 {
		s.Rows = 24
	}
	return s
}

// PTY 是一条常驻的交互式终端会话。
//
// 它和 Exec 的区别不是「多一个参数」，而是根本性的：
//   - Exec 每条命令新建一条 session、不申请伪终端、输出收完才返回；
//   - PTY 申请伪终端并跑一个交互式 shell，输出边产生边推，按键边按边传。
//
// 也正因为如此，Exec 那一套「超时杀掉、输出留前 1MiB」的模型在这里完全不适用：
// 终端本来就是要一直开着的，也没有「输出总量」这个概念。
type PTY struct {
	hostID string
	conn   *ssh.Client // 独占连接，见 OpenPTY
	sess   *ssh.Session
	stdin  io.WriteCloser

	onData func([]byte)
	onExit func(string)

	mu     sync.Mutex
	closed bool // 本地已关闭：Write / Resize 直接拒绝
	exited bool // onExit 已触发过（保证恰好一次）
	done   chan struct{}
}

// alive 表示这条终端还活着，可以被复用。
func (p *PTY) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && !p.exited
}

// HostID 返回这条终端属于哪台主机。
func (p *PTY) HostID() string { return p.hostID }

// Done 在终端结束时关闭，便于测试等待。
func (p *PTY) Done() <-chan struct{} { return p.done }

// ErrPTYClosed 是「终端已经关了还往里写」的统一错误。
//
// 单独定义一个而不是直接返回 io.ErrClosedPipe：调用方（界面）需要
// 区分「终端关了」（正常，用户自己关的，不必报错）和「写入真的失败了」。
var ErrPTYClosed = errors.New("交互终端已关闭")

// Write 把用户的按键送到远端。
func (p *PTY) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	p.mu.Lock()
	if p.closed || p.exited {
		p.mu.Unlock()
		return ErrPTYClosed
	}
	w := p.stdin
	p.mu.Unlock()

	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("发送按键失败: %w", err)
	}
	return nil
}

// Resize 通知远端终端尺寸变了。
//
// 少了这一步，远端程序会一直以为自己还是 80x24：top 的进程列表会挤在
// 左边一小条里，vim 会画错半屏。窗口尺寸不是装饰，是程序排版依据。
func (p *PTY) Resize(size TerminalSize) error {
	size = size.normalized()
	p.mu.Lock()
	if p.closed || p.exited {
		p.mu.Unlock()
		return ErrPTYClosed
	}
	s := p.sess
	p.mu.Unlock()

	// 又是那个反直觉的顺序：WindowChange(h, w) —— 高在前、宽在后。
	// 写反的后果不是报错，而是终端被悄悄改成 24 列 80 行，
	// 表现为「排版有点怪」，很难联想到是参数顺序。
	if err := s.WindowChange(size.Rows, size.Cols); err != nil {
		return fmt.Errorf("调整终端尺寸失败: %w", err)
	}
	return nil
}

// Close 关掉这条终端。
//
// 可以重复调用。关掉 session 会让远端的 shell 收到 SIGHUP 而退出；
// 独占的连接也一并关掉，不留半开状态。
func (p *PTY) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	_ = p.sess.Close()
	_ = p.conn.Close()
	return nil
}

// finish 触发一次 onExit，重复调用无效果。
func (p *PTY) finish(reason string) {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return
	}
	p.exited = true
	cb := p.onExit
	close(p.done)
	p.mu.Unlock()

	if cb != nil {
		cb(reason)
	}
}

// ptyWriter 把远端来的字节交给 onData。
//
// 两个细节不能省：
//   - **必须复制**。ssh 库传进来的切片是复用的，而 onData 会把这块内存
//     交给另一个 goroutine 去 base64 编码、发事件；不复制就会读到下一批数据。
//   - **必须加锁**。ssh 库对 stdout / stderr 是各起一个 goroutine 拷贝的，
//     两个流可能同时到达；终端的画面是按到达顺序拼出来的，交错写入会画乱。
type ptyWriter struct{ p *PTY }

func (w ptyWriter) Write(b []byte) (int, error) {
	chunk := make([]byte, len(b))
	copy(chunk, b)

	w.p.mu.Lock()
	cb, gone := w.p.onData, w.p.closed
	w.p.mu.Unlock()

	// 关闭之后到达的尾巴照收但不派发：仍然要返回 len(b)，
	// 否则 ssh 库会把这个当成写失败，在 Wait() 上多报一个无关的错误。
	if cb != nil && !gone {
		cb(chunk)
	}
	return len(b), nil
}

// ptyExitReason 把 Wait() 的错误翻译成一句给用户看的原因。
func ptyExitReason(err error) string {
	if err == nil {
		return "远端 shell 已退出"
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("远端 shell 已退出（退出码 %d）", exitErr.ExitStatus())
	}
	if errors.Is(err, io.EOF) {
		return "远端已断开连接"
	}
	return "终端连接中断：" + err.Error()
}

// OpenPTY 在指定主机上开一条交互式终端。
//
// 已经开着且还活着时**直接复用**，不会换一条新的：调用方（界面）在切模式、
// 切主机时会反复调这里，而用户已经 cd 过去的目录、跑着的进程都在那条会话里，
// 悄悄重开会把它们全部丢掉，界面却以为只是「又打开了一次」。
//
// onData 与 onExit 都会被在**独立 goroutine** 上调用，实现里不能假设
// 自己在调用方的那条栈上。onExit 恰好触发一次；onData 收到的字节块
// 调用方可以持有（内部已经复制过）。
func (c *Client) OpenPTY(hostID, term string, size TerminalSize, onData func([]byte), onExit func(string)) (*PTY, error) {
	if term == "" {
		term = "xterm-256color"
	}
	size = size.normalized()

	c.mu.Lock()
	if old, ok := c.ptys[hostID]; ok && old.alive() {
		c.mu.Unlock()
		return old, nil
	}
	// 顺手把已经死掉的条目清掉，再数上限 —— 否则用户开开关关几次之后，
	// 明明只有一条活的也会被告知「已达上限」。
	for id, p := range c.ptys {
		if !p.alive() {
			delete(c.ptys, id)
		}
	}
	if len(c.ptys) >= MaxPTYTotal {
		c.mu.Unlock()
		return nil, fmt.Errorf("同时打开的交互终端已达上限（%d 个），请先关掉一些", MaxPTYTotal)
	}
	c.mu.Unlock()

	// 拨号与建会话都不持锁：可能很慢，持锁会卡住所有其他主机的操作。
	// 代价是并发调用同一主机时可能白做一次，由下面的二次检查兜住。
	p, err := c.openPTY(hostID, term, size, onData, onExit)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if cur, ok := c.ptys[hostID]; ok && cur.alive() {
		c.mu.Unlock()
		_ = p.Close() // 白做的那条：关掉，别泄漏一条 SSH 连接
		return cur, nil
	}
	if len(c.ptys) >= MaxPTYTotal {
		c.mu.Unlock()
		_ = p.Close()
		return nil, fmt.Errorf("同时打开的交互终端已达上限（%d 个），请先关掉一些", MaxPTYTotal)
	}
	c.ptys[hostID] = p
	c.mu.Unlock()
	return p, nil
}

// openPTY 真正建立终端，不碰注册表。
func (c *Client) openPTY(hostID, term string, size TerminalSize, onData func([]byte), onExit func(string)) (*PTY, error) {
	// 独占一条连接，**刻意不走连接池**（用 dialRaw 而不是 dial）。
	//
	// 池里的连接会被 Exec 顺手关掉：命令超时、或 Wait() 报出非 ExitError 时，
	// Exec 都会调 Disconnect(hostID) 把池里那条连接关掉。交互终端不能接受
	// 这种连坐 —— 用户在另一个标签里跑了一条无关的命令超时，不该把他正开着的
	// 终端一起掐了。代价是每台开着终端的主机多一次握手，对终端而言这是正常开销。
	conn, err := c.dialRaw(hostID)
	if err != nil {
		return nil, err
	}

	sess, err := conn.NewSession()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("创建会话失败: %w", err)
	}

	modes := ssh.TerminalModes{
		// ECHO=1：让远端的行规程回显用户输入。这是真终端的行为 ——
		// 关掉它用户就看不见自己敲了什么。
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	// RequestPty(term, h, w, modes)：**高在前、宽在后**，和直觉相反。
	// 传反了不会报错，只会得到一个 80 行 24 列的终端，画面「有点怪」。
	if err := sess.RequestPty(term, size.Rows, size.Cols, modes); err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("申请伪终端失败: %w", err)
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("打开终端输入通道失败: %w", err)
	}

	p := &PTY{
		hostID: hostID,
		conn:   conn,
		sess:   sess,
		stdin:  stdin,
		onData: onData,
		onExit: onExit,
		done:   make(chan struct{}),
	}

	// stdout 与 stderr 都指向同一个 writer：申请了伪终端之后，远端是把
	// stderr 也接到伪终端上的，正常情况只会走 stdout 这一路；但万一某个
	// 服务端仍用扩展数据通道发，这里也不会把它悄悄丢掉。
	sess.Stdout = ptyWriter{p}
	sess.Stderr = ptyWriter{p}

	if err := sess.Shell(); err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("启动远端 shell 失败: %w", err)
	}

	// Wait() 会等所有输出拷贝结束才返回，所以 onExit 一定发生在
	// 最后一块输出之后 —— 界面不会先收到「已退出」再收到残帧。
	go func() { p.finish(ptyExitReason(sess.Wait())) }()

	return p, nil
}

// ClosePTY 关掉某台主机的交互终端。返回 false 表示本来就没开着。
func (c *Client) ClosePTY(hostID string) bool {
	c.mu.Lock()
	p, ok := c.ptys[hostID]
	if ok {
		delete(c.ptys, hostID)
	}
	c.mu.Unlock()

	if !ok {
		return false
	}
	_ = p.Close()
	return true
}

// closeAllPTY 关闭所有交互终端（供 Close 调用）。
func (c *Client) closeAllPTY() {
	c.mu.Lock()
	all := make([]*PTY, 0, len(c.ptys))
	for _, p := range c.ptys {
		all = append(all, p)
	}
	c.ptys = map[string]*PTY{}
	c.mu.Unlock()

	for _, p := range all {
		_ = p.Close()
	}
}

// PTYCount 返回当前还活着的交互终端数量（供界面与测试使用）。
func (c *Client) PTYCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.ptys {
		if p.alive() {
			n++
		}
	}
	return n
}

// PTY 返回某台主机当前开着的终端；没有则返回 nil。
func (c *Client) PTY(hostID string) *PTY {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ptys[hostID]
}
