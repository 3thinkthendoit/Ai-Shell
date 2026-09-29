// Package sshclient 负责真正连接 Linux 主机。
//
// 关键设计：凭证只在本包内被解引用，SSH 连接对象从不离开后端。
// agent 拿到的永远只是 host_id 和执行结果，拿不到任何密钥材料。
package sshclient

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"ai-shell/internal/vault"
)

// Result 是一条命令的执行结果。
type Result struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
}

// MaxCaptureBytes 是单条命令**每一路**输出（stdout / stderr 各自）保留的最大字节数。
//
// 为什么必须有上限：Exec 会把输出整段读进内存，而输出大小完全由远端决定。
// 用户敲一句 `cat /var/log/huge` 或 `yes`，就能让后端吃满内存、
// 再让整个字符串经 IPC 进到 WebView 的 DOM 里。这不是理论风险 ——
// ReadFile 早就为此加了 256KB 上限，只是没同步应用到 Exec。
//
// 1 MiB 大致相当于一个终端的滚动回放容量；再多，用户真正需要的做法是
// 用 head / tail / grep 缩小范围，而不是把整份日志倒进界面。
//
// 是变量而不是常量，便于测试把上限压到很小来验证截断行为。
var MaxCaptureBytes = 1 << 20

// cappedBuffer 只保留前 limit 字节，多出来的丢弃但**继续接收**。
//
// 「继续接收」是关键：如果直接停止读取，远端进程写满管道后会阻塞，
// 命令永远不结束，只能等超时被杀 —— 用户看到的是「卡住」，而不是「输出被截断」。
//
// 不需要加锁：ssh 库对同一路输出是单 goroutine 顺序写入的，
// 而读取发生在 Wait() 返回之后。
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int64
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
			c.dropped += int64(len(p) - room)
		}
	} else {
		c.dropped += int64(len(p))
	}
	// 必须报告「全部写入成功」：报短写会让 ssh 的拷贝循环当成错误而中断会话。
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// Client 是带连接复用的 SSH 客户端池。
type Client struct {
	v    *vault.Vault
	mu   sync.Mutex
	pool map[string]*ssh.Client

	// dialing 记录「正在拨号中」的主机，用来保证同一主机同一时刻只拨一次。
	//
	// 为什么需要它：connect() 刻意不在持锁状态下拨号（否则一次慢拨号会卡住
	// 所有其他主机），代价就是同一主机可能被多个 goroutine 同时拨号。
	// 而 dial() 成功后会「关掉池里的旧连接再放入新连接」——如果此时已经有别的
	// goroutine 拿到了那个旧连接，它的命令就会失败（实测 8 个并发首次调用里
	// 有 7 个失败，报 "unexpected packet in response to channel open"）。
	//
	// 加了这层单飞之后，同一主机的拨号被串行化：后来者等前者拨完直接复用结果。
	dialing map[string]chan struct{}

	// running 记录每台主机**当前在跑的那条命令**，用来支持人工中断。
	//
	// 按 hostID 索引而不是引入一个 runID：agent 与其它调用方保证
	// 「同一主机同时只有一条 Exec 在跑」，再让一个 ID 贯穿前后端只会多出
	// 一处可能不同步的状态。代价是并发跑同一主机的多条命令时只有最后一条可中断 ——
	// 而那种用法在当前设计里不存在（agent 是串行的）。
	running map[string]*runningCmd

	// ptys 记录每台主机**当前开着的那条交互终端**（见 pty.go）。
	//
	// 和 pool 分开管理，因为生命周期完全不同：pool 里的连接是短命的、
	// 可以被 Exec 随手关掉，而终端要一直活着直到用户自己关。
	// 每条终端自己独占一条连接，所以这里放的是终端对象而不是连接。
	ptys map[string]*PTY
}

// runningCmd 是一条正在执行的命令，用于支持中断。
//
// 存在的意义是区分「命令自己失败」和「被人为杀掉」：
// 两者都会让 sess.Wait() 返回错误，只看错误分不出来。
type runningCmd struct {
	mu        sync.Mutex
	sess      *ssh.Session
	cancelled bool
	finished  bool
}

// cancel 杀掉这条命令。返回 false 表示它已经结束（或已被取消过），
// 调用方据此区分「确实中断了一条命令」和「按晚了」。
func (r *runningCmd) cancel() bool {
	r.mu.Lock()
	if r.finished || r.cancelled {
		r.mu.Unlock()
		return false
	}
	r.cancelled = true
	r.mu.Unlock()

	// 在锁外做网络 IO：Signal/Close 会等一个往返，持锁做会让
	// 同时到达的 Wait() 结束路径被卡住。
	// 用 SIGKILL 而不是 SIGINT：无 PTY 的会话里信号转发不可靠，
	// 而「用户按了中断」必须真的停住，不能看程序心情。
	_ = r.sess.Signal(ssh.SIGKILL)
	_ = r.sess.Close()
	return true
}

func (r *runningCmd) finish() {
	r.mu.Lock()
	r.finished = true
	r.mu.Unlock()
}

func (r *runningCmd) wasCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// exitCodeKilled 是 shell 对「被 SIGKILL 杀掉」的惯例退出码（128+9）。
// 用它而不是 -1：137 是个真实存在过的约定值，用户看到能对上号。
const exitCodeKilled = 137

// New 创建 SSH 客户端池。
func New(v *vault.Vault) *Client {
	return &Client{
		v:       v,
		pool:    map[string]*ssh.Client{},
		dialing: map[string]chan struct{}{},
		running: map[string]*runningCmd{},
		ptys:    map[string]*PTY{},
	}
}

// Close 关闭所有连接。
func (c *Client) Close() {
	// 先关交互终端：它们各自持有独立连接，不在 pool 里。
	// 漏掉这一步的话，退出应用时终端那条连接会一直挂到进程被回收。
	c.closeAllPTY()

	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cl := range c.pool {
		_ = cl.Close()
		delete(c.pool, k)
	}
}

// Disconnect 关闭指定主机的连接（配置变更后调用）。
func (c *Client) Disconnect(hostID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.pool[hostID]; ok {
		_ = cl.Close()
		delete(c.pool, hostID)
	}
}

// Ping 测试连通性。
func (c *Client) Ping(hostID string) error {
	_, err := c.Exec(hostID, "echo ok", 10*time.Second)
	return err
}

// Exec 在远端执行一条命令。
func (c *Client) Exec(hostID, cmd string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cl, err := c.connect(hostID)
	if err != nil {
		return Result{}, err
	}

	sess, err := cl.NewSession()
	if err != nil {
		c.Disconnect(hostID)
		return Result{}, fmt.Errorf("创建会话失败: %w", err)
	}
	defer sess.Close()

	// 登记为「该主机当前在跑的命令」，让人工中断能找到它。
	// 同一主机已有在跑的命令时直接覆盖：当前设计里不会发生（界面与 agent 都是串行的），
	// 而为此加一道拒绝反而可能把将来某个合理的并发用法堵死。
	rc := &runningCmd{sess: sess}
	c.mu.Lock()
	c.running[hostID] = rc
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		// 只在仍指向自己时删除，避免把后来者的登记清掉
		if c.running[hostID] == rc {
			delete(c.running, hostID)
		}
		c.mu.Unlock()
		rc.finish()
	}()

	var stdout, stderr cappedBuffer
	stdout.limit = MaxCaptureBytes
	stderr.limit = MaxCaptureBytes
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	start := time.Now()
	if err := sess.Start(cmd); err != nil {
		return Result{}, fmt.Errorf("启动命令失败: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case werr := <-done:
		res := Result{
			Stdout:     stdout.String(),
			Stderr:     stderr.String(),
			DurationMs: time.Since(start).Milliseconds(),
			Truncated:  stdout.dropped > 0 || stderr.dropped > 0,
		}
		// 必须先判「是不是被中断的」：被杀掉时 Wait() 返回的是
		// ExitMissingError（远端还没来得及回 exit-status），
		// 和「命令自己退出」混在一起会把中断报成一次普通失败。
		if rc.wasCancelled() {
			res.ExitCode = exitCodeKilled
			return res, fmt.Errorf("命令已被中断")
		}
		var exitErr *ssh.ExitError
		switch {
		case werr == nil:
			res.ExitCode = 0
		case errors.As(werr, &exitErr):
			res.ExitCode = exitErr.ExitStatus()
		default:
			c.Disconnect(hostID)
			return res, fmt.Errorf("命令执行异常: %w", werr)
		}
		return res, nil

	case <-time.After(timeout):
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		<-done
		c.Disconnect(hostID)
		return Result{
			Stdout:     stdout.String(),
			Stderr:     stderr.String(),
			ExitCode:   -1,
			DurationMs: time.Since(start).Milliseconds(),
			Truncated:  stdout.dropped > 0 || stderr.dropped > 0,
		}, fmt.Errorf("命令超时（%s）已被终止", timeout)
	}
}

// Cancel 中断指定主机上正在执行的命令。
//
// 返回 false 表示当时没有在跑的命令（跑完了、或按晚了）。
// 中断成功后不主动断开连接：被杀的只是这条命令的会话，
// 连接本身还能继续用，断开反而让用户的下一条命令多一次握手。
func (c *Client) Cancel(hostID string) bool {
	c.mu.Lock()
	rc := c.running[hostID]
	c.mu.Unlock()
	if rc == nil {
		return false
	}
	return rc.cancel()
}

// ReadFile 读取远端文件（带字节上限，避免把大文件灌进内存）。
func (c *Client) ReadFile(hostID, path string, maxBytes int) (Result, error) {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	// head -c 保证不会因为文件过大而失控；路径用单引号包裹防注入
	q := shellQuote(path)
	return c.Exec(hostID, fmt.Sprintf("head -c %d -- %s", maxBytes, q), 30*time.Second)
}

// ListDir 列目录。
func (c *Client) ListDir(hostID, path string) (Result, error) {
	q := shellQuote(path)
	return c.Exec(hostID, "ls -la -- "+q, 30*time.Second)
}

// WriteFile 通过 stdin 写入远端文件。
//
// 覆写前一定先备份原文件。备份是这个流程里唯一的退路，所以它的成败必须
// 一路传到用户眼前 —— 见 writeFileScript 里的说明。
func (c *Client) WriteFile(hostID, path, content string) (Result, error) {
	cl, err := c.connect(hostID)
	if err != nil {
		return Result{}, err
	}
	sess, err := cl.NewSession()
	if err != nil {
		c.Disconnect(hostID)
		return Result{}, fmt.Errorf("创建会话失败: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// 与 Exec 一致的超时保护：远端磁盘满、脚本卡住时 sess.Run 会无限阻塞，
	// 且该调用不感知 ctx —— 没有超时的话 agent 整轮就挂死在这里。
	const writeTimeout = 60 * time.Second
	type runResult struct{ err error }
	done := make(chan runResult, 1)
	start := time.Now()
	go func() { done <- runResult{sess.Run(writeFileScript(path))} }()

	select {
	case r := <-done:
		if err := r.err; err != nil {
			var exitErr *ssh.ExitError
			if errors.As(err, &exitErr) {
				return Result{
					Stdout:     decorateBackup(stdout.String()),
					Stderr:     stderr.String(),
					ExitCode:   exitErr.ExitStatus(),
					DurationMs: time.Since(start).Milliseconds(),
				}, nil
			}
			return Result{}, err
		}
	case <-time.After(writeTimeout):
		_ = sess.Close() // 解除对 Run 的阻塞，goroutine 随之退出
		<-done
		c.Disconnect(hostID)
		return Result{}, fmt.Errorf("写入文件超时（%s）已被终止", writeTimeout)
	}
	return Result{
		Stdout:     decorateBackup(stdout.String()),
		Stderr:     stderr.String(),
		ExitCode:   0,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// 远端脚本与 Go 侧之间的私有协议标记。它们不会出现在最终界面里 ——
// decorateBackup 会把它们翻译成可读文案。
const (
	backupOKPrefix     = "__AISHELL_BACKUP__="
	backupFailedMarker = "__AISHELL_BACKUP_FAILED__"
	// backupNewFileNote 是脚本在「目标不存在、无需备份」时回传的占位值。
	backupNewFileNote = "(新文件)"
)

// writeFileScript 构造远端写入脚本。
//
// 三个设计要点，每一个都对应一个踩过的坑：
//
//  1. **备份失败必须可见。** 早期版本是 `cp … 2>/dev/null; cat > …` ——
//     `2>/dev/null` 把「磁盘满 / 无权限 / 源文件读不了」的报错全部吞掉，
//     然后在**没有任何退路**的情况下继续覆写目标文件，用户全程无感。
//     现在 cp 的报错原样进 stderr，且失败时立刻 exit 90，绝不继续写。
//  2. **备份名不能只用秒级时间戳。** 原来后缀是 `$(date +%s)`，同一秒内二次
//     写入会生成同一个备份名、把上一次的备份覆盖掉 —— 而「同一秒内改两次」
//     恰恰是 agent 连续修同一个配置时最常见的形态。加上 `-$$`（shell PID）后，
//     同秒并发也不会撞名。
//  3. **「目标不存在」与「备份失败」必须区分。** 二者在旧实现里都表现为
//     cp 失败，所以只能静默；现在用 `[ -e ]` 显式判断，新建文件会明确回报，
//     不会被误读成备份失败。
func writeFileScript(path string) string {
	q := shellQuote(path)
	return fmt.Sprintf(
		`p=%s; bak="$p.bak.$(date +%%Y%%m%%d-%%H%%M%%S).$$"; `+
			`if [ -e "$p" ]; then `+
			`cp -a -- "$p" "$bak" || { echo %s; exit 90; }; `+
			`echo "%s$bak"; `+
			`else echo "%s%s"; fi; `+
			`cat > "$p"`,
		q, backupFailedMarker, backupOKPrefix, backupOKPrefix, backupNewFileNote)
}

// decorateBackup 把脚本回传的私有标记换成给人（和 LLM）看的文案，
// 避免内部协议泄漏到界面与对话里。
func decorateBackup(out string) string {
	if !strings.Contains(out, backupOKPrefix) && !strings.Contains(out, backupFailedMarker) {
		return out
	}
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case t == backupFailedMarker:
			// 失败路径同样要翻译 —— 否则这条内部标记会原样出现在界面上。
			lines[i] = "[写入] 备份原文件失败，已中止写入（原文件未被改动）"
		case strings.HasPrefix(t, backupOKPrefix):
			v := strings.TrimPrefix(t, backupOKPrefix)
			if v == backupNewFileNote {
				lines[i] = "[写入] 目标原不存在，已新建文件（无需备份）"
			} else {
				lines[i] = "[写入] 原文件已备份到 " + v
			}
		}
	}
	return strings.Join(lines, "\n")
}

// SystemInfo 一次性采集系统概览。
func (c *Client) SystemInfo(hostID string) (Result, error) {
	const script = `echo "== os =="; cat /etc/os-release 2>/dev/null | head -4; ` +
		`echo "== kernel =="; uname -a; ` +
		`echo "== uptime =="; uptime; ` +
		`echo "== cpu =="; nproc; ` +
		`echo "== memory =="; free -h 2>/dev/null; ` +
		`echo "== disk =="; df -hT 2>/dev/null | head -12; ` +
		`echo "== failed units =="; systemctl --failed --no-pager 2>/dev/null | head -20; ` +
		`echo "== top mem procs =="; ps -eo pid,comm,%cpu,%mem --sort=-%mem 2>/dev/null | head -8; ` +
		`echo "== listening =="; (ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | head -20`
	return c.Exec(hostID, script, 45*time.Second)
}

// ---- 连接管理 ----

// connect 返回该主机可用的连接，必要时拨号。
//
// 三层意图，顺序不能乱：
//  1. 池里有活连接 → 直接用（ssh.Client 支持并发开多个 session，可以安全共享）；
//  2. 池里的连接已死 → 摘掉并关掉它（只在它仍是池中那一个时才关，避免误关
//     已经被换成新连接的条目），然后重新走一遍；
//  3. 池里没有、也没人在拨 → 自己成为拨号者；若已有别人在拨 → 等它拨完再复用。
//
// 第 3 条就是「单飞」：没有它，同一主机的并发首次调用会互相拆台（见 dialing 字段）。
func (c *Client) connect(hostID string) (*ssh.Client, error) {
	for {
		c.mu.Lock()
		if cl, ok := c.pool[hostID]; ok {
			c.mu.Unlock()
			if isAlive(cl) {
				return cl, nil
			}
			// 连接已死：摘掉并关闭，然后回到循环重新建立。
			c.mu.Lock()
			if cur, still := c.pool[hostID]; still && cur == cl {
				delete(c.pool, hostID)
				c.mu.Unlock()
				_ = cl.Close()
			} else {
				c.mu.Unlock()
			}
			continue
		}

		// 池里没有连接。有人正在拨吗？
		if ch, ok := c.dialing[hostID]; ok {
			c.mu.Unlock()
			// 等待也要有期限：拨号方万一异常挂住（握手阶段对端不回话），
			// 无限等待会让这台主机的所有调用永久卡死。
			select {
			case <-ch: // 等它拨完（成功或失败）
			case <-time.After(30 * time.Second):
				return nil, fmt.Errorf("等待主机 %s 的连接建立超时，请重试", hostID)
			}
			continue
		}

		// 由我负责拨号。登记占位后立刻放锁 —— 拨号期间不能持锁，
		// 否则会卡住其它主机的所有操作。
		ch := make(chan struct{})
		c.dialing[hostID] = ch
		c.mu.Unlock()

		cl, err := c.dial(hostID)

		c.mu.Lock()
		delete(c.dialing, hostID)
		c.mu.Unlock()
		close(ch)

		return cl, err
	}
}

// dial 拨号并把连接放进池子，供 Exec 这类短命令复用。
func (c *Client) dial(hostID string) (*ssh.Client, error) {
	cl, err := c.dialRaw(hostID)
	if err != nil {
		return nil, err
	}

	// 不变量：dial 只会在「池里没有该主机的连接」时被调用（connect 的单飞保证）。
	// 因此这里直接放入即可。
	//
	// 早先的版本会在放入前「关掉池里的旧连接」，那是并发首连失败的根因：
	// 另一个 goroutine 可能已经拿到那个旧连接并在上面开 session，
	// 旧连接被关掉后它就报 "unexpected packet in response to channel open"。
	// 若将来有人破坏这个不变量，这里宁可留下一个未关闭的连接（进程退出时释放），
	// 也不要关掉别人正在用的连接。
	c.mu.Lock()
	c.pool[hostID] = cl
	c.mu.Unlock()
	return cl, nil
}

// dialRaw 只拨号，**不碰连接池**。
//
// 交互终端必须走这条，而不是 dial：它需要一条只属于自己的连接。
// 池里的连接会被 Exec 在超时或异常时顺手关掉（Disconnect），
// 终端要是共用它，用户在另一个标签里跑的一条无关命令超时，
// 就能把他正开着的终端一起掐了 —— 而且是静默的，看不出因果。
func (c *Client) dialRaw(hostID string) (*ssh.Client, error) {
	host, ok := c.v.GetHost(hostID)
	if !ok {
		return nil, fmt.Errorf("主机不存在: %s", hostID)
	}
	sec, ok := c.v.Secret(hostID)
	if !ok && host.AuthMethod != vault.AuthAgent {
		return nil, fmt.Errorf("主机 %s 未配置登录凭据", host.Name)
	}

	auths, err := buildAuthMethods(host, sec)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            host.User,
		Auth:            auths,
		HostKeyCallback: c.hostKeyCallback(host.Addr),
		Timeout:         15 * time.Second,
	}

	cl, err := ssh.Dial("tcp", host.Addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败: %w", host.Addr, err)
	}
	return cl, nil
}

// isAlive 探测连接是否还活着。
// SendRequest(wantReply=true) 在「半开连接」（对端已被 NAT/防火墙静默回收，
// 但本机 TCP 还没感知）上会永久阻塞，因此必须带超时：超时即视为已死，
// 由调用方关闭连接 —— Close 会让卡住的 SendRequest 返回，goroutine 随之退出。
func isAlive(cl *ssh.Client) bool {
	errCh := make(chan error, 1)
	go func() {
		_, _, err := cl.SendRequest("keepalive@openssh.com", true, nil)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		return err == nil
	case <-time.After(5 * time.Second):
		return false
	}
}

func buildAuthMethods(host vault.Host, sec vault.HostSecret) ([]ssh.AuthMethod, error) {
	var auths []ssh.AuthMethod

	switch host.AuthMethod {
	case vault.AuthPassword:
		if sec.Password == "" {
			return nil, errors.New("未配置密码")
		}
		auths = append(auths, ssh.Password(sec.Password))
		auths = append(auths, ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range qs {
				ans[i] = sec.Password
			}
			return ans, nil
		}))

	case vault.AuthPrivateKey:
		var signer ssh.Signer
		var err error
		if sec.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(sec.PrivateKey), []byte(sec.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(sec.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))

	case vault.AuthAgent:
		am, err := agentAuth()
		if err != nil {
			return nil, err
		}
		auths = append(auths, am)

	default:
		return nil, fmt.Errorf("未知的登录方式: %s", host.AuthMethod)
	}

	// 私钥 + 密码兜底：有些主机既允许密钥也允许密码
	if host.AuthMethod == vault.AuthPrivateKey && sec.Password != "" {
		auths = append(auths, ssh.Password(sec.Password))
	}
	return auths, nil
}

func agentAuth() (ssh.AuthMethod, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		if runtime.GOOS == "windows" {
			return nil, errors.New("Windows 下暂不支持 ssh-agent，请改用私钥或密码登录")
		}
		return nil, errors.New("SSH_AUTH_SOCK 未设置，无法使用 ssh-agent")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("连接 ssh-agent 失败: %w", err)
	}
	return ssh.PublicKeysCallback(agent.NewClient(conn).Signers), nil
}

// hostKeyCallback 实现 TOFU：先信 ~/.ssh/known_hosts，其次信本地已记录的指纹，
// 都不命中则记录指纹放行。一旦已记录指纹与实际不符 —— 直接断开（防中间人）。
func (c *Client) hostKeyCallback(addr string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)

		if path := knownHostsPath(); path != "" {
			if cb, err := knownhosts.New(path); err == nil {
				if err := cb(hostname, remote, key); err == nil {
					return nil
				} else {
					var ke *knownhosts.KeyError
					if !errors.As(err, &ke) || len(ke.Want) > 0 {
						return fmt.Errorf("主机密钥与 known_hosts 不匹配，已中断连接（可能是中间人攻击）")
					}
				}
			}
		}

		if saved, ok := c.v.HostKey(addr); ok {
			if saved != fp {
				return fmt.Errorf("主机密钥指纹不匹配！已记录 %s，实际 %s —— 已中断连接", saved, fp)
			}
			return nil
		}

		_ = c.v.SetHostKey(addr, fp)
		return nil
	}
}

func knownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
