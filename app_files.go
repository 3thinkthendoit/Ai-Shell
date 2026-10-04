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
	"encoding/base64"
	"fmt"
	"path"
	"strings"

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

// DownloadRemoteFile 读取远端文件，返回 base64 内容交给前端保存。
//
// 不在这里弹「保存到哪」的对话框：那需要 Wails 的 SaveFileDialog，
// 而它要求调用线程有 Wails 上下文。由前端拿数据后用 <a download> 触发
// 浏览器下载更简单，也不必让后端知道用户想存哪。
//
// **超过上限时明确拒绝，而不是返回截断的内容。** 这是这个函数里最重要的
// 一条：用户下载一个大日志，如果只拿到前半截却没有任何提示，他很可能
// 拿这份残缺文件去做覆盖/迁移/取证 —— 静默截断比直接报错危险得多。
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

	data, mayTruncate, total, _, err := a.ssh.ReadFileBinary(hostID, target, sshclient.MaxTransferBytes)
	if err != nil {
		a.auditFile("下载失败："+err.Error(), hostID, target)
		return FileOpResult{Error: err.Error()}
	}
	if mayTruncate {
		size := ""
		if total > 0 {
			size = fmt.Sprintf("（%s）", humanBytes(total))
		}
		a.auditFile(fmt.Sprintf("下载被拒：文件 %d 字节超过上限", total), hostID, target)
		return FileOpResult{Error: fmt.Sprintf(
			"文件%s 超过 %s 上限，未下载。请改用终端里的 scp/sftp 取这个大文件。",
			size, humanBytes(sshclient.MaxTransferBytes))}
	}
	a.auditFile(fmt.Sprintf("下载 %d 字节", len(data)), hostID, target)
	return FileOpResult{
		OK:      true,
		Message: base64.StdEncoding.EncodeToString(data),
	}
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
