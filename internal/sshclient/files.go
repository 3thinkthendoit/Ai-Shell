package sshclient

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// 这个文件是文件管理弹窗的底层：列目录、删路径、二进制读写。
//
// 为什么全部走 shell 命令而不是 SFTP：本项目连一个 SFTP 依赖都没有，
// 而所有既有的远端文件能力（ReadFile/WriteFile）本来就是在 Exec 上拼命令。
// 引入 SFTP 意味着多一条独立的长连接、一套独立的认证路径，以及一个新的
// 依赖 —— 而后端已经在为「读一个文件」开 SSH 会话了，为同一件事再养一条
// 协议通道不划算。代价是必须自己处理命令注入与输出解析，这两件事
// 下面每个函数都单独交代了做法。

// FileEntry 是目录里的一项。
type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // 绝对路径，含父目录
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`    // 权限串，如 "-rw-r--r--"
	ModTime int64  `json:"modTime"` // Unix 秒；0 表示没解析出来
	// IsLink 表示这是个符号链接。单独标出来是因为 ls 的大小/权限说的是
	// 链接本身而不是它指向的目标 —— 用户看到「大小 12」的目录会以为文件损坏。
	IsLink bool `json:"isLink"`
}

// ListDirEntries 列出远端目录内容（已解析成结构化条目）。
//
// 不用 `ls -la` 的列位置解析：字段之间是若干空格，而**文件名本身可以含空格**，
// 一旦名字里有空格，按列切分就会把它切成两半、把后半截当成新的一列。
// 这里用 `ls -lan`（数字 uid/gid，避免用户名里的空格，也省掉 NSS 查询）
// 加 `--time-style=+%s`（时间戳格式固定、无空格、无需解析月份名），
// 然后**只按前 8 个字段切分**，剩下的整段都是文件名 —— 文件名是最后一个
// 字段，从第 9 个字符起原样取即可，空格不再有歧义。
//
// hidden 控制是否带 -a：默认列出隐藏文件（与 ls -la 语义一致），
// 前端负责过滤 `.` 与 `..`。
func (c *Client) ListDirEntries(hostID, path string) ([]FileEntry, Result, error) {
	q := shellQuote(path)
	// --color=never 必须有：默认带颜色时输出里会混入 ANSI 转义，
	// 会把文件名的开头几个字符吃掉，表现为「第一个文件的名字莫名少一段」。
	cmd := "LC_ALL=C ls -lan --color=never --time-style=+%s -- " + q
	res, err := c.Exec(hostID, cmd, 30*time.Second)
	if err != nil {
		return nil, res, err
	}
	if res.ExitCode != 0 {
		return nil, res, fmt.Errorf("列目录失败：%s", firstNonEmpty(res.Stderr, "退出码 "+strconv.Itoa(res.ExitCode)))
	}
	return parseLsLong(res.Stdout, path), res, nil
}

// parseLsLong 解析 `ls -lan --time-style=+%s` 的输出。
//
// 每行是**6 个元信息字段 + 文件名**：
//
//	drwxr-xr-x 3 0 0 4096 1735689600 dir-name
//	-rw-r--r-- 1 0 0  220 1735689600 file.txt
//	lrwxrwxrwx 1 0 0    7 1735689600 link -> /etc/hosts
//	│          │ │ │ │    │          └── 文件名（可能是最后一段，也可能整段含空格）
//	│          │ │ │ │    └── 修改时间（Unix 秒）
//	│          │ │ │ └── 大小
//	│          │ └ └── owner / group（-n 保证是数字，不会有空格）
//	│          └── 硬链接数
//	└── 权限串（首位是 d/l/-）
//
// 之所以强调这个数量：--time-style=+%s 把原本「月份 日 时间」三段压缩成
// 一段，所以**总字段数比常见的 ls -l 输出少**。按 8 段去切会把文件名当成
// 字段吃掉、或因为凑不满 8 段而丢弃整行 —— 后者表现为「列表是空的，
// 但远端 ls 明明有输出」。
//
// **也不能按单个空格 SplitN**：ls 的列宽右对齐，数字列前面会补空格
// （上面第 2 行的 ` 220`）。按单空格切会切出空字段，让「第 5 段是大小」
// 整体错位。所以手动跳过 6 个以连续空白分隔的字段，剩下整段当文件名。
//
// 文件名原样保留（含空格）；`-> 目标` 留在名字里并在 IsLink 上标记 ——
// 比悄悄丢掉更有用，用户至少知道这是个链接以及它指向哪里。
func parseLsLong(out, dirPath string) []FileEntry {
	entries := make([]FileEntry, 0, 32)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// total 行（某些实现会打印）直接跳过。
		if strings.HasPrefix(line, "total ") {
			continue
		}
		fields, name, ok := splitLsLine(line)
		if !ok {
			continue
		}
		mode := fields[0]
		// 第一列必须是权限串（10 字符，形如 drwxr-xr-x）。不是的话说明
		// 这一行不是 ls 的条目（比如远端把 warning 混进了 stdout），跳过。
		if len(mode) < 10 {
			continue
		}
		entry := FileEntry{
			Mode:   mode,
			IsDir:  mode[0] == 'd',
			IsLink: mode[0] == 'l',
		}
		// size 在第 5 列，mtime 在第 6 列。
		if n, err := strconv.ParseInt(fields[4], 10, 64); err == nil {
			entry.Size = n
		}
		if ts, err := strconv.ParseInt(fields[5], 10, 64); err == nil {
			entry.ModTime = ts
		}
		// `.` 与 `..` 不在弹窗里展示（导航靠「上一级」按钮），
		// 但保留在底层解析里会让上层的过滤逻辑无处安放 —— 这里直接滤掉。
		if name == "." || name == ".." || name == "" {
			continue
		}
		entry.Name = name
		entry.Path = joinRemotePath(dirPath, name)
		entries = append(entries, entry)
	}
	return entries
}

// lsMetaFields 是 ls 长格式里文件名之前的字段数：
// 权限、链接数、owner、group、大小、时间。第 7 段起是文件名。
//
// （--time-style=+%s 把常见的「月份 日 时刻」压成一段，所以这里是 6 而不是 8。）
const lsMetaFields = 6

// splitLsLine 把一行 ls 长格式切成「前 6 个元信息字段」与「剩下的文件名」。
//
// 这 6 段以任意长度的空白分隔（ls 用空格右对齐数字列），它们之后的所有内容
// —— 连同其中的空格 —— 都是文件名。这正是这套解析能同时正确处理
// 「文件名含空格」与「数字列有前导空格」的原因。
func splitLsLine(line string) (fields []string, name string, ok bool) {
	fields = make([]string, 0, lsMetaFields)
	i := 0
	for len(fields) < lsMetaFields {
		// 跳过当前的分隔空白
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			// 还没凑够元信息字段就到行尾了：不是一条完整的 ls 条目。
			return nil, "", false
		}
		start := i
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		fields = append(fields, line[start:i])
	}
	// 元信息之后：跳掉分隔空白，剩下的整段就是文件名。
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	name = line[i:]
	return fields, name, true
}

// joinRemotePath 把目录与名字拼成绝对路径，避免出现 `//` 或漏掉分隔符。
func joinRemotePath(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// DeletePath 删除远端的一个文件或目录。
//
// 用 `rm -rf`：弹窗的删除按钮已经把「确认」交给了用户（前端弹窗），
// 这里再用 `-i` 之类只会让远端在无 TTY 的连接上卡住读 stdin ——
// 表现是「点了删除一直转圈，最后超时」。
//
// 路径用单引号包裹：文件名里带空格、分号、$ 都是合法的，裸拼会变成
// 命令注入（一个叫 `a; rm -rf ~` 的文件名足以毁掉整个主目录）。
func (c *Client) DeletePath(hostID, path string) (Result, error) {
	if strings.TrimSpace(path) == "" || path == "/" {
		// 底线：不接受空路径或根目录。前端已经拦了一道，后端再拦一道 ——
		// 这条一旦漏了，误删的是整台机器。
		return Result{}, fmt.Errorf("拒绝删除空路径或根目录")
	}
	return c.Exec(hostID, "rm -rf -- "+shellQuote(path), 60*time.Second)
}

// ReadFileBinary 读取远端文件的原始字节。
//
// 与 ReadFile 的区别：那个走 `head -c` 把内容当**文本**塞进 stdout，
// 中途会经过 UTF-8 解码（无效字节变成替换字符），二进制文件（图片、
// 压缩包、可执行文件）会被破坏。这里让远端先 base64 编码再传 ——
// base64 字符集全是可打印 ASCII，任何一层都不会动它。
//
// maxBytes 是**解码后**的大小上限。base64 会膨胀约 33%，所以远端截断
// 位置要按编码后的长度算。
//
// 返回值里的 total 是远端的**真实文件大小**（来自 `wc -c`），mayTruncate
// 表示「文件可能已被截到 maxBytes」。
//
// 为什么要专门回报大小，而不是「解码后长度恰好等于 maxBytes 就算截断」：
// **文件正好等于上限时**那个判据会误报，而用户手上真的有一个 32 MiB 的文件
// 并不罕见。多跑一条 wc -c 就能把「恰好这么大」与「被截断了」分开 ——
// 这比让用户拿到一个静默截断的文件要好得多（见 app_files 里的处理）。
func (c *Client) ReadFileBinary(hostID, path string, maxBytes int64) (data []byte, mayTruncate bool, total int64, res Result, err error) {
	if maxBytes <= 0 {
		maxBytes = defaultTransferLimit
	}
	// 按 base64 长度截断，保证解码后不超过 maxBytes。
	encoded := base64.StdEncoding.EncodedLen(int(maxBytes))
	q := shellQuote(path)
	// 一条命令里同时取「真实大小」与「内容」：
	//   wc -c 走 stderr？不行 —— 这里用两条输出、以标记行分隔，避免解析歧义。
	// 先打 SIZE 行再打内容，标记用不可能出现在 base64 字符集里的字符（'#'）。
	cmd := fmt.Sprintf("sz=$(wc -c < %s) || exit 1; printf 'SIZE:%%s\\n' \"$sz\"; head -c %d -- %s | base64 -w0",
		q, encoded, q)
	res, err = c.Exec(hostID, cmd, 120*time.Second)
	if err != nil {
		return nil, false, 0, res, err
	}
	if res.ExitCode != 0 {
		return nil, false, 0, res, fmt.Errorf("读取文件失败：%s", firstNonEmpty(res.Stderr, "退出码 "+strconv.Itoa(res.ExitCode)))
	}

	// 切开 SIZE 行与 base64 正文。
	raw := strings.TrimSpace(res.Stdout)
	var body string
	if i := strings.IndexByte(raw, '\n'); i >= 0 && strings.HasPrefix(raw, "SIZE:") {
		if n, perr := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(raw[:i], "SIZE:")), 10, 64); perr == nil {
			total = n
		}
		body = raw[i+1:]
	} else {
		// 远端没按预期回 SIZE 行（shell 不支持 $() 等）：退回只看正文。
		// 此时 total 未知（0），由调用方按长度判断是否可能截断。
		body = raw
	}

	body = strings.TrimSpace(body)
	data, decErr := base64.StdEncoding.DecodeString(body)
	if decErr != nil {
		// -w0 缺失或远端 base64 实现不同会让输出带换行；去掉空白再试一次。
		compact := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, body)
		data, decErr = base64.StdEncoding.DecodeString(compact)
		if decErr != nil {
			return nil, false, 0, res, fmt.Errorf("远端返回的内容不是合法 base64（该机器的 base64 实现可能不支持 -w0）：%w", decErr)
		}
	}

	// 两种判据取其一：知道真实大小就比大小；不知道就看长度是否顶着上限。
	if total > 0 {
		mayTruncate = total > int64(len(data))
	} else {
		mayTruncate = int64(len(data)) >= maxBytes
	}
	return data, mayTruncate, total, res, nil
}

// WriteFileBinary 把原始字节写到远端文件（覆写，带备份）。
//
// 与 WriteFile 的区别是**编码方式**：那个直接把内容当字符串经 stdin 送，
// 二进制里的 \x00 与无效 UTF-8 会在 SSH 通道或 Go 的 string 转换里损坏。
// 这里把内容 base64 编码后送过去，由远端 `base64 -d` 解码写盘。
//
// 送数据的方式是**另开一条会话、把命令连同数据一起写进 stdin** ——
// 因为 Exec 会立刻关掉 stdin（见 Exec 的注释），无法用来喂数据。
func (c *Client) WriteFileBinary(hostID, path string, data []byte) (Result, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	script := writeBinaryScript(path)
	return c.runWithStdin(hostID, script, encoded, 300*time.Second)
}

// defaultTransferLimit 是单次上传/下载的大小上限（32 MiB）。
//
// 定这个数的理由：整份内容要作为**一个 base64 字符串**在内存里存在三份
// （文件本体、编码串、Wails 的 IPC 序列化），32 MiB 的文件对应约 130 MiB
// 峰值占用。再往上就该走 SFTP 或分块，而不是把这份简单实现撑爆。
const defaultTransferLimit = 32 << 20

// MaxTransferBytes 是上传/下载共用的上限（变量，便于测试压小）。
var MaxTransferBytes int64 = defaultTransferLimit

// writeBinaryScript 构造「解码 stdin 写入文件」的远端脚本。
//
// 与 writeFileScript 的差别只有最后一环：从 `cat > "$p"` 换成
// `base64 -d > "$p"`。备份逻辑（含失败必须可见、$-$$ 防同秒撞名、
// 区分「新文件」与「备份失败」）原样保留 —— 那些坑是在文本写入里
// 踩出来的，二进制写入一个都不少。
func writeBinaryScript(path string) string {
	q := shellQuote(path)
	return fmt.Sprintf(
		`p=%s; bak="$p.bak.$(date +%%Y%%m%%d-%%H%%M%%S).$$"; `+
			`if [ -e "$p" ]; then `+
			`cp -a -- "$p" "$bak" || { echo %s; exit 90; }; `+
			`echo "%s$bak"; `+
			`else echo "%s%s"; fi; `+
			`base64 -d > "$p"`,
		q, backupFailedMarker, backupOKPrefix, backupOKPrefix, backupNewFileNote)
}

// runWithStdin 用一条新会话执行 script，并把 stdin 内容喂给它。
// 逻辑与 WriteFile 相同（超时保护 + 装饰备份信息），抽出来供二进制写入复用。
func (c *Client) runWithStdin(hostID, script, stdinContent string, timeout time.Duration) (Result, error) {
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

	sess.Stdin = strings.NewReader(stdinContent)
	var stdout, stderr cappedBuffer
	stdout.limit = MaxCaptureBytes
	stderr.limit = MaxCaptureBytes
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	type runResult struct{ err error }
	done := make(chan runResult, 1)
	start := time.Now()
	go func() { done <- runResult{sess.Run(script)} }()

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
	case <-time.After(timeout):
		_ = sess.Close()
		<-done
		c.Disconnect(hostID)
		return Result{}, fmt.Errorf("写入文件超时（%s）已被终止", timeout)
	}
	return Result{
		Stdout:     decorateBackup(stdout.String()),
		Stderr:     stderr.String(),
		ExitCode:   0,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}

// HomeDir 返回远端的家目录（文件管理弹窗在拿不到 OSC 7 时的回退位置）。
func (c *Client) HomeDir(hostID string) (string, error) {
	res, err := c.Exec(hostID, "pwd -P", 15*time.Second)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(res.Stdout)
	if home == "" {
		return "", fmt.Errorf("无法确定远端家目录")
	}
	return home, nil
}
