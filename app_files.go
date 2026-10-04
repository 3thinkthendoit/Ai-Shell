package main

// 文件管理弹窗的后端入口：列目录、删除、上传、下载。
//
// 设计上刻意**不经过策略引擎**：这一组操作与常驻交互终端同类 ——
// 用户在自己的机器上手动点开一个目录、删一个自己选中的文件，与他在
// 终端里敲 `rm` 是同一类行为（终端那条路径同样完全绕过策略）。
// 策略引擎管的是 **LLM 提出的**命令，不是人亲手点的按钮。
//
// 但这不等于「什么都不拦」：
//   - 删除由前端弹窗二次确认（用户明确点过才知道自己删的是什么）；
//   - 后端拒绝空路径与根目录（前端那一道拦不住 bug，后端这道是底线）；
//   - 上传/下载有 32 MiB 上限，超了明确报错，而不是把内存打爆；
//   - 所有操作写审计，事后可查。

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ai-shell/internal/audit"
	"ai-shell/internal/sshclient"
)

// FileListResult 是一次列目录的结果。
type FileListResult struct {
	OK      bool                  `json:"ok"`
	Path    string                `json:"path"` // 实际列出的目录（已归一）
	Parent  string                `json:"parent"`
	Entries []sshclient.FileEntry `json:"entries"`
	Error   string                `json:"error"`
}

// FileOpResult 是删除/上传/下载的结果。
type FileOpResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Error   string `json:"error"`
	// Cancelled 表示用户在保存对话框里按了取消。这不是错误：界面必须
	// 区分「失败」（要弹红字）与「用户反悔了」（什么都不该显示）。
	Cancelled bool `json:"cancelled"`
}

// EvFileTransfer 是文件传输进度事件名。前端 bindEvents 按这个名字注册
// 监听（前端 src/store.js 的 EV_NAMES），改名要两边同步。
// 命名必须以 Ev 开头：contract_test.go 靠这个前缀从 AST 里收集事件常量，
// 做前后端事件名的交叉校验 —— 换名字会让那道防线静默失效。
const EvFileTransfer = "fm:transfer"

// FileTransferProgress 是一次下载的进度快照，随分块读逐块上报。
type FileTransferProgress struct {
	ID    string  `json:"id"`    // 传输 id，前端用它调 CancelFileTransfer
	Kind  string  `json:"kind"`  // 目前只有 "download"，留出上传分块的空间
	Name  string  `json:"name"`  // 远端文件名（进度条上显示）
	Dest  string  `json:"dest"`  // 本地保存路径
	Done  int64   `json:"done"`  // 已传输的原始字节
	Total int64   `json:"total"` // 远端文件总字节
	BPS   float64 `json:"bps"`   // 累计平均速度（字节/秒）
}

// ListRemoteDir 列出远端目录。
//
// path 为空时回落到家目录：界面上第一次打开弹窗时可能还没收到 OSC 7
// （终端刚打开、还没打印过提示符），此时给家目录比给一个空弹窗有用。
func (a *App) ListRemoteDir(hostID, path string) FileListResult {
	if err := a.ready(); err != nil {
		return FileListResult{Error: err.Error()}
	}
	if strings.TrimSpace(hostID) == "" {
		return FileListResult{Error: "请先选择一台主机"}
	}
	target := strings.TrimSpace(path)
	if target == "" {
		home, err := a.ssh.HomeDir(hostID)
		if err != nil {
			return FileListResult{Error: "无法确定远端家目录：" + err.Error()}
		}
		target = home
	}
	target = normalizeRemotePath(target)

	entries, _, err := a.ssh.ListDirEntries(hostID, target)
	if err != nil {
		return FileListResult{Path: target, Error: err.Error()}
	}
	return FileListResult{
		OK:      true,
		Path:    target,
		Parent:  parentRemotePath(target),
		Entries: entries,
	}
}

// DeleteRemotePath 删除远端的一个文件或目录。
func (a *App) DeleteRemotePath(hostID, path string) FileOpResult {
	if err := a.ready(); err != nil {
		return FileOpResult{Error: err.Error()}
	}
	if strings.TrimSpace(hostID) == "" {
		return FileOpResult{Error: "请先选择一台主机"}
	}
	target := normalizeRemotePath(strings.TrimSpace(path))
	if target == "" || target == "/" || target == "." {
		return FileOpResult{Error: "拒绝删除空路径或根目录"}
	}

	res, err := a.ssh.DeletePath(hostID, target)
	if err != nil {
		a.auditFile("删除失败："+err.Error(), hostID, target)
		return FileOpResult{Error: err.Error()}
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("远端退出码 %d", res.ExitCode)
		}
		a.auditFile("删除失败："+msg, hostID, target)
		return FileOpResult{Error: msg}
	}
	a.auditFile("删除", hostID, target)
	// 回显 target 而不是入参 path：path 可能带尾斜杠（如 /srv/app.conf/），
	// 而实际删的是归一化后的 target。回显原始输入会让用户以为删的是另一个路径。
	return FileOpResult{OK: true, Message: "已删除 " + target}
}

// UploadRemoteFile 把本地文件上传到远端目录。
//
// 走 base64 而不是原始字节：远端用 `base64 -d` 解码写盘，
// 这样二进制文件（图片、压缩包、可执行文件）不会被沿途的
// 字节→字符串转换损坏（见 sshclient.WriteFileBinary）。
//
// base64Data 是**整个文件的 base64**，由前端读本地文件后编码。
// 让前端编码而不是后端读本地路径：Wails 的 OpenFileDialog 返回的路径
// 在 WebView 里是拿不到内容的，前端要读文件只能走 `<input type=file>`，
// 而它给的正是 File 对象 —— 在那边编码一次比在 Go 里再读一次盘更直接，
// 也少传一份原始字节。
func (a *App) UploadRemoteFile(hostID, dir, name, base64Data string) FileOpResult {
	if err := a.ready(); err != nil {
		return FileOpResult{Error: err.Error()}
	}
	if strings.TrimSpace(hostID) == "" {
		return FileOpResult{Error: "请先选择一台主机"}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		// 名字里带 `/` 会让 target 变成一个完全不同的路径（可能是系统目录），
		// 而调用方以为自己只是在当前目录里放个文件。
		return FileOpResult{Error: "文件名不合法（不能为空、不能含 /）"}
	}
	target := joinRemote(normalizeRemotePath(dir), name)

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return FileOpResult{Error: "上传内容不是合法 base64：" + err.Error()}
	}
	if int64(len(data)) > sshclient.MaxTransferBytes {
		return FileOpResult{Error: fmt.Sprintf("文件超过上限（%d MiB）", sshclient.MaxTransferBytes>>20)}
	}

	res, err := a.ssh.WriteFileBinary(hostID, target, data)
	if err != nil {
		a.auditFile("上传失败："+err.Error(), hostID, target)
		return FileOpResult{Error: err.Error()}
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("远端退出码 %d", res.ExitCode)
		}
		a.auditFile("上传失败："+msg, hostID, target)
		return FileOpResult{Error: msg}
	}
	a.auditFile(fmt.Sprintf("上传 %d 字节", len(data)), hostID, target)
	// res.Stdout 里可能有备份说明（decorateBackup 翻译过），一并回给界面。
	return FileOpResult{OK: true, Message: strings.TrimSpace(res.Stdout + " 已上传 " + name)}
}

// DownloadRemoteFile 把远端文件下载到用户挑选的本地路径。
//
// 与旧实现的差别：内容不再作为 base64 塞回前端、靠浏览器下载 —— 那条路
// 在应用窗口里依赖 WebView 的下载行为（macOS 的 WKWebView 对
// <a download> 支持很差，表现为点了没反应），且无法显示进度。
// 现在改走 Wails 原生保存对话框 + 后端分块落盘 + 进度事件（fm:transfer），
// 大于一个块的文件会持续回报进度与速度，中途可取消。
//
// 流程与顺序是有讲究的：
//  1. 先探大小（StatFileSize）——文件不存在、超过上限都在**弹对话框之前**
//     失败。对话框弹了又被一条错误收场，比直接报错更让人困惑。
//  2. 再弹保存对话框 —— 用户取消就安静返回（Cancelled），什么都不显示。
//  3. 最后分块传输（downloadTo），每块回报进度。
func (a *App) DownloadRemoteFile(hostID, path string) FileOpResult {
	if err := a.ready(); err != nil {
		return FileOpResult{Error: err.Error()}
	}
	if strings.TrimSpace(hostID) == "" {
		return FileOpResult{Error: "请先选择一台主机"}
	}
	target := normalizeRemotePath(strings.TrimSpace(path))
	if target == "" || target == "/" {
		return FileOpResult{Error: "请选择一个文件"}
	}

	total, err := a.ssh.StatFileSize(hostID, target)
	if err != nil {
		a.auditFile("下载失败："+err.Error(), hostID, target)
		return FileOpResult{Error: err.Error()}
	}
	if total > sshclient.MaxTransferBytes {
		a.auditFile(fmt.Sprintf("下载被拒：文件 %d 字节超过上限", total), hostID, target)
		return FileOpResult{Error: fmt.Sprintf(
			"文件（%s）超过 %s 上限，未下载。请改用终端里的 scp/sftp 取这个大文件。",
			humanBytes(total), humanBytes(sshclient.MaxTransferBytes))}
	}

	dest, err := a.pickSaveDest(target)
	if err != nil {
		return FileOpResult{Error: err.Error()}
	}
	if dest == "" {
		// 用户在系统对话框里按了取消：不是错误，界面什么都不该显示。
		return FileOpResult{Cancelled: true}
	}

	return a.downloadTo(hostID, target, dest, func(p FileTransferProgress) {
		a.emit(EvFileTransfer, p)
	})
}

// pickSaveDest 弹出原生保存对话框，返回用户选的本地路径（取消返回空串）。
func (a *App) pickSaveDest(target string) (string, error) {
	if a.ctx == nil {
		// 测试直接构造 App 不带 ctx；生产里 startup 之后 ctx 一定就绪。
		return "", fmt.Errorf("窗口尚未就绪")
	}
	dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "保存到本机",
		DefaultFilename: path.Base(target),
	})
	if err != nil {
		return "", fmt.Errorf("无法打开保存对话框：%w", err)
	}
	return strings.TrimSpace(dest), nil
}

// downloadTo 分块读取远端文件并写到本地 dest，逐块回报进度。
//
// 单独抽出来（而不是并进 DownloadRemoteFile）是为了可测：进度回调与
// 目标路径都由调用方给，测试不需要真的弹对话框、也不需要 Wails 上下文。
//
// 两条铁律：
//   - **任何失败都必须删掉半成品本地文件**。一个「自称完整」的残缺文件
//     比失败危险得多 —— 用户会拿它去覆盖、去部署、去取证。取消同理。
//   - **写完的字节数必须等于探得的总大小**。分块之间的远端文件是可能
//     变化的（被截断、被删除），少了就报错，绝不交出缺尾巴的文件。
func (a *App) downloadTo(hostID, target, dest string, onProgress func(FileTransferProgress)) FileOpResult {
	total, err := a.ssh.StatFileSize(hostID, target)
	if err != nil {
		a.auditFile("下载失败："+err.Error(), hostID, target)
		return FileOpResult{Error: err.Error()}
	}
	if total > sshclient.MaxTransferBytes {
		a.auditFile(fmt.Sprintf("下载被拒：文件 %d 字节超过上限", total), hostID, target)
		return FileOpResult{Error: fmt.Sprintf(
			"文件（%s）超过 %s 上限，未下载。请改用终端里的 scp/sftp 取这个大文件。",
			humanBytes(total), humanBytes(sshclient.MaxTransferBytes))}
	}

	// 每次传输一个独立 context：CancelFileTransfer 找到它并触发取消。
	parent := a.ctx
	if parent == nil {
		parent = context.Background() // 测试环境没有 Wails ctx
	}
	ctx, cancel := context.WithCancel(parent)
	id := strconv.FormatInt(atomic.AddInt64(&a.tSeq, 1), 10)
	a.registerTransfer(id, cancel)
	defer a.unregisterTransfer(id)
	defer cancel()

	name := path.Base(target)
	report := func(done int64, bps float64) {
		if onProgress == nil {
			return
		}
		onProgress(FileTransferProgress{
			ID: id, Kind: "download", Name: name, Dest: dest,
			Done: done, Total: total, BPS: bps,
		})
	}

	f, err := os.Create(dest)
	if err != nil {
		a.auditFile("下载失败："+err.Error(), hostID, target)
		return FileOpResult{Error: "无法创建本地文件：" + err.Error()}
	}
	fail := func(msg string) FileOpResult {
		f.Close()
		os.Remove(dest)
		a.auditFile("下载失败："+msg, hostID, target)
		return FileOpResult{Error: msg}
	}

	start := time.Now()
	var written int64
	for written < total {
		if err := ctx.Err(); err != nil {
			return fail("已取消，未保存文件")
		}
		chunk := sshclient.ChunkSize
		if remain := total - written; remain < chunk {
			chunk = remain
		}
		data, cerr := a.ssh.ReadFileChunk(hostID, target, written, chunk)
		if cerr != nil {
			return fail(cerr.Error())
		}
		if len(data) == 0 {
			return fail(fmt.Sprintf("远端文件在传输中变短（已传 %d / %d 字节）", written, total))
		}
		if _, werr := f.Write(data); werr != nil {
			return fail("写入本地文件失败：" + werr.Error())
		}
		written += int64(len(data))
		// 速度用**累计平均**而不是瞬时值：分块粒度大，瞬时速度抖得厉害，
		// 进度条旁边跳来跳去的数字只会让人以为网络不稳。
		report(written, float64(written)/time.Since(start).Seconds())
	}

	if err := f.Close(); err != nil {
		os.Remove(dest)
		a.auditFile("下载失败："+err.Error(), hostID, target)
		return FileOpResult{Error: "写入本地文件失败：" + err.Error()}
	}
	a.auditFile(fmt.Sprintf("下载 %d 字节到本地 %s", written, dest), hostID, target)
	return FileOpResult{OK: true, Message: dest}
}

// registerTransfer / unregisterTransfer 维护活跃传输的取消注册表。
// map 懒初始化：测试直接构造 &App{}，不走 newApp()。
func (a *App) registerTransfer(id string, cancel context.CancelFunc) {
	a.tMu.Lock()
	defer a.tMu.Unlock()
	if a.tCancels == nil {
		a.tCancels = make(map[string]context.CancelFunc)
	}
	a.tCancels[id] = cancel
}

func (a *App) unregisterTransfer(id string) {
	a.tMu.Lock()
	defer a.tMu.Unlock()
	delete(a.tCancels, id)
}

// CancelFileTransfer 取消一次进行中的下载（按进度事件里给的 id）。
// 返回是否存在这样的传输 —— 前端不需要处理返回值，它只为测试与调试存在。
func (a *App) CancelFileTransfer(id string) bool {
	a.tMu.Lock()
	cancel, ok := a.tCancels[id]
	a.tMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// humanBytes 把字节数变成人能读的大小，用在错误信息里。
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	}
}

// ---- 辅助 ----

// normalizeRemotePath 归一化远端路径：去掉尾部斜杠（根目录除外）。
func normalizeRemotePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return "/"
	}
	return p
}

// parentRemotePath 返回上一级目录。已在根目录时返回根目录本身
// （而不是空串）—— 界面上「上一级」按钮据此置灰。
func parentRemotePath(p string) string {
	p = normalizeRemotePath(p)
	if p == "" || p == "/" || p == "." {
		return "/"
	}
	parent := path.Dir(p)
	if parent == "." || parent == "" {
		return "/"
	}
	return parent
}

// joinRemote 拼接远端路径。
func joinRemote(dir, name string) string {
	dir = normalizeRemotePath(dir)
	if dir == "" {
		return name
	}
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// auditFile 记一条文件操作审计。
func (a *App) auditFile(note, hostID, target string) {
	a.auditLog(audit.Entry{
		Kind:     audit.KindDirect,
		HostID:   hostID,
		HostName: hostNameOf(a.v, hostID),
		Command:  target,
		Decision: "human",
		Rule:     "file-manager",
		Note:     "文件管理：" + note,
	})
}
