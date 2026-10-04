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
	"encoding/base64"
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

	// dlHeadNoise / dlTailNoise 是**二进制下载时注入到 stdout 的杂音**。
	//
	// 存在的理由：真实远端并不是一台只回答我们要的东西的机器。
	// 登录 shell 的 /etc/profile 会打欢迎语、busybox 的 base64 会把用法
	// 提示打到 stdout、缺子命令会报 `bc: command not found`……
	// 这些都会混进下载输出里。
	//
	// 这正是「远端返回的内容不是合法 base64：illegal base64 data at input
	// byte 4」那条 bug 的成因：内容本身是对的，只是前后多了别的东西。
	// 没有这个开关，那个 bug 在测试里永远复现不出来（假远端太干净了）。
	dlHeadNoise atomic.Value // []string
	dlTailNoise atomic.Value // []string

	// corruptDownloadBody 为真时，跳过 base64 编码、把文件内容原样写进
	// 哨兵之间 —— 模拟"远端根本没编码"或"内容本身就是二进制垃圾"。
	// 用于验证错误信息是否带上可读的预览。
	corruptDownloadBody atomic.Bool

	// downloadMissingEnd 为真时，下载分支不打印 END 哨兵 —— 模拟真实
	// 机器上出现过的怪癖：base64 载荷完整，END 却始终没出现（连接抖动、
	// shell 提前收工）。客户端应当靠「长度严格等于请求值」抢救成功，
	// 而不是为一个丢失的标记把完好的数据扔掉。
	downloadMissingEnd atomic.Bool

	// downloadShortBody 为真时，只发送前一半 base64（裁到 4 字符的倍数，
	// 仍是合法可解码的 base64）—— 模拟载荷在半路被截断。配合上面的
	// missingEnd 验证：抢救路径必须拒绝长度不符的数据，绝不悄悄收下
	// 残缺的块。
	downloadShortBody atomic.Bool

	ptyMu   sync.Mutex
	lastPTY *PTYRequest
	resizes []Size
}

// SetCorruptDownloadBody 开关「下载正文不编码」的模拟。
func (s *Server) SetCorruptDownloadBody(v bool) { s.corruptDownloadBody.Store(v) }

// SetDownloadMissingEnd 开关「END 哨兵丢失」的模拟。
func (s *Server) SetDownloadMissingEnd(v bool) { s.downloadMissingEnd.Store(v) }

// SetDownloadShortBody 开关「载荷被截断一半」的模拟。
func (s *Server) SetDownloadShortBody(v bool) { s.downloadShortBody.Store(v) }

// SetDownloadNoise 设置下载输出里注入的杂音（head = 正文之前，tail = 正文之后）。
// 传 nil 清空。测试用它来复现「远端 stdout 不干净」的各种真实情况。
func (s *Server) SetDownloadNoise(head, tail []string) {
	s.dlHeadNoise.Store(head)
	s.dlTailNoise.Store(tail)
}

func (s *Server) downloadNoise() []string     { return noiseOf(s.dlHeadNoise) }
func (s *Server) downloadTailNoise() []string { return noiseOf(s.dlTailNoise) }

func noiseOf(v atomic.Value) []string {
	if lines, ok := v.Load().([]string); ok {
		return lines
	}
	return nil
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
	// Modes 是 pty-req 载荷里的 POSIX 终端模式（opcode → 值，TTY_OP_END 截止）。
	// 客户端靠初始 ECHO=0 做到 shell integration 零回显注入（见 sshclient.OpenPTY
	// 与 app.injectShellIntegration），这条约定必须能从报文层面钉住。
	Modes map[uint8]uint32
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
	rows, rest, ok := readUint32(rest)
	if !ok {
		return PTYRequest{}, false
	}
	// 宽度（像素）、高度（像素）：本服务端不关心，但必须消费掉才能读到模式
	if _, rest, ok = readUint32(rest); !ok {
		return PTYRequest{}, false
	}
	if _, rest, ok = readUint32(rest); !ok {
		return PTYRequest{}, false
	}
	// 终端模式：重复的 (opcode byte, uint32 值)，以 opcode 0（TTY_OP_END）结束
	modes := map[uint8]uint32{}
	for len(rest) >= 5 {
		op := rest[0]
		if op == 0 {
			break
		}
		v, r, ok := readUint32(rest[1:])
		if !ok {
			break
		}
		modes[op] = v
		rest = r
	}
	return PTYRequest{Term: term, Cols: int(cols), Rows: int(rows), Modes: modes}, true
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

// snapshot 返回一份文件表的副本。
//
// 返回副本而不是直接交出 map：调用方（fakeLsLong）要遍历它，
// 而遍历期间若另一个 goroutine 在写（上传/删除），直接读会触发
// 并发 map 读写崩溃 —— 那种崩溃只在并发测试下偶发，极难定位。
func (f *memFS) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.files))
	for k, v := range f.files {
		out[k] = v
	}
	return out
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

// 二进制下载正文与大小探测的哨兵，必须与 sshclient（readChunkCmd /
// statSizeCmd）里的一致。刻意重复定义：对方改了协议，相关测试会立刻
// 失败 —— 这是想要的信号。（曾用 ###...###：# 在 shell 里开启注释，
// 真实远端把整条管道吞掉了，而这里的假远端不执行命令、只会照着常量发
// 输出，根本发现不了。客户端已换成 @@...@@ 并加引号，命令文本另有
// 语法单测守着。）
const (
	downloadBodyMark = "@@B64-BEGIN@@"
	downloadBodyEnd  = "@@B64-END@@"
	sizeBodyMark     = "@@SZ-BEGIN@@"
	sizeBodyEnd      = "@@SZ-END@@"
)

// wrapBase64 模拟 busybox base64 的默认行为：每 76 字符插一个换行
// （它不认 -w0，所以客户端必须自己剥空白）。
func wrapBase64(s string) string {
	const width = 76
	var sb strings.Builder
	for len(s) > width {
		sb.WriteString(s[:width])
		sb.WriteByte('\n')
		s = s[width:]
	}
	sb.WriteString(s)
	return sb.String()
}

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

// runBinaryWriteScript 模拟 sshclient.WriteFileBinary 的远端脚本。
//
// 与 runWriteScript 的**唯一**差别在收尾：这里要先把 stdin 的内容 base64
// 解码再落盘（脚本最后是 `base64 -d > "$p"`）。备份逻辑完全一致，
// 所以这里同样先跑一遍备份分支，保证「上传覆写会留备份」这条也被验证到。
func runBinaryWriteScript(cmd string, ch ssh.Channel, fs *memFS, failBackup bool) int {
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

	// 脚本最后是 `base64 -d > "$p"`：stdin 送来的是 base64 文本。
	body, _ := io.ReadAll(ch)
	flat := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(body))
	decoded, err := base64.StdEncoding.DecodeString(flat)
	if err != nil {
		// 真 `base64 -d` 遇到坏输入会报错且**不覆盖**目标文件
		// （它已经把旧内容读走了，但写不出来）。这里复刻「报错且不写」，
		// 否则测出来的行为比真实环境宽松。
		fmt.Fprint(ch.Stderr(), "base64: invalid input\n")
		return 1
	}
	fs.write(p, string(decoded))
	return 0
}

// extractRmTarget 从 `rm -rf -- '<path>'` 里取出路径。
//
// 按 `--` 切分而不是取最后一个 token：路径里的空格被 shellQuote 保护在
// 单引号内，按空格切会把带空格的文件名切碎 —— 而那正是要测的边界之一。
func extractRmTarget(cmd string) string {
	i := strings.Index(cmd, "--")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(cmd[i+2:])
	return unquoteShellArg(rest)
}

// extractRedirectTarget 从 `… > '<path>'` 里取出重定向目标。
func extractRedirectTarget(cmd string) string {
	i := strings.LastIndex(cmd, ">")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(cmd[i+1:])
	// 跳过 `>` 之后可能紧跟的 `>`（追加重定向，这里用不到但要防误判）。
	rest = strings.TrimSpace(strings.TrimPrefix(rest, ">"))
	return unquoteShellArg(rest)
}

// unquoteShellArg 去掉 shellQuote 加上的单引号包裹（含内层 '\” 的还原）。
//
// 与 sshclient.shellQuote 是同一个协议的镜像实现，刻意不跨包共享：
// 一旦对方改了引号策略，这里的解析会失配、相关用例立刻失败 —— 这正是想要的信号。
func unquoteShellArg(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `'\''`, "'")
	}
	return s
}

// extractLsDir 从 `ls -lan --color=never --time-style=+%s -- <dir>` 里取出目录。
//
// 必须找**最后**一个独立的 ` -- `，不能找第一个 "--"：命令里的
// `--color=never` 和 `--time-style=...` 都以 `--` 开头，取第一个会把
// "color=never --time-style=+%s -- '/srv'" 整段当成目录名，
// 于是列表永远是空的 —— 而 ls 明明是成功的，很能误导人。
func extractLsDir(cmd string) string {
	i := strings.LastIndex(cmd, " -- ")
	if i < 0 {
		return "."
	}
	dir := unquoteShellArg(strings.TrimSpace(cmd[i+4:]))
	if dir == "" {
		return "."
	}
	return dir
}

// extractDownloadPath 从下载命令里取出文件路径。
//
// 命令形状：`sz=$(wc -c < '/p') || exit 1; printf 'SIZE:%s\n' "$sz";
// printf '@@B64-BEGIN@@'; head -c N -- '/p' | base64 || exit $?;
// printf '@@B64-END@@\n'`
//
// 取**最后一个** `-- ` 之后的单引号串：路径本身可能含 `--`，按第一个找会切错。
// 用 wc 那个 `< '/p'` 里的路径也行，但 head 那段才是真正读内容的一条，
// 以它为准更能反映「客户端读的是哪个文件」。
func extractDownloadPath(cmd string) string {
	i := strings.LastIndex(cmd, " -- ")
	if i < 0 {
		return ""
	}
	rest := cmd[i+4:]
	if j := strings.Index(rest, "|"); j >= 0 {
		rest = rest[:j]
	}
	return strings.Trim(strings.TrimSpace(rest), "'\"")
}

// extractWcPath 从大小探针命令里取出路径：
// `printf '@@SZ-BEGIN@@'; wc -c < '/p'; printf '@@SZ-END@@\n'`
func extractWcPath(cmd string) string {
	i := strings.Index(cmd, "wc -c < ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(cmd[i+len("wc -c < "):])
	if j := strings.Index(rest, ";"); j >= 0 {
		rest = rest[:j]
	}
	return strings.Trim(strings.TrimSpace(rest), "'\"")
}

// extractChunkRange 从分块读命令里取出区间：
// `tail -c +<off1> -- 'p' | head -c <size> | base64`。
// off1 是 tail 的 **1 起**偏移，与命令字面一致；调用方换算成 0 起。
func extractChunkRange(cmd string) (off1, size int64, ok bool) {
	i := strings.Index(cmd, "tail -c +")
	if i < 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(cmd[i+len("tail -c +"):], "%d", &off1); err != nil {
		return 0, 0, false
	}
	j := strings.Index(cmd, "head -c ")
	if j < 0 {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(cmd[j+len("head -c "):], "%d", &size); err != nil {
		return 0, 0, false
	}
	return off1, size, true
}

// fakeLsLong 生成 `ls -lan --time-style=+%s -- <dir>` 的输出。
//
// 输出的**形状**必须与真实 ls 一致：6 个元信息字段（权限/链接数/uid/gid/
// 大小/时间戳）之后是文件名，且文件名可能含空格。解析器就是按这个形状写的，
// 假远端若图省事只打印名字，那条解析路径就完全没有被测到。
//
// 内容取自 memFS 的键：这样「上传后刷新列表能看到新文件」「删除后它消失」
// 这两条端到端链路才成立，而不是永远返回一份写死的清单。
func fakeLsLong(cmd string, fs *memFS) string {
	dir := extractLsDir(cmd)
	var sb strings.Builder
	sb.WriteString("total 8\n")
	// 假设「一直在那里」的两项，让目录列表不至于空着（与老用例的输出一致）。
	sb.WriteString("drwxr-xr-x 2 0 0 4096 1758880800 subdir\n")
	sb.WriteString("-rw-r--r-- 1 0 0  120 1758880800 app.conf\n")
	if fs == nil {
		return sb.String()
	}
	// 把 memFS 里属于这个目录的普通文件也列出来（按名字排序，稳定）。
	prefix := dir
	if prefix == "." {
		prefix = ""
	} else if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var names []string
	for p := range fs.snapshot() {
		if prefix == "" {
			// 无目录上下文时只列顶层（不含 / 的名字），避免把全库倒出来。
			if !strings.Contains(p, "/") {
				names = append(names, p)
			}
			continue
		}
		if strings.HasPrefix(p, prefix) {
			rest := strings.TrimPrefix(p, prefix)
			if rest != "" && !strings.Contains(rest, "/") {
				names = append(names, p)
			}
		}
	}
	sort.Strings(names)
	for _, p := range names {
		content, _ := fs.read(p)
		base := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			base = p[i+1:]
		}
		fmt.Fprintf(&sb, "-rw-r--r-- 1 0 0 %d 1758880800 %s\n", len(content), base)
	}
	return sb.String()
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
		strings.HasPrefix(c, "touch ") || c == "touch":
		// 高危变更类：假远端一律成功，真测的是策略闸门而不是文件系统。
		return 0

	// rm 必须**真的删**：文件管理弹窗的删除要能端到端验证
	// 「点了删除之后那个文件确实不见了」，只报成功而没删会把
	// 「删错了目标/删了空路径」这类实现错误盖过去。
	// 带 -rf 时不校验路径存在性（与真 rm -f 一致），也不递归删子项 ——
	// 测试里删的都是扁平的顶层文件，多写一套递归只是自找麻烦。
	case strings.HasPrefix(c, "rm ") || c == "rm":
		target := extractRmTarget(c)
		if target == "" {
			fmt.Fprint(ch.Stderr(), "rm: missing operand\n")
			return 1
		}
		fs.remove(target)
		return 0

	case strings.Contains(c, writeMarkerProbe):
		// sshclient.WriteFile / WriteFileBinary 下发的脚本。
		// 两者只差最后一环：文本写入是 `cat > "$p"`，二进制是 `base64 -d > "$p"`。
		// 假远端是命令解释器而不是真 shell，没法逐句执行这个多语句脚本，
		// 只能按同一个 marker 认出来，再按收尾命令分流 —— 这也正是
		// writeMarkerProbe 存在的意义（见它的常量注释）。
		if strings.Contains(c, "base64 -d >") {
			return runBinaryWriteScript(c, ch, fs, failBackup)
		}
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

	// 下载大小探测：`printf '@@SZ-BEGIN@@'; wc -c < '/p'; printf '@@SZ-END@@\n'`
	// 必须排在下载分支之前（后者按 `| base64` 匹配，探针里没有它，顺序其实
	// 无关，但放在一起便于对照）。
	// 杂音包裹与下载分支同理：脏 stdout 的机器上探针同样会被污染，
	// 客户端靠哨兵取中间那个数字。
	case strings.HasPrefix(c, "printf '"+sizeBodyMark+"'"):
		p := extractWcPath(c)
		if p == "" {
			fmt.Fprint(ch.Stderr(), "wc: invalid usage\n")
			return 1
		}
		content, ok := fs.read(p)
		if !ok {
			fmt.Fprintf(ch.Stderr(), "wc: %s: No such file or directory\n", p)
			return 1
		}
		for _, line := range srv.downloadNoise() {
			fmt.Fprint(ch, line)
		}
		fmt.Fprint(ch, sizeBodyMark)
		fmt.Fprintf(ch, "%d\n", len(content))
		fmt.Fprintln(ch, sizeBodyEnd)
		for _, line := range srv.downloadTailNoise() {
			fmt.Fprint(ch, line)
		}
		return 0

	// 二进制下载（分块）：`printf '@@B64-BEGIN@@';
	//            tail -c +<off1> -- '/p' | head -c <size> | base64 || exit $?;
	//            printf '@@B64-END@@\n'`
	//
	// 客户端的命令文本本身有语法单测（internal/sshclient 对 readChunkCmd
	// 的 shell 分词检查）—— 这里只负责按协议应答，不执行命令，语法错误
	// 在这一侧是发现不了的（### 标记事故的教训）。
	//
	// 必须排在 `head -c` 文本分支**之前**：那条分支按 `--` 切分并假设右边
	// 就是路径，遇到管道尾巴会把整个 `| base64` 当成文件名的一部分，
	// 于是报告「文件不存在」——一个纯粹的匹配顺序问题，却表现得像文件没了。
	case strings.Contains(c, downloadBodyMark) || strings.Contains(c, "| base64"):
		p := extractDownloadPath(c)
		if p == "" {
			fmt.Fprint(ch.Stderr(), "tail: invalid arguments\n")
			return 1
		}
		off1, size, ok := extractChunkRange(c)
		if !ok || off1 < 1 || size <= 0 {
			fmt.Fprint(ch.Stderr(), "tail: invalid arguments\n")
			return 1
		}
		content, ok := fs.read(p)
		if !ok {
			fmt.Fprintf(ch.Stderr(), "tail: cannot open '%s' for reading: No such file or directory\n", p)
			return 1
		}
		data := []byte(content)
		start := off1 - 1
		if start > int64(len(data)) {
			start = int64(len(data))
		}
		end := start + size
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		chunk := data[start:end]
		// 模拟真实环境里各种会污染 stdout 的东西（欢迎语、用法提示……）。
		// 客户端取两个标记之间的内容，头尾杂音天然被无视。
		for _, line := range srv.downloadNoise() {
			fmt.Fprint(ch, line)
		}
		fmt.Fprint(ch, downloadBodyMark)
		if srv.corruptDownloadBody.Load() {
			// 故意不编码：正文里会是任意字节，客户端必须拒绝并给出可读原因。
			fmt.Fprint(ch, chunk)
		} else {
			// busybox 的 base64 不认 -w0，默认就换行；这里固定按换行输出，
			// 逼客户端用 stripWhitespace 去处理，而不是假设 -w0 生效。
			enc := base64.StdEncoding.EncodeToString(chunk)
			if srv.downloadShortBody.Load() {
				// 只发前一半，裁到 4 字符的倍数 —— 截断的 base64 仍是
				// 合法可解码的，这正是「残缺数据伪装完整」的最阴险形态。
				if cut := (len(enc) / 4) * 4 / 2; cut > 0 {
					enc = enc[:cut]
				}
			}
			fmt.Fprint(ch, wrapBase64(enc))
		}
		if !srv.downloadMissingEnd.Load() {
			fmt.Fprintf(ch, "%s\n", downloadBodyEnd)
		}
		// 尾巴上的污染：在 END 标记**之后**再吐一行提示。
		for _, line := range srv.downloadTailNoise() {
			fmt.Fprint(ch, line)
		}
		return 0

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

	case strings.HasPrefix(stripEnvPrefix(c), "ls"):
		// 文件管理用的是 `LC_ALL=C ls -lan --color=never --time-style=+%s -- <path>`，
		// 必须按那个格式回答（时间戳是裸 Unix 秒、uid/gid 是数字），
		// 否则解析器的「第 6 列是时间戳」假设在测试里得不到验证。
		// 开头的 LC_ALL=C 必须先剥掉：它是环境变量前缀，不是命令名的一部分，
		// 直接按 "ls" 前缀匹配会一路落到 unknown command 分支上去。
		// 旧式的 `ls`（不带这些选项）保留原输出：ReadFile/ListDir 的老用例
		// 依赖它，且那条路径本来就只做文本展示、不解析。
		if strings.Contains(c, "--time-style=+%s") {
			fmt.Fprint(ch, fakeLsLong(c, fs))
			return 0
		}
		fmt.Fprint(ch, "total 8\ndrwxr-xr-x 2 root root 4096 Sep 27 10:00 .\n-rw-r--r-- 1 root root  120 Sep 27 10:00 app.conf\n")

	default:
		fmt.Fprintf(ch.Stderr(), "test-sshd: unknown command: %s\n", c)
		return 127
	}
	return 0
}

// stripEnvPrefix 去掉命令开头的 `VAR=value ` 环境变量前缀。
//
// 真 shell 里 `LC_ALL=C ls` 执行的是 ls；假远端如果不剥这一层，
// 按 "ls" 前缀匹配就会落到 unknown command 分支 —— 表现为
// 「列目录在测试里全挂，但真机上好好的」，很容易被误判成解析器写错了。
// 只认形如 `NAME=...` 且名字全是字母数字下划线的开头，避免把
// `echo a=b` 这类正常内容误伤。
func stripEnvPrefix(c string) string {
	for {
		eq := strings.Index(c, "=")
		if eq <= 0 {
			return c
		}
		name := c[:eq]
		for i := 0; i < len(name); i++ {
			ch := name[i]
			ok := ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
			if !ok {
				return c
			}
		}
		rest := strings.TrimLeft(c[eq+1:], " ")
		// 值本身也可能带引号（LC_ALL="C"），一并剥掉。
		rest = strings.TrimPrefix(rest, "\"")
		rest = strings.TrimPrefix(rest, "'")
		sp := strings.IndexByte(rest, ' ')
		if sp < 0 {
			return c
		}
		c = strings.TrimLeft(rest[sp+1:], " ")
	}
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
