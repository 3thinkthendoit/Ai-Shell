package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// 这一组用例覆盖文件管理弹窗的后端：列目录、删除、上传、下载。
//
// 重点不在「顺利路径能跑通」，而在几条**错了会很危险**的边界：
// 空路径/根目录必须拒绝删除、文件名里的路径穿越必须拒绝、
// 超过上限必须明确报错而不是把内存打爆、二进制内容必须一字不差。

func TestListRemoteDir_EmptyPathFallsBackToHome(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 空路径是有意为之：前端在还没收到 OSC 7 时会传空串，
	// 后端负责回落到家目录 —— 让「拿不到目录」只有一个处理点。
	res := a.ListRemoteDir(hostID, "")
	if !res.OK {
		t.Fatalf("空路径应回落到家目录，实得错误: %s", res.Error)
	}
	if res.Path != "/home/test" {
		t.Fatalf("应回落到假远端的家目录 /home/test，实得 %q", res.Path)
	}
	if len(res.Entries) == 0 {
		t.Fatal("家目录不该是空的（假远端固定有几项）")
	}
}

func TestListRemoteDir_ParsesEntries(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	res := a.ListRemoteDir(hostID, "/etc/nginx")
	if !res.OK {
		t.Fatalf("列目录失败: %s", res.Error)
	}
	if res.Path != "/etc/nginx" {
		t.Fatalf("路径应为 /etc/nginx，实得 %q", res.Path)
	}
	if res.Parent != "/etc" {
		t.Fatalf("上一级应为 /etc，实得 %q", res.Parent)
	}

	var dir, file bool
	for _, e := range res.Entries {
		if e.Name == "subdir" {
			dir = true
			if !e.IsDir {
				t.Error("subdir 应被识别为目录（权限串以 d 开头）")
			}
			// 路径必须是拼好的绝对路径，前端点进去时直接用它。
			if e.Path != "/etc/nginx/subdir" {
				t.Errorf("目录路径应为 /etc/nginx/subdir，实得 %q", e.Path)
			}
		}
		if e.Name == "app.conf" {
			file = true
			if e.IsDir {
				t.Error("app.conf 不该被判为目录")
			}
			if e.Size != 120 {
				t.Errorf("大小应为 120，实得 %d", e.Size)
			}
			if e.ModTime == 0 {
				// 别在消息里写 "%s"：go vet 会把 t.Error 的参数当成格式化串，
				// 报 "possible Printf formatting directive"。改成不带 % 的措辞。
				t.Error("时间戳应被解析出来（远端 --time-style 给的是 Unix 秒）")
			}
		}
	}
	if !dir || !file {
		t.Fatalf("应同时列出目录与文件，实得 %+v", res.Entries)
	}

	// `.` 与 `..` 必须在解析层就被滤掉：导航靠「上一级」按钮，
	// 让它们出现在列表里只会让用户疑惑该点哪个。
	for _, e := range res.Entries {
		if e.Name == "." || e.Name == ".." {
			t.Errorf("不该把 %q 列进结果", e.Name)
		}
	}
}

// 目录列表里含空格的文件名必须完整保留。
// 这是选 `ls -lan --time-style=+%s` 而不是按列切分的全部理由 ——
// 按固定列位置切的话，"my report.txt" 会被切成 "my" 和 "report.txt"。
func TestListRemoteDir_KeepsNamesWithSpaces(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 先上传一个含空格的文件名，再列出来看。
	up := a.UploadRemoteFile(hostID, "/srv", "my report.txt", base64.StdEncoding.EncodeToString([]byte("hi")))
	if !up.OK {
		t.Fatalf("上传失败: %s", up.Error)
	}

	res := a.ListRemoteDir(hostID, "/srv")
	if !res.OK {
		t.Fatalf("列目录失败: %s", res.Error)
	}
	var found bool
	for _, e := range res.Entries {
		if e.Name == "my report.txt" {
			found = true
			if e.Path != "/srv/my report.txt" {
				t.Errorf("含空格文件名的路径应完整，实得 %q", e.Path)
			}
		}
	}
	if !found {
		t.Fatalf("含空格的文件名没被完整列出，实得 %+v", res.Entries)
	}
}

// 根目录的「上一级」是它自己 —— 前端据此把按钮置灰。
func TestListRemoteDir_RootParentIsItself(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)
	res := a.ListRemoteDir(hostID, "/")
	if !res.OK {
		t.Fatalf("列根目录失败: %s", res.Error)
	}
	if res.Parent != "/" {
		t.Fatalf("根目录的上一级应是 /，实得 %q", res.Parent)
	}
}

func TestDeleteRemotePath_DeletesFile(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 上传一个文件，确认它在，然后删掉，确认它真的不在了。
	up := a.UploadRemoteFile(hostID, "/srv", "gone.txt", base64.StdEncoding.EncodeToString([]byte("x")))
	if !up.OK {
		t.Fatalf("上传失败: %s", up.Error)
	}
	before := a.ListRemoteDir(hostID, "/srv")
	if !hasEntry(before.Entries, "gone.txt") {
		t.Fatalf("上传后应能列到文件，实得 %+v", before.Entries)
	}

	del := a.DeleteRemotePath(hostID, "/srv/gone.txt")
	if !del.OK {
		t.Fatalf("删除失败: %s", del.Error)
	}

	after := a.ListRemoteDir(hostID, "/srv")
	if hasEntry(after.Entries, "gone.txt") {
		t.Fatal("删除后文件仍在列表里 —— 删除没有真正生效")
	}
}

// 空路径与根目录必须被拒绝。这一条是底线：漏了就是删整台机器。
func TestDeleteRemotePath_RefusesDangerousPaths(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	for _, p := range []string{"", "   ", "/", ".", "//"} {
		res := a.DeleteRemotePath(hostID, p)
		if res.OK {
			t.Errorf("删除 %q 应被拒绝，但它报告成功了", p)
		}
		if res.Error == "" {
			t.Errorf("删除 %q 被拒绝时必须给出原因", p)
		}
	}
}

// 上传必须支持二进制：内容里的 \x00 与无效 UTF-8 不能被沿途损坏。
// 这是整个上传走 base64 而不是直接塞字符串的理由。
func TestUploadRemoteFile_BinaryRoundTrip(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 一段含 NUL、0xFF、以及无效 UTF-8 序列的字节。
	raw := []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	up := a.UploadRemoteFile(hostID, "/srv", "blob.bin", base64.StdEncoding.EncodeToString(raw))
	if !up.OK {
		t.Fatalf("上传失败: %s", up.Error)
	}

	down := a.DownloadRemoteFile(hostID, "/srv/blob.bin")
	if !down.OK {
		t.Fatalf("下载失败: %s", down.Error)
	}
	got, err := base64.StdEncoding.DecodeString(down.Message)
	if err != nil {
		t.Fatalf("下载回来不是合法 base64: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("二进制内容被改动了：\n发出 %v\n收回 %v", raw, got)
	}
}

// 文件名里带 / 必须被拒绝：那会把文件写到另一个目录去，
// 而调用方以为自己只是「在当前目录里放个文件」。
func TestUploadRemoteFile_RejectsPathTraversal(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	for _, name := range []string{"", "  ", "../evil", "a/b", "x\x00y"} {
		res := a.UploadRemoteFile(hostID, "/srv", name, base64.StdEncoding.EncodeToString([]byte("x")))
		if res.OK {
			t.Errorf("文件名 %q 应被拒绝", name)
		}
	}
}

// 超过上限时必须明确报错，而不是把它读进内存再说 ——
// 那种失败方式会把页面和后端一起打爆。
func TestUploadRemoteFile_RejectsOversize(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 把上限压到 8 字节，这样一条短内容就能验证「超过上限被拒」，
	// 而不必真的构造 32 MiB 的字符串（那会让用例本身很慢）。
	old := sshclient.MaxTransferBytes
	sshclient.MaxTransferBytes = 8
	t.Cleanup(func() { sshclient.MaxTransferBytes = old })

	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 64)))
	res := a.UploadRemoteFile(hostID, "/srv", "big.txt", big)
	if res.OK {
		t.Fatal("超过上限的上传应被拒绝")
	}
	if !strings.Contains(res.Error, "上限") {
		t.Fatalf("错误信息应说明是超过上限，实得 %q", res.Error)
	}
}

func TestDownloadRemoteFile_ReturnsContent(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 假远端预置了 ConfPath。
	down := a.DownloadRemoteFile(hostID, "/srv/app.conf")
	if !down.OK {
		t.Fatalf("下载失败: %s", down.Error)
	}
	data, err := base64.StdEncoding.DecodeString(down.Message)
	if err != nil {
		t.Fatalf("内容不是合法 base64: %v", err)
	}
	if !strings.Contains(string(data), "listen") {
		t.Fatalf("下载内容不符: %q", string(data))
	}
}

func TestDownloadRemoteFile_MissingFileReportsError(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)
	res := a.DownloadRemoteFile(hostID, "/srv/does-not-exist")
	if res.OK {
		t.Fatal("下载不存在的文件应报错")
	}
	if res.Error == "" {
		t.Fatal("失败时必须给出原因，否则界面只能显示一个空错误")
	}
}

// 超过上限的下载必须**明确拒绝**，而不是交回一段被静默截断的内容。
//
// 这条守的是本功能最危险的一种失败：用户下载一个大日志，只拿到前半截
// 却看到「已下载」，然后拿这份残缺文件去做覆盖或迁移。那种损失是不可逆的，
// 而且现场没有任何线索指向「文件其实是完整的，只是没传完」。
func TestDownloadRemoteFile_RejectsOversize(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 把上限压到 8 字节。假远端预置的 /srv/app.conf 远长于它，
	// 于是「超限」这条路径真的被走到 —— 前提是假远端**真的按 limit 截断**
	// （它现在会了；以前不截断，这条用例根本测不出东西）。
	old := sshclient.MaxTransferBytes
	sshclient.MaxTransferBytes = 8
	t.Cleanup(func() { sshclient.MaxTransferBytes = old })

	res := a.DownloadRemoteFile(hostID, "/srv/app.conf")
	if res.OK {
		// 最要命的形态：返回成功，但内容只有 8 字节。
		data, _ := base64.StdEncoding.DecodeString(res.Message)
		t.Fatalf("超限的下载必须被拒绝，却报告成功并返回了 %d 字节", len(data))
	}
	if !strings.Contains(res.Error, "上限") {
		t.Fatalf("拒绝理由应说明是超过上限，实得 %q", res.Error)
	}
}

// 文件**恰好等于**上限时必须正常下载 —— 这是「按长度顶格即视为截断」
// 那种实现会误报的边界，也是这里专门回报真实大小的原因。
func TestDownloadRemoteFile_ExactLimitIsNotTruncated(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	raw := []byte("0123456789")
	up := a.UploadRemoteFile(hostID, "/srv", "exact.bin", base64.StdEncoding.EncodeToString(raw))
	if !up.OK {
		t.Fatalf("准备用例的上传失败: %s", up.Error)
	}

	old := sshclient.MaxTransferBytes
	sshclient.MaxTransferBytes = int64(len(raw))
	t.Cleanup(func() { sshclient.MaxTransferBytes = old })

	res := a.DownloadRemoteFile(hostID, "/srv/exact.bin")
	if !res.OK {
		t.Fatalf("恰好等于上限的文件应能下载，实得错误: %s", res.Error)
	}
	got, _ := base64.StdEncoding.DecodeString(res.Message)
	if string(got) != string(raw) {
		t.Fatalf("内容不符：期望 %q，实得 %q", raw, got)
	}
}

// 没选主机时所有操作都该给出可读的拒绝，而不是发一条空主机 ID 的请求。
func TestFileOps_RequireHost(t *testing.T) {
	a, _ := newShellTestApp(t, vault.ModeManual, nil)

	if res := a.ListRemoteDir("", "/tmp"); res.OK {
		t.Error("没选主机时列目录应被拒绝")
	}
	if res := a.DeleteRemotePath("", "/tmp/x"); res.OK {
		t.Error("没选主机时删除应被拒绝")
	}
	if res := a.DownloadRemoteFile("", "/tmp/x"); res.OK {
		t.Error("没选主机时下载应被拒绝")
	}
	if res := a.UploadRemoteFile("", "/tmp", "a", ""); res.OK {
		t.Error("没选主机时上传应被拒绝")
	}
}

func hasEntry(entries []sshclient.FileEntry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}
