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

// b64Begin / b64End 是下载正文的哨兵。
//
// 字符用 @ 而不是 #，这是用一次真实事故换来的教训：曾用 ###B64-BEGIN###
// 且**没加引号**，# 在 shell 里开启注释 —— 远端把
// "###B64-BEGIN###; head ... ; printf ..." 整段吞成注释，真正执行的只剩
// 一个零参数 printf，报出：
//
//	printf: usage: printf [-v var] format [arguments]
//
// 整个下载就此失败（且报错完全看不出是标记的问题）。
// @ 不开启注释、不触发 glob，也不在 base64 标准字符集（A-Za-z0-9+/=）里，
// 两边都不会误认。即便如此，命令里仍然给它们套上单引号 ——
// 不依赖「这个字符碰巧无害」的运气（见 readChunkCmd）。
const (
	b64Begin = "@@B64-BEGIN@@"
	b64End   = "@@B64-END@@"
)

// ChunkSize 是分块下载每块请求的**原始字节数**（512 KiB）。
//
// 下载改成分块读（见 ReadFileChunk）之后，块大小是一对取舍：
//   - 太小 → 每块一次 SSH 往返，32 MiB 的文件会多出几百次往返，慢；
//   - 太大 → 进度条长时间不动，用户不知道是卡了还是在传。
//
// 还有一个**硬约束**：base64 会把块膨胀到 4/3（busybox 还会加约 2.7% 的
// 换行），而 Exec 的输出捕获上限是 MaxCaptureBytes（1 MiB）。1 MiB 的块
// 编码后是 1.4 MB —— 恰好被捕获上限拦腰截断，结束哨兵丢了，整块作废
// （这条是用一次真实测试失败换来的）。512 KiB 编码后约 718 KiB，
// 加上哨兵与换行仍稳稳落在上限内；32 MiB 上限对应最多 64 次往返，
// 进度条每半 MiB 动一格。调大 ChunkSize 必须先算这笔账。
const ChunkSize int64 = 512 << 10

// szBegin / szEnd 是大小探针的哨兵：脏 stdout 的机器上 `wc -c` 的输出
// 会混进欢迎语，直接解析整段 stdout 会失败 —— 取两个标记之间的那个数字。
const (
	szBegin = "@@SZ-BEGIN@@"
	szEnd   = "@@SZ-END@@"
)

// statSizeCmd 构造「查文件字节数」的命令。单独抽出是为了做 shell 语法单测。
func statSizeCmd(path string) string {
	return fmt.Sprintf("printf '%s'; wc -c < %s; printf '%s\\n'", szBegin, shellQuote(path), szEnd)
}

// readChunkCmd 构造「读文件 [offset, offset+size) 区间的 base64」命令。
//
// 与 readBinaryCmd（已删）的逐段设计一致，只列增量：
//   - `tail -c +K`：POSIX 的 K 是 **1 起**的偏移，所以调用方传 0 起
//     的 offset 时这里要 +1。差一位就是整体错位一个字节 —— 二进制内容
//     全部损坏，而且很难从表象看出原因，所以这一位必须钉死在构造处。
//   - `head -c size` 把尾部截断：tail 会一路输出到文件末尾，没有这一环
//     除了最后一块之外每块都会多读。管道退出码取**最后**一环（base64），
//     tail 在 head 关闭管道后收到 SIGPIPE 退出不影响数据正确性。
//   - 哨兵夹正文、base64 挂 `|| exit $?`、标记套引号 —— 理由同前：
//     杂音隔离、失败可见、不赌字符无害。
func readChunkCmd(path string, offset, size int64) string {
	q := shellQuote(path)
	return fmt.Sprintf(
		"printf '%s'; tail -c +%d -- %s | head -c %d | base64 || exit $?; printf '%s\\n'",
		b64Begin, offset+1, q, size, b64End)
}

// StatFileSize 查询远端文件的字节数。下载用它先探明总量：
// 超限要在弹保存对话框**之前**拒绝，分块循环要知道终点。
func (c *Client) StatFileSize(hostID, path string) (int64, error) {
	res, err := c.Exec(hostID, statSizeCmd(path), 30*time.Second)
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("%s", firstNonEmpty(strings.TrimSpace(res.Stderr), "退出码 "+strconv.Itoa(res.ExitCode)))
	}
	// 取哨兵之间的数字；没拿到哨兵（老 shell/命令被截断）就退回解析
	// 整段输出 —— 失败时错误里会带原文预览，不至于莫名其妙。
	body, ok := extractBetween(res.Stdout, szBegin, szEnd)
	if !ok {
		body = res.Stdout
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(body), 10, 64)
	if perr != nil || n < 0 {
		// stdout 混进了别的行（登录脚本欢迎语之类）时解析会失败 ——
		// 与其猜哪一行是大小，不如把原文带进错误里让人一眼看出原因。
		return 0, fmt.Errorf("无法确定文件大小（远端输出：%q）", previewForError(res.Stdout))
	}
	return n, nil
}

// ReadFileChunk 读取远端文件 [offset, offset+size) 的原始字节。
//
// 下载从「一次读整个文件」改成了分块读 —— 一次 Exec 拿不到中间进度，
// 而一个 32 MiB 的传输没有进度条，用户只能看着界面猜它死没死。
// 分块之后每块回来都能发一次进度、检查一次取消。
//
// 返回的字节数可能**小于** size：那是区间越过文件末尾的正常截断
// （最后一块）。中途拿到 0 字节则说明文件比预期短了（传输中被截断/删除），
// 调用方必须当作错误处理，绝不能当成正常结束 —— 那会得到一个
// 缺了尾巴还自称完整的文件。
func (c *Client) ReadFileChunk(hostID, path string, offset, size int64) ([]byte, error) {
	if offset < 0 || size <= 0 {
		return nil, fmt.Errorf("非法的读取区间 [%d, %d)", offset, offset+size)
	}
	res, err := c.Exec(hostID, readChunkCmd(path, offset, size), 120*time.Second)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("读取文件失败：%s", firstNonEmpty(res.Stderr, "退出码 "+strconv.Itoa(res.ExitCode)))
	}

	// 取哨兵之间的内容。ok=false 分两种，必须分开处理：
	//
	//   - 有 BEGIN 没 END：先尝试**抢救**。真实机器上出现过这种怪癖 ——
	//     base64 载荷完整、END 始终没来（连接抖动、shell 提前收工），
	//     数据一个字节都没坏，为一个丢失的标记报错是浪费用户时间。
	//     抢救的接受条件是**解码长度与请求的 size 严格相等**：截断在
	//     4 字符边界上的 base64 同样能解码成功，但长度必然小于请求值 ——
	//     这道长度闸门是防「悄悄收下残缺数据」的唯一防线，绝不能放宽成
	//     「解得动就要」。长度不符时按截断报错（多半是输出被 Exec 的
	//     捕获上限 MaxCaptureBytes 拦腰截断，要修 ChunkSize 的信号）。
	//   - 连 BEGIN 都没有：老 shell / 命令被截断 —— 兜底把整段当正文解，
	//     解码失败时错误里会带原文预览，比静默成功诚实。
	body, ok := extractBetween(res.Stdout, b64Begin, b64End)
	if !ok && strings.Contains(res.Stdout, b64Begin) {
		salvage := res.Stdout[strings.Index(res.Stdout, b64Begin)+len(b64Begin):]
		if data, serr := decodeB64Body(salvage); serr == nil && int64(len(data)) == size {
			return data, nil
		}
		return nil, fmt.Errorf(
			"远端输出不完整（缺少结束标记）：本块应返回 %d 字节，实际数据不足或无法解码",
			size)
	}
	if !ok {
		body = res.Stdout
	}
	return decodeB64Body(body)
}

// decodeB64Body 把远端回的 base64 文本解成字节。
//
// 无条件剥掉所有空白：远端若是 busybox base64（不认 -w0），输出会带
// 换行；这里的剥离是幂等的，先剥再解，比"解失败再剥再解"少一次无用功。
// 还失败就考虑 URL-safe 变体（少数精简系统的 base64 会用 -_ 而不是 +/）。
func decodeB64Body(body string) ([]byte, error) {
	body = stripWhitespace(body)
	data, decErr := base64.StdEncoding.DecodeString(body)
	if decErr != nil {
		if alt, aerr := base64.RawURLEncoding.DecodeString(strings.TrimRight(body, "=")); aerr == nil {
			return alt, nil
		}
		// 把「远端到底回了什么」带进错误里。只报 input byte 4 等于没说：
		// 用户看到的是"数据坏了"，而我们其实一眼就能看出是尾巴上多了提示。
		return nil, fmt.Errorf(
			"远端返回的内容不是合法 base64：%w（收到 %d 字节，开头为 %q）",
			decErr, len(body), previewForError(body))
	}
	return data, nil
}

// extractBetween 取 begin 与 end 之间的内容。
//
// 返回的 ok 区分两种「内容为空」：
//   - ok=true  且 body 为空 —— 哨兵都在、正文为空，即**空文件**，合法；
//   - ok=false —— 没找到哨兵，调用方该走兜底解析。
//
// 之前用「body 是否为空串」判断，空文件会被误当成没拿到哨兵，
// 走了兜底路径后把杂音和标记本身一起喂给 base64 解码，直接报错。
//
// 用"第一个 begin + 其后的第一个 end"而不是 LastIndex：远端可能因为
// 多跑了一次提示而出现重复标记，取最靠前的一对才对应真正的那段正文。
func extractBetween(s, begin, end string) (body string, ok bool) {
	i := strings.Index(s, begin)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(begin):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// stripWhitespace 去掉所有空白字符。base64 不在乎它们，而远端各种实现
// 会在哪儿插换行/空格完全不可预测（-w0 缺失、tr 缺失、busybox 变体……）。
func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		}
		return r
	}, s)
}

// previewForError 取一小段内容放进错误信息里，便于用户/我们判断是哪种污染。
// 只取开头，且把不可打印字符换掉，免得日志里出现控制序列。
func previewForError(s string) string {
	const max = 60
	out := make([]rune, 0, max)
	for _, r := range s {
		if len(out) >= max {
			return string(out) + "…"
		}
		if r < 32 || r == 127 {
			out = append(out, '·')
			continue
		}
		out = append(out, r)
	}
	return string(out)
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
