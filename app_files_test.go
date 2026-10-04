package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

	got, _ := mustDownload(t, a, hostID, "/srv/blob.bin")
	if string(got) != string(raw) {
		t.Fatalf("二进制内容被改动了：\n发出 %v\n收回 %v", raw, got)
	}
}

// 回归：远端 stdout 里混进了**别的行**时，下载仍必须成功。
//
// 这就是线上报的那条：
//
//	远端返回的内容不是合法 base64（该机器的 base64 实现可能不支持 -w0）：
//	illegal base64 data at input byte 4
//
// 内容本身完全正确，问题在于远端多打了几行 —— 登录脚本的欢迎语、
// busybox base64 的用法提示、`bc: command not found` 之类。
// 旧实现只在正文**前面**放了一个 SIZE 标记，挡不住**尾巴**上的污染：
// 提示行拼在 base64 后面，解码就在正文中间某处失败，报出的字节偏移
// 看起来像"数据坏了"，其实数据是好的。
//
// 现在正文被一对哨兵夹住，标记之外的一切都被无视。几条用例分别覆盖
// 头部污染、尾部污染、以及两头都有 —— 最后一种才是真实环境最常见的。
func TestDownloadRemoteFile_SurvivesNoisyStdout(t *testing.T) {
	a, hostID, srv := newShellTestAppWithServer(t, vault.ModeManual, nil)

	raw := []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if up := a.UploadRemoteFile(hostID, "/srv", "noisy.bin", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("上传失败: %s", up.Error)
	}

	cases := []struct {
		name string
		head []string
		tail []string
	}{
		{"尾巴有提示（本次线上那种）", nil, []string{"bc: command not found\n"}},
		{"头部有欢迎语", []string{"Welcome to Ubuntu 22.04 LTS\n"}, nil},
		{"两头都有", []string{"-bash: /etc/profile.d/x.sh: Permission denied\n"}, []string{"base64: invalid option -- 'w'\n"}},
		{"多行尾部污染", nil, []string{"Usage: base64 [-d] [file]\n", "bc: command not found\n"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv.SetDownloadNoise(c.head, c.tail)
			t.Cleanup(func() { srv.SetDownloadNoise(nil, nil) })

			got, _ := mustDownload(t, a, hostID, "/srv/noisy.bin")
			if string(got) != string(raw) {
				t.Fatalf("杂音混进了内容里：\n发出 %v\n收回 %v", raw, got)
			}
		})
	}
}

// 远端把 base64 换行了（busybox 不认 -w0）也必须能解出来。
//
// 换行本身不算污染 —— 它在哨兵**内部**。这条用例守的是
// stripWhitespace 那道处理：漏了它，凡是 base64 不带 -w0 的机器
// 都会下载失败，而这类机器（精简镜像、老发行版）恰恰很常见。
func TestDownloadRemoteFile_SurvivesWrappedBase64(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 内容要足够长，超过 76 字符才会被假远端折行。
	raw := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 8))
	if up := a.UploadRemoteFile(hostID, "/srv", "long.txt", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("上传失败: %s", up.Error)
	}

	got, _ := mustDownload(t, a, hostID, "/srv/long.txt")
	if string(got) != string(raw) {
		t.Fatalf("内容被改动：\n发出 %d 字节\n收回 %d 字节", len(raw), len(got))
	}
}

// 真的解不出来时，错误信息必须带上「远端到底回了什么」。
//
// 原来只报 `illegal base64 data at input byte 4` —— 用户看到的是"数据坏了"，
// 完全无法判断是文件问题还是远端在乱打日志。带上开头的内容预览，
// 一眼就能看出尾巴上多了一行提示。
func TestDownloadRemoteFile_CorruptBodyReportsPreview(t *testing.T) {
	a, hostID, srv := newShellTestAppWithServer(t, vault.ModeManual, nil)

	// 用一段不被 base64 字符集接受的内容当正文：模拟远端把二进制
	// 原样吐了出来（而不是编码后）。哨兵之间全是非法字符。
	srv.SetCorruptDownloadBody(true)
	t.Cleanup(func() { srv.SetCorruptDownloadBody(false) })

	dest, res, _ := doDownload(t, a, hostID, "/srv/app.conf")
	if res.OK {
		t.Fatal("正文非法时下载应失败")
	}
	if !strings.Contains(res.Error, "不是合法 base64") {
		t.Fatalf("错误信息应指明 base64 解析失败，实得: %s", res.Error)
	}
	// 关键：得说清楚"收到了什么"，否则用户只能猜。
	if !strings.Contains(res.Error, "收到") {
		t.Fatalf("错误信息应带上远端回的内容预览，实得: %s", res.Error)
	}
	// 失败时半成品文件必须被清掉：留着会被人当成完整文件去用。
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("失败的下载不应留下半成品文件（%s）", dest)
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

// doDownload 走一遍真实下载（分块落盘路径 downloadTo），把进度事件收集起来。
// dest 固定落在临时目录，测试结束时自动清理。
func doDownload(t *testing.T, a *App, hostID, path string) (string, FileOpResult, []FileTransferProgress) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out.bin")
	var events []FileTransferProgress
	res := a.downloadTo(hostID, path, dest, func(p FileTransferProgress) {
		events = append(events, p)
	})
	return dest, res, events
}

// mustDownload 是 doDownload 的「应该成功」版本，顺带读回落盘内容。
func mustDownload(t *testing.T, a *App, hostID, path string) ([]byte, []FileTransferProgress) {
	t.Helper()
	dest, res, events := doDownload(t, a, hostID, path)
	if !res.OK {
		t.Fatalf("下载失败: %s", res.Error)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("下载结果文件读不回来: %v", err)
	}
	return data, events
}

func TestDownloadRemoteFile_WritesContent(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 假远端预置了 ConfPath。
	data, _ := mustDownload(t, a, hostID, "/srv/app.conf")
	if !strings.Contains(string(data), "listen") {
		t.Fatalf("下载内容不符: %q", string(data))
	}
}

func TestDownloadRemoteFile_MissingFileReportsError(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)
	dest, res, _ := doDownload(t, a, hostID, "/srv/does-not-exist")
	if res.OK {
		t.Fatal("下载不存在的文件应报错")
	}
	if res.Error == "" {
		t.Fatal("失败时必须给出原因，否则界面只能显示一个空错误")
	}
	// 探测阶段就失败，本地连文件都不该有。
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("失败的下载不应留下半成品文件（%s）", dest)
	}
}

// 空文件的下载必须是**成功的空文件**，而不是失败。
//
// 总量为 0 时分块循环一次都不进，直接落一个 0 字节的本地文件 ——
// 这本身就是用户要的结果。别把它当成「什么都没发生」的失败路径。
func TestDownloadRemoteFile_EmptyFile(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	// 上传一个空文件作为下载对象。
	if up := a.UploadRemoteFile(hostID, "/srv", "empty.bin", ""); !up.OK {
		t.Fatalf("上传空文件失败: %s", up.Error)
	}

	data, _ := mustDownload(t, a, hostID, "/srv/empty.bin")
	if len(data) != 0 {
		t.Fatalf("空文件不应有内容, 实得 %d 字节", len(data))
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
	// 于是「超限」这条路径真的被走到 —— 探测（wc -c）先行，
	// 超限在弹对话框、写任何本地字节之前就被拒绝。
	old := sshclient.MaxTransferBytes
	sshclient.MaxTransferBytes = 8
	t.Cleanup(func() { sshclient.MaxTransferBytes = old })

	dest, res, _ := doDownload(t, a, hostID, "/srv/app.conf")
	if res.OK {
		t.Fatalf("超限的下载必须被拒绝，却报告成功（%s）", dest)
	}
	if !strings.Contains(res.Error, "上限") {
		t.Fatalf("拒绝理由应说明是超过上限，实得 %q", res.Error)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的下载不应留下半成品文件（%s）", dest)
	}
}

// 文件**恰好等于**上限时必须正常下载 —— 这是「按长度顶格即视为截断」
// 那种实现会误报的边界，也是这里先用 wc -c 探明真实大小的原因。
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

	got, _ := mustDownload(t, a, hostID, "/srv/exact.bin")
	if string(got) != string(raw) {
		t.Fatalf("内容不符：期望 %q，实得 %q", raw, got)
	}
}

// 远端把 END 哨兵弄丢时，只要 base64 载荷完整，下载就必须成功。
//
// 真实机器上出现过：base64 完整、END 始终没出现（连接抖动、shell 提前
// 收工），数据一个字节都没坏。旧版整文件下载在这种情况下的行为是
// 把哨兵一起喂给解码器，报出莫名其妙的
// "不是合法 base64：illegal base64 data at input byte 0"——
// 数据是好的，报错却像文件坏了。现在的抢救路径按「解码长度与请求的
// 块大小严格相等」放行。
func TestDownloadRemoteFile_MissingEndMarkStillSucceeds(t *testing.T) {
	a, hostID, srv := newShellTestAppWithServer(t, vault.ModeManual, nil)

	srv.SetDownloadMissingEnd(true)
	t.Cleanup(func() { srv.SetDownloadMissingEnd(false) })

	raw := []byte("payload without a closing marker from the remote")
	if up := a.UploadRemoteFile(hostID, "/srv", "noend.bin", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("准备用例的上传失败: %s", up.Error)
	}

	got, _ := mustDownload(t, a, hostID, "/srv/noend.bin")
	if string(got) != string(raw) {
		t.Fatalf("抢救出的内容不符：期望 %q，实得 %q", raw, got)
	}
}

// 载荷也不完整时必须报错，绝不悄悄收下残缺的块。
//
// 最阴险的形态：base64 在 4 字符边界上被截断 —— 仍然"合法"、仍然解得动，
// 但只有前一半。抢救路径的长度闸门（解码长度 == 请求的块大小）就是
// 防这道题的：不符即拒，报错里带上应传字节数。
func TestDownloadRemoteFile_TruncatedChunkFails(t *testing.T) {
	a, hostID, srv := newShellTestAppWithServer(t, vault.ModeManual, nil)

	srv.SetDownloadMissingEnd(true)
	srv.SetDownloadShortBody(true)
	t.Cleanup(func() {
		srv.SetDownloadMissingEnd(false)
		srv.SetDownloadShortBody(false)
	})

	raw := []byte(strings.Repeat("x", 4096))
	if up := a.UploadRemoteFile(hostID, "/srv", "short.bin", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("准备用例的上传失败: %s", up.Error)
	}

	dest, res, _ := doDownload(t, a, hostID, "/srv/short.bin")
	if res.OK {
		t.Fatal("载荷残缺的块必须报错，不能当成功处理")
	}
	if !strings.Contains(res.Error, "不完整") {
		t.Fatalf("报错应说明输出不完整，实得 %q", res.Error)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("失败的下载不应留下半成品文件（%s）", dest)
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

// 分块下载必须逐块回报进度：这是进度条与速度显示的数据来源。
//
// 2.5 MiB 恰好横跨 ChunkSize=1 MiB 的三块边界（1M / 2M / 2.5M），
// 一旦有人把 ChunkSize 调大导致退化成单块、或把分块逻辑改坏，
// 这里的块数断言会立刻抓住。
func TestDownloadRemoteFile_ReportsChunkedProgress(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	raw := []byte(strings.Repeat("x", 2*1024*1024+512*1024))
	if up := a.UploadRemoteFile(hostID, "/srv", "big.bin", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("准备用例的上传失败: %s", up.Error)
	}

	dest, res, events := doDownload(t, a, hostID, "/srv/big.bin")
	if !res.OK {
		t.Fatalf("下载失败: %s", res.Error)
	}
	if !strings.Contains(res.Message, dest) && res.Message != dest {
		t.Fatalf("成功结果应给出保存路径，实得 %q", res.Message)
	}

	wantChunks := (len(raw) + int(sshclient.ChunkSize) - 1) / int(sshclient.ChunkSize)
	if len(events) != wantChunks {
		t.Fatalf("应逐块回报 %d 次进度，实得 %d 次", wantChunks, len(events))
	}
	var last int64
	for i, p := range events {
		if p.Done <= last {
			t.Fatalf("进度必须单调递增：第 %d 次 done=%d，上一次 %d", i, p.Done, last)
		}
		last = p.Done
		if p.Total != int64(len(raw)) {
			t.Fatalf("进度必须带总大小 %d，实得 %d", len(raw), p.Total)
		}
		if p.BPS <= 0 {
			t.Fatalf("进度必须带正的速度，第 %d 次实得 %v", i, p.BPS)
		}
		if p.ID == "" || p.Name != "big.bin" || p.Dest != dest {
			t.Fatalf("进度缺少 id/文件名/目标路径：%+v", p)
		}
	}
	if last != int64(len(raw)) {
		t.Fatalf("最后一块的 done 应等于总大小 %d，实得 %d", len(raw), last)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("下载结果文件读不回来: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("分块拼接内容不符：发出 %d 字节，收回 %d 字节", len(raw), len(got))
	}
}

// 取消必须真的停下来，且半成品文件必须被删掉 ——
// 用户按了取消之后屏幕上若留下一个「看起来完整」的残缺文件，
// 它迟早会被拿去当真文件用。
func TestDownloadRemoteFile_CancelStopsAndCleansUp(t *testing.T) {
	a, hostID := newShellTestApp(t, vault.ModeManual, nil)

	raw := []byte(strings.Repeat("x", 3*1024*1024))
	if up := a.UploadRemoteFile(hostID, "/srv", "canc.bin", base64.StdEncoding.EncodeToString(raw)); !up.OK {
		t.Fatalf("准备用例的上传失败: %s", up.Error)
	}

	dest := filepath.Join(t.TempDir(), "out.bin")
	var events []FileTransferProgress
	res := a.downloadTo(hostID, "/srv/canc.bin", dest, func(p FileTransferProgress) {
		events = append(events, p)
		// 收到第一次进度就取消：模拟用户盯着进度条点了「取消」。
		a.CancelFileTransfer(p.ID)
	})
	if res.OK {
		t.Fatal("取消后的下载不应报告成功")
	}
	if !strings.Contains(res.Error, "已取消") {
		t.Fatalf("取消应有可读的报错，实得 %q", res.Error)
	}
	if len(events) < 1 {
		t.Fatal("取消前至少应有一次进度回调")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("取消后必须清掉半成品文件（%s）", dest)
	}
}

// 取消一个不存在的传输必须安全返回 false，而不是 panic 或误伤别的传输。
func TestCancelFileTransfer_UnknownIDReturnsFalse(t *testing.T) {
	a, _ := newShellTestApp(t, vault.ModeManual, nil)
	if a.CancelFileTransfer("no-such-id") {
		t.Fatal("不存在的传输 id 应返回 false")
	}
}
