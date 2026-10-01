// Package sshtest 提供一个进程内的真实 SSH 服务器，供集成测试使用。
//
// 它不是 mock —— 是一个走完整 TCP + SSH 握手 + 会话 + exec 通道的真服务器。
// 只有被测试代码 import，因此不会进入最终二进制。
//
// 之所以不用 Docker 起真 Linux：进程内服务器可在 CI 里稳定重复，无外部依赖，
// 而我们要验证的是**客户端**（认证、指纹、通道、退出码、超时），不是远端行为。
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// 测试账号与固定数据。
const (
	User     = "tester"
	KeyUser  = "keyuser"
	Password = "correct-horse-battery-staple"

	// LeakedSecret 模拟「配置文件里夹带了密钥」的场景，用于验证输出脱敏。
	LeakedSecret = "SuperSecretDBPassword123"

	// 下面这一组模拟**真实世界最常见**的泄漏形态：不是 `password = x`，
	// 而是 .env / env / docker inspect 那种「带前缀的键名 + 平台 token」。
	//
	// 之所以要单独造一份：原来的夹具只用了 `password = ...`，
	// 恰好是脱敏实现本来就能处理的形态，于是带前缀的键名（DB_PASSWORD）、
	// 云厂商 token（AWS_SECRET_ACCESS_KEY）、粘连写法（PGPASSWORD）
	// 整类漏网时，端到端用例照样全绿。
	EnvStyleSecret = "hunter2-not-a-real-password"
	AWSStyleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	JWTStyleSecret = "abcdef1234567890abcdef"
	EnvStylePath   = "/srv/billing.conf"

	// YAMLEnvSecret 模拟「键名与密钥分处两行」的 env 块：
	//
	//	- name: REDIS_PASSWORD
	//	  value: <secret>
	//
	// 这是 `kubectl get pod -o yaml` / `docker inspect` / docker-compose 的
	// 标准形状。单看任意一行都不敏感（name / value 都是普通键名），
	// 所以必须做跨行结构感知才能抓到 —— 曾经是明确记录的泄漏缺口。
	YAMLEnvSecret = "yaml-env-block-secret-9911"

	// ShortOptSecret 模拟「命令行短选项里的密码」：
	//
	//	mysql -p<secret> mydb
	//
	// 泄漏路径是命令回显 —— `ps aux`、shell 历史、`set -x` 追踪、
	// 以及程序报错时把整条命令行打出来。`-p` 单独看歧义极大
	// （ssh/mkdir/docker 里含义都不同），所以只在认出命令身份时才认。
	ShortOptSecret = "short-opt-secret-7788"

	// ConfPath 是那个夹带密钥的配置文件路径。
	ConfPath = "/srv/app.conf"
	// NginxPath 是普通配置文件，用于对照。
	NginxPath = "/etc/nginx/nginx.conf"
)

// Server 是运行中的测试 SSH 服务器。
type Server struct {
	Addr      string
	HostKeyFP string
	ln        net.Listener

	fs *memFS
	// failBackup 为真时，写文件的备份阶段会失败（模拟磁盘满/无权限），
	// 用于验证「备份失败就中止、绝不继续覆写」。
	failBackup atomic.Bool

	ptyMu   sync.Mutex
	lastPTY *PTYRequest
	resizes []Size
}

// PTYRequest 记录一次 pty-req 的内容。
//
// 之所以必须能断言「远端实际收到什么」，而不是相信调用点：伪终端的尺寸
// 在两侧的字段顺序是相反的 —— 客户端侧 RequestPty(term, 高, 宽)，
// 而 SSH 报文里是「宽、高」。任何一处写反都不会报错，只会得到一个
// 尺寸颠倒的终端，表现为「排版有点怪」，很难联想到参数顺序。
type PTYRequest struct {
	Term string
	Cols int
	Rows int
}

// Size 是一次终端尺寸（列、行）。
//
// 刻意不复用 sshclient.TerminalSize：sshclient 的测试要 import 本包，
// 本包再反向 import sshclient 会构成测试期的 import cycle。
type Size struct {
	Cols int
	Rows int
}

func (s *Server) recordPTY(p PTYRequest) {
	s.ptyMu.Lock()
	s.lastPTY = &p
	s.ptyMu.Unlock()
}

func (s *Server) recordResize(sz Size) {
	s.ptyMu.Lock()
	s.resizes = append(s.resizes, sz)
	s.ptyMu.Unlock()
}

// LastPTY 返回最近一次 pty-req；第二个返回值为 false 表示从没收到过。
func (s *Server) LastPTY() (PTYRequest, bool) {
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	if s.lastPTY == nil {
		return PTYRequest{}, false
	}
	return *s.lastPTY, true
}

// Resizes 返回收到的所有 window-change 尺寸，按到达顺序。
func (s *Server) Resizes() []Size {
	s.ptyMu.Lock()
	defer s.ptyMu.Unlock()
	return append([]Size(nil), s.resizes...)
}

// FailBackup 让后续的写文件操作在备份阶段失败。
func (s *Server) FailBackup(v bool) { s.failBackup.Store(v) }

// ReadRemoteFile 读取测试服务器上的文件内容（测试断言用）。
func (s *Server) ReadRemoteFile(path string) (string, bool) { return s.fs.read(path) }

// SetRemoteFile 预置或覆盖一个文件。
func (s *Server) SetRemoteFile(path, content string) { s.fs.write(path, content) }

// RemoveRemoteFile 删除文件，用于构造「目标不存在」的场景。
func (s *Server) RemoveRemoteFile(path string) { s.fs.remove(path) }

// Backups 返回某路径的所有备份文件名（已排序），用于断言备份是否真的产生了。
func (s *Server) Backups(path string) []string { return s.fs.backups(path) }

// Start 在 127.0.0.1 的随机端口启动服务器，测试结束时自动关闭。
func Start(tb testing.TB) *Server {
	tb.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		tb.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == User && string(pass) == Password {
				return nil, nil
			}
			return nil, fmt.Errorf("认证失败")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == KeyUser {
				return nil, nil
			}
			return nil, fmt.Errorf("认证失败")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	s := &Server{
		Addr:      ln.Addr().String(),
		HostKeyFP: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		ln:        ln,
		fs:        newMemFS(),
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn, cfg, s)
		}
	}()

	tb.Cleanup(s.Close)
	return s
}

// Close 停止服务器。
func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

// UserKeyPEM 生成一把 OpenSSH 格式的用户私钥，用于测试公钥认证路径。
func UserKeyPEM(tb testing.TB) string {
	tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		tb.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func handleConn(c net.Conn, cfg *ssh.ServerConfig, srv *Server) {
	defer c.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "仅支持 session")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, chReqs, srv)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, srv *Server) {
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			p, ok := parsePTYRequest(req.Payload)
			if !ok {
				_ = req.Reply(false, nil)
				continue
			}
			srv.recordPTY(p)
			_ = req.Reply(true, nil)

		case "window-change":
			// 客户端发这个请求时 want-reply 是 false，Reply 因此是空操作，
			// 但调一次无害，也让「想回就回」这条规则保持一致。
			if sz, ok := parseWindowChange(req.Payload); ok {
				srv.recordResize(sz)
			}
			_ = req.Reply(true, nil)

		case "shell":
			_ = req.Reply(true, nil)
			go func() {
				code := fakeShell(ch)
				sendExitStatus(ch, code)
				_ = ch.Close()
			}()
			// **不能 return**：shell 会话是长命的，后面还会来
			// window-change、signal 等请求，退出循环就再没人处理它们了 ——
			// 表现是终端尺寸永远不变（top 一直按初始尺寸排版）。
			// 只有 exec 那种「一条命令一条会话」才该 return。
			continue

		case "exec":
			cmd := parseExecPayload(req.Payload)
			_ = req.Reply(true, nil)
			go func() {
				code := RunCommand(cmd, ch, srv)
				sendExitStatus(ch, code)
				_ = ch.Close()
			}()
			return // 一条会话只执行一条命令
		default:
			_ = req.Reply(false, nil)
		}
	}
	_ = ch.Close()
}

// parsePTYRequest 解析 pty-req 的载荷（RFC 4254 §6.2）：
//
//	string  TERM
//	uint32  终端宽度（列）
//	uint32  终端高度（行）
//	uint32  宽度（像素）、uint32 高度（像素）、string 编码后的终端模式
//
// 注意这里的顺序是**宽在前、高在后**，与客户端侧 RequestPty(term, 高, 宽)
// 恰好相反。两处都对才能对上。
func parsePTYRequest(p []byte) (PTYRequest, bool) {
	term, rest, ok := readString(p)
	if !ok {
		return PTYRequest{}, false
	}
	cols, rest, ok := readUint32(rest)
	if !ok {
		return PTYRequest{}, false
	}
	rows, _, ok := readUint32(rest)
	if !ok {
		return PTYRequest{}, false
	}
	return PTYRequest{Term: term, Cols: int(cols), Rows: int(rows)}, true
}

// parseWindowChange 解析 window-change：uint32 宽（列）、uint32 高（行）。
func parseWindowChange(p []byte) (Size, bool) {
	cols, rest, ok := readUint32(p)
	if !ok {
		return Size{}, false
	}
	rows, _, ok := readUint32(rest)
	if !ok {
		return Size{}, false
	}
	return Size{Cols: int(cols), Rows: int(rows)}, true
}

func readString(p []byte) (string, []byte, bool) {
	if len(p) < 4 {
		return "", nil, false
	}
	n := int(binary.BigEndian.Uint32(p[:4]))
	if n < 0 || len(p)-4 < n {
		return "", nil, false
	}
	return string(p[4 : 4+n]), p[4+n:], true
}

func readUint32(p []byte) (uint32, []byte, bool) {
	if len(p) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(p[:4]), p[4:], true
}

// fakeShell 是一个**能被断言的假 shell**。
//
// 它不是真 shell —— 我们测的是客户端：伪终端申请得对不对、按键到没到远端、
// 尺寸变化有没有传过去。所以它只做三件事，每件都能从输出里看出来：
//
//  1. 先把提示符写出去（真实 shell 一启动就会打印提示符，这也是最容易
//     被前端漏掉的一段输出）；
//  2. 把收到的字节原样回显 —— 真终端下这一步由远端的行规程完成，
//     这里没有真 pty，只能自己来；
//  3. 收到回车就把这一行交给 RunCommand 真执行，并在前后包上
//     OSC 133 命令边界标记（C=开始、D;code=结束），与客户端给真 shell
//     注入的 init 片段同一套序列；收到 `exit` 就正常退出，用于验证退出路径。
func fakeShell(ch ssh.Channel) int {
	const prompt = "sh$ "
	_, _ = io.WriteString(ch, prompt)

	var line []byte
	buf := make([]byte, 1024)
	for {
		n, err := ch.Read(buf)
		for _, b := range buf[:n] {
			switch b {
			case '\r', '\n':
				_, _ = io.WriteString(ch, "\r\n")
				cmd := strings.TrimSpace(string(line))
				line = line[:0]
				if cmd == "exit" {
					return 0
				}
				if cmd == "" {
					_, _ = io.WriteString(ch, prompt)
					continue
				}
				// shell integration 标记：命令边界靠 OSC 133 报给客户端的
				// VT 模型（C=开始，D;code=结束），与真 shell 注入的
				// init 片段同一套序列；「命令结束且全屏重绘过→定格
				// 最后一帧」的链路要靠它端到端验证。
				_, _ = io.WriteString(ch, "\x1b]133;C\x07")
				code := RunCommand(cmd, ch, nil)
				_, _ = fmt.Fprintf(ch, "\x1b]133;D;%d\x07%s", code, prompt)
			default:
				line = append(line, b)
				_, _ = ch.Write([]byte{b})
			}
		}
		if err != nil {
			// 客户端关掉通道时走到这里，属于正常结束。
			return 0
		}
	}
}

func parseExecPayload(p []byte) string {
	if len(p) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(p[:4])
	if int(n) > len(p)-4 {
		return ""
	}
	return string(p[4 : 4+n])
}

func sendExitStatus(ch ssh.Channel, code int) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(code))
	_, _ = ch.SendRequest("exit-status", false, payload)
}

// memFS 是测试服务器上的极简内存文件系统。
//
// 它存在是为了能对 write_file 做端到端断言 —— 写文件是全流程里破坏性最强的操作
// （直接覆盖远端生产配置），却一度完全没有测试覆盖。
type memFS struct {
	mu    sync.Mutex
	files map[string]string
}

func newMemFS() *memFS {
	return &memFS{files: map[string]string{
		ConfPath:  fmt.Sprintf("listen = 0.0.0.0:8080\npassword = %s\nretries = 3\n", LeakedSecret),
		NginxPath: "user nginx;\nworker_processes auto;\nevents { worker_connections 1024; }\n",
		// 形态来自真实的 `cat app.conf` / `env` / `docker inspect` / `ps` 输出：
		// 带前缀的键名、云厂商 token、粘连写法、JSON 风格的引号键名、
		// kubectl/docker 的 env 块（键名与密钥分处两行）、
		// 以及命令回显里的短选项密码（ps 能看到）。
		EnvStylePath: fmt.Sprintf(`app_name = "billing"
listen = 0.0.0.0:9090
DB_PASSWORD=%s
AWS_SECRET_ACCESS_KEY=%s
JWT_SECRET=%s
PGPASSWORD=%s
MAX_TOKENS=4096
PWD=/srv
{"api_key": "%s"}
env:
- name: REDIS_PASSWORD
  value: %s
# ps 里的启动命令行
root  1842  0.0  0.2  12345  6789 ?  Ssl  10:00  0:01 mysql -p%s billing
`, EnvStyleSecret, AWSStyleSecret, JWTStyleSecret, EnvStyleSecret, EnvStyleSecret,
			YAMLEnvSecret, ShortOptSecret),
	}}
}

func (f *memFS) read(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	return c, ok
}

func (f *memFS) write(p, c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = c
}

func (f *memFS) remove(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
}

// backups 返回某路径的所有备份文件名，按名字排序。
func (f *memFS) backups(p string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.files {
		if strings.HasPrefix(k, p+".bak.") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ---- 与 sshclient.WriteFile 的私有协议保持一致 ----
//
// 这几个常量必须与 internal/sshclient/client.go 里的同名常量一致。
// 刻意不做跨包共享：一旦对方改了协议，相关测试会立刻失败 —— 这正是想要的信号。
const (
	backupOKPrefix     = "__AISHELL_BACKUP__="
	backupFailedMarker = "__AISHELL_BACKUP_FAILED__"
	backupNewFileNote  = "(新文件)"
	writeMarkerProbe   = "__AISHELL_BACKUP__"
)

// backupSeq 代替真实脚本里的 `-$$`（shell PID），保证同一「秒」内多次写入
// 也能拿到不同的备份名 —— 真实脚本靠 $$，测试里靠自增。
var backupSeq atomic.Int64

// extractWritePath 从写脚本里取出被 shellQuote 过的目标路径。
func extractWritePath(cmd string) (string, bool) {
	const lead = "p='"
	i := strings.Index(cmd, lead)
	if i < 0 {
		return "", false
	}
	rest := cmd[i+len(lead):]
	var sb strings.Builder
	for j := 0; j < len(rest); j++ {
		if rest[j] != '\'' {
			sb.WriteByte(rest[j])
			continue
		}
		// shellQuote 把内层单引号写成 '\''：先闭合、再转义、再开启
		if strings.HasPrefix(rest[j:], `'\''`) {
			sb.WriteByte('\'')
			j += 3
			continue
		}
		return sb.String(), true
	}
	return "", false
}

// runWriteScript 模拟 sshclient.WriteFile 的远端脚本：备份已存在的目标 →
// 回报备份名 → 从 stdin 读入新内容落盘。
//
// 这里刻意「忠实」地模拟了脚本的备份命名逻辑：脚本里带 `$$`（shell PID）才生成
// 唯一名，否则同一秒内会撞名。于是如果有人把 `$$` 去掉（回归），备份会互相覆盖，
// TestWriteFileBackupNamesDoNotCollide 就会失败。
func runWriteScript(cmd string, ch ssh.Channel, fs *memFS, failBackup bool) int {
	p, ok := extractWritePath(cmd)
	if !ok {
		fmt.Fprint(ch.Stderr(), "test-sshd: 无法从写脚本中解析目标路径\n")
		return 127
	}

	old, exists := fs.read(p)
	if exists {
		if failBackup {
			fmt.Fprint(ch.Stderr(), "cp: cannot create regular file '"+p+".bak': No space left on device\n")
			fmt.Fprintln(ch, backupFailedMarker)
			return 90
		}
		suffix := "20260927-100000"
		if strings.Contains(cmd, "$$") {
			suffix = fmt.Sprintf("%s.%d", suffix, backupSeq.Add(1))
		}
		bak := p + ".bak." + suffix
		fs.write(bak, old)
		fmt.Fprintf(ch, "%s%s\n", backupOKPrefix, bak)
	} else {
		fmt.Fprintf(ch, "%s%s\n", backupOKPrefix, backupNewFileNote)
	}

	// 脚本最后是 `cat > "$p"`：内容从 stdin 来
	body, _ := io.ReadAll(ch)
	fs.write(p, string(body))
	return 0
}

// writeBulk 往指定流写**恰好** n 字节（分块写，模拟真实程序的连续输出）。
//
// 精确到字节是刻意的：测试要验证「正好等于上限」与「超出上限一字节」这两种
// 边界行为，多写一块就没法区分了。
func writeBulk(ch ssh.Channel, arg string, toStderr bool) {
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(arg), "%d", &n)
	if n <= 0 {
		return
	}
	chunk := []byte(strings.Repeat("x", 4096) + "\n")
	w := io.Writer(ch)
	if toStderr {
		w = ch.Stderr()
	}
	for written := 0; written < n; {
		buf := chunk
		if remaining := n - written; remaining < len(buf) {
			buf = buf[:remaining]
		}
		if _, err := w.Write(buf); err != nil {
			return // 客户端断了就别再写，否则会一直卡在这
		}
		written += len(buf)
	}
}

// RunCommand 是一个极简的命令解释器，覆盖测试需要的行为。
// 它不是真 shell —— 我们测的是客户端，不是远端。
func RunCommand(cmd string, ch ssh.Channel, srv *Server) int {
	c := strings.TrimSpace(cmd)

	var fs *memFS
	failBackup := false
	if srv != nil {
		fs = srv.fs
		failBackup = srv.failBackup.Load()
	} else {
		fs = newMemFS()
	}

	// 人工 shell 会包一层「cd <dir> && …」；剥掉后再匹配具体命令。
	c = stripCdPrefix(c)

	switch {
	// 客户端 OpenTerminal 注入的 shell integration 行（stty 三写 + 单行片段）。
	// 假远端接受但忽略：133 标记由 fakeShell 自己发，这里若报 unknown
	// 会污染输出并让注入行带上 133;D;127 的假命令边界。
	case strings.HasPrefix(c, "stty "), c == "stty",
		strings.HasPrefix(c, "if [ -n \"$BASH_VERSION\""):
		return 0

	case strings.HasPrefix(c, "command -v"):
		// 存在性探测：内建白名单里的算存在，刻意缺席的第三方返回 1。
		bin := extractCommandVArg(c)
		if bin == "" || bin == "nosuch-aishell-bin-xyz" {
			return 1
		}
		return 0

	case strings.HasPrefix(c, "echo "):
		fmt.Fprintln(ch, strings.TrimPrefix(c, "echo "))

	case c == "pwd" || strings.HasPrefix(c, "pwd "):
		fmt.Fprintln(ch, "/home/test")

	case strings.HasPrefix(c, "mkdir ") || c == "mkdir" ||
		strings.HasPrefix(c, "touch ") || c == "touch" ||
		strings.HasPrefix(c, "rm ") || c == "rm":
		// 高危变更类：假远端一律成功，真测的是策略闸门而不是文件系统。
		return 0

	case strings.Contains(c, writeMarkerProbe):
		// sshclient.WriteFile 下发的脚本
		return runWriteScript(c, ch, fs, failBackup)

	case strings.Contains(c, "uname -a") || strings.Contains(c, "== os =="):
		fmt.Fprint(ch, "== os ==\nNAME=\"Test Linux\"\nVERSION=\"1.0\"\n"+
			"== kernel ==\nLinux testhost 6.1.0-test #1 SMP x86_64 GNU/Linux\n"+
			"== uptime ==\n 10:00:00 up 3 days,  load average: 0.10, 0.20, 0.15\n"+
			"== cpu ==\n4\n"+
			"== memory ==\n              total  used  free\nMem:           7.8Gi 2.1Gi 5.7Gi\n"+
			"== disk ==\nFilesystem  Type  Size Used Avail Use%\n/dev/sda1   ext4   40G  12G   28G  30% /\n"+
			"== failed units ==\n0 loaded units listed.\n"+
			"== top mem procs ==\n  PID COMMAND  %CPU %MEM\n  1   systemd   0.1  0.5\n"+
			"== listening ==\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n")

	case strings.HasPrefix(c, "cat "):
		path := strings.Trim(strings.TrimSpace(strings.TrimPrefix(c, "cat ")), "'\"")
		content, ok := fs.read(path)
		if !ok {
			fmt.Fprintf(ch.Stderr(), "cat: %s: No such file or directory\n", path)
			return 1
		}
		fmt.Fprint(ch, content)

	// sshclient.ReadFile 实际用的是 head -c <n> -- <path>
	case strings.HasPrefix(c, "head -c "):
		rest := strings.TrimPrefix(c, "head -c ")
		parts := strings.SplitN(rest, "--", 2)
		if len(parts) != 2 {
			fmt.Fprint(ch.Stderr(), "head: invalid arguments\n")
			return 1
		}
		var limit int
		_, _ = fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &limit)
		path := strings.Trim(strings.TrimSpace(parts[1]), "'\"")
		content, ok := fs.read(path)
		if !ok {
			fmt.Fprintf(ch.Stderr(), "head: cannot open '%s' for reading: No such file or directory\n", path)
			return 1
		}
		if limit > 0 && len(content) > limit {
			content = content[:limit]
		}
		fmt.Fprint(ch, content)

	case c == "fail":
		fmt.Fprint(ch.Stderr(), "boom: something went wrong\n")
		return 3

	// 读光 stdin 再退出：用于验证客户端在 Exec 时会主动发 stdin EOF。
	// 若客户端不发，这条命令会一直挂着等输入，直到超时被杀。
	case c == "hungry":
		body, _ := io.ReadAll(ch)
		fmt.Fprintf(ch, "read %d bytes\n", len(body))

	// 复刻真实主机上「需要交互式终端」的失败形态（procps 的 top 在无 PTY 时）。
	// 有了它才能端到端验证 TTYHint 真的被接进了执行结果。
	case strings.HasPrefix(c, "top"):
		fmt.Fprint(ch.Stderr(), "TERM environment variable not set.\n")
		return 1

	// 模拟一个全屏 TUI（top/vim 那类）：切备用屏幕、画一帧、等键盘。
	// 读到 q 或通道关闭就退出 —— 备用屏幕释放触发定格的链路靠它验证。
	case strings.HasPrefix(c, "tuitop"):
		fmt.Fprint(ch, "\x1b[?1049htop - fake\r\nload: 1.2\r\n")
		buf := make([]byte, 64)
		for {
			n, err := ch.Read(buf)
			if err != nil {
				return 0
			}
			if strings.Contains(string(buf[:n]), "q") {
				fmt.Fprint(ch, "\x1b[?1049l")
				return 0
			}
		}

	// 不进备用屏幕的全屏重绘 TUI（procps top 的同种）：CUP+ED 重绘三帧
	// 就退出。top 不发 ?1049h，客户端只能靠「命令期间全屏重绘过」
	// 识别它 —— 这个命令就是那条识别链路的端到端载体。
	case strings.HasPrefix(c, "tuispin"):
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(ch, "\x1b[H\x1b[2Jspin %d\r\nload: 0.%d\r\n", i, i)
		}
		return 0

	// 大量输出，用来验证客户端的捕获上限，以及「超限后仍继续排空」这个关键行为。
	// 若客户端在达到上限后停止读取，这里的 Write 会阻塞，命令永远不结束 ——
	// 测试会以超时失败，正好把那个回归钉住。
	case strings.HasPrefix(c, "bigout "):
		writeBulk(ch, strings.TrimPrefix(c, "bigout "), false)

	case strings.HasPrefix(c, "bigerr "):
		writeBulk(ch, strings.TrimPrefix(c, "bigerr "), true)

	// sudo 在无终端时的另一种措辞，用于验证签名表覆盖多种程序
	case strings.HasPrefix(c, "sudo "):
		fmt.Fprint(ch.Stderr(), "sudo: a terminal is required to read the password; "+
			"either use the -S option to read from standard input or configure an askpass helper\n")
		return 1

	case strings.HasPrefix(c, "sleep "):
		var n int
		_, _ = fmt.Sscanf(c, "sleep %d", &n)
		time.Sleep(time.Duration(n) * time.Second)

	case strings.HasPrefix(c, "ls"):
		fmt.Fprint(ch, "total 8\ndrwxr-xr-x 2 root root 4096 Sep 27 10:00 .\n-rw-r--r-- 1 root root  120 Sep 27 10:00 app.conf\n")

	default:
		fmt.Fprintf(ch.Stderr(), "test-sshd: unknown command: %s\n", c)
		return 127
	}
	return 0
}

func stripCdPrefix(c string) string {
	for {
		// cd ~ && …  /  cd '/path' && …  /  cd "/path" && …
		if !strings.HasPrefix(c, "cd ") {
			return c
		}
		rest := c[3:]
		var after string
		switch {
		case strings.HasPrefix(rest, "'"):
			end := strings.Index(rest[1:], "'")
			if end < 0 {
				return c
			}
			after = strings.TrimSpace(rest[1+end+1:])
		case strings.HasPrefix(rest, "\""):
			end := strings.Index(rest[1:], "\"")
			if end < 0 {
				return c
			}
			after = strings.TrimSpace(rest[1+end+1:])
		default:
			and := strings.Index(rest, "&&")
			if and < 0 {
				return c
			}
			after = strings.TrimSpace(rest[and:])
		}
		if !strings.HasPrefix(after, "&&") {
			return c
		}
		c = strings.TrimSpace(strings.TrimPrefix(after, "&&"))
		// 纯 cd 跟踪哨兵：cd X && printf ... 不再剥
		if strings.HasPrefix(c, "printf ") {
			return c
		}
	}
}

func extractCommandVArg(c string) string {
	// command -v -- 'bin' >/dev/null 2>&1
	rest := strings.TrimSpace(strings.TrimPrefix(c, "command -v"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "--"))
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	if rest[0] == '\'' || rest[0] == '"' {
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
