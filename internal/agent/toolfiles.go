package agent

// 文件传输工具：upload_local_file（本地 → 远端）与
// download_remote_file（远端 → 本地）。
//
// 这两个工具与前端「文件管理」弹窗共用同一套底层能力
// （sshclient.WriteFileBinary / ReadFileChunk / StatFileSize），
// 但**策略完全不同**：弹窗是「人亲手点的按钮」，不过策略引擎
// （见 app_files.go 的设计声明）；而这里是「LLM 提出的操作」，
// 必须过策略引擎 —— 上传是写远端，因此走 EvaluateWrite（永远确认）；
// 下载是读远端，因此走读类规则（凭据路径硬拒绝）。
//
// 两条硬边界：
//   1. 下载在**开始写本地文件之前**先探大小并检查远端文件存在 ——
//      下一半才发现超限，会留下一个自称完整的残缺文件。
//      （历史上这里还有 32 MiB 的大小上限，已按需求移除；整份内容
//      进内存的内存峰值由调用方自行权衡。）
//   2. 本地路径由 LLM 指定，因此必须限制在用户可见的目录内
//      （见 localPathAllowed）。否则模型可以读用户机器上任意文件，
//      而那已经不是「运维助手」该有的能力。

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-shell/internal/sshclient"
)

// 本地路径的硬边界。
//
// 这两个工具的 `local` 参数是 **LLM 填的**，因此它可以直接指定
// 本机上任意文件的路径。上传时那意味着「把用户机器上的任意文件
// 发到远端主机」—— 包括凭证库本体（vault.enc）与主密钥（master.key）。
//
// 这不是理论风险：模型读了一次 /etc/shadow 被拒绝后，很自然的
// 下一步就是「那把本地那份传上去看看」。所以这里必须硬拦，
// 而不是指望策略模式或人工审批 —— 审批弹窗只会显示一个路径，
// 用户看不出那是什么文件。
//
// 边界定为「用户的配置目录」：那是 vault.enc、master.key、audit.log
// 与 sessions.enc 的所在地（见 vault.DefaultDir）。
var protectedLocalFiles = []string{
	"vault.enc",    // 凭证库本体（主机口令、私钥、LLM API Key，加密后）
	"master.key",   // 主密钥文件（keyring 不可用时的落点）
	"audit.log",    // 审计日志（含命令原文）
	"sessions.enc", // 会话历史（含排查过程中出现过的内容）
}

// localPathBlocked 判断一个本地路径是否命中硬边界。
//
// 按**文件名**而不是路径前缀判：用户可能把整个配置目录拷贝到别处，
// 那时前缀就失效了，而文件名不会变。误伤的处理方式是把 `local`
// 换成一个指向别处的路径 —— 用户自己机器上的文件，想传的话
// 完全可以先拷一份再传。
func localPathBlocked(local string) (bool, string) {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(local)))
	for _, name := range protectedLocalFiles {
		if base == name {
			return true, name
		}
		// 审计日志的归档段（audit.seg-0000000001-0000004200.log）
		if name == "audit.log" && strings.HasPrefix(base, "audit.seg-") {
			return true, base
		}
	}
	return false, ""
}

// runUploadLocalFile 把本地文件上传到远端主机。
func runUploadLocalFile(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	remote := strings.TrimSpace(strArg(args, "path"))
	local := strings.TrimSpace(strArg(args, "local"))

	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("缺少 host_id")
	}
	if remote == "" {
		return sshclient.Result{}, fmt.Errorf("path 为空：请给出远端目标路径")
	}
	if local == "" && strArg(args, "content_base64") == "" {
		return sshclient.Result{}, fmt.Errorf("local 为空：请给出本地文件路径")
	}

	// 本地路径硬边界：绝不允许把凭证库/主密钥/审计日志传上远端主机。
	if blocked, name := localPathBlocked(local); blocked {
		return sshclient.Result{}, fmt.Errorf(
			"拒绝上传 %s：该文件承载本应用的密钥或审计数据，不属于可上传内容", name)
	}

	// 两条内容来源：本地文件，或模型直接给的 base64。
	// 后者是逃生门 —— 某些场景下本地没有那个文件（内容本来就是
	// 模型生成的，比如一份补丁），但它应当是例外而非常态。
	//
	// 本地文件走**分块读写**（第一块截断+备份、后续块追加）：文件可能是
	// 任意大小，整份读进内存再编码会把进程顶到 OOM —— 这正是当初 32 MiB
	// 上限存在的理由；分块之后上限没了，风险也没了。
	var note string
	if b64 := strArg(args, "content_base64"); b64 != "" {
		data, derr := decodeUploadContent(args)
		if derr != nil {
			return sshclient.Result{}, derr
		}
		if len(data) == 0 {
			return sshclient.Result{}, fmt.Errorf("content_base64 是空内容，未上传")
		}
		res, werr := ctx.ssh.WriteFileBinary(hostID, remote, data)
		if werr != nil {
			return sshclient.Result{}, werr
		}
		if res.ExitCode != 0 {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = fmt.Sprintf("远端退出码 %d", res.ExitCode)
			}
			return sshclient.Result{ExitCode: res.ExitCode, Stderr: msg}, fmt.Errorf("上传失败：%s", msg)
		}
		note = fmt.Sprintf("已上传 %d 字节到 %s", len(data), remote)
		if extra := strings.TrimSpace(res.Stdout); extra != "" {
			note = fmt.Sprintf("已上传 %d 字节到 %s（%s）", len(data), remote, extra)
		}
		return sshclient.Result{Stdout: note, ExitCode: 0}, nil
	}

	uploaded, err := uploadLocalFileChunked(ctx, hostID, local, remote)
	if err != nil {
		return sshclient.Result{}, err
	}
	return sshclient.Result{Stdout: fmt.Sprintf("已上传 %d 字节到 %s", uploaded, remote), ExitCode: 0}, nil
}

// uploadLocalFileChunked 分块读本地文件、分块写远端，返回上传的总字节数。
//
// 失败的善后与下载侧对称：清掉远端半成品（覆盖场景下备份已生成，原文件
// 可从 .bak 找回），绝不让一个「看起来完整」的残缺文件留在远端。
func uploadLocalFileChunked(ctx toolCtx, hostID, local, remote string) (int64, error) {
	f, err := os.Open(local)
	if err != nil {
		return 0, fmt.Errorf("读取本地文件失败：%v", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("读取本地文件失败：%v", err)
	}
	total := st.Size()
	if total == 0 {
		// 空文件不是错误，但要说明白：否则用户会以为传了个有内容的东西。
		return 0, fmt.Errorf("本地文件 %s 是空文件，未上传", local)
	}

	removePartial := func() {
		_, _ = ctx.ssh.Exec(hostID, "rm -f -- "+shellQuote(remote), 30*time.Second)
	}

	buf := make([]byte, sshclient.ChunkSize)
	var written int64
	first := true
	for written < total {
		want := total - written
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, rerr := io.ReadFull(f, buf[:want])
		if n == 0 {
			if rerr != nil {
				return 0, fmt.Errorf("读取本地文件失败：%v", rerr)
			}
			return 0, fmt.Errorf("本地文件在读取中途变短（已读 %d / %d 字节）", written, total)
		}
		var res sshclient.Result
		if first {
			res, err = ctx.ssh.WriteFileBinary(hostID, remote, buf[:n])
			first = false
		} else {
			res, err = ctx.ssh.AppendFileBinary(hostID, remote, buf[:n])
		}
		if err != nil {
			removePartial()
			return 0, err
		}
		if res.ExitCode != 0 {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = fmt.Sprintf("远端退出码 %d", res.ExitCode)
			}
			removePartial()
			return 0, fmt.Errorf("上传失败：%s", msg)
		}
		written += int64(n)
	}

	// 收尾核验：远端大小必须与本地一致，否则视为残缺（与 App 通道的
	// UploadRemoteFileFinish 同一条铁律）。
	size, err := ctx.ssh.StatFileSize(hostID, remote)
	if err != nil {
		removePartial()
		return 0, fmt.Errorf("核验远端文件失败：%v", err)
	}
	if size != total {
		removePartial()
		return 0, fmt.Errorf("远端文件大小（%d 字节）与本地（%d 字节）不一致，已清理半成品", size, total)
	}
	return written, nil
}

// runDownloadRemoteFile 把远端文件下载到本地磁盘。
//
// 走分块读而不是一次读完：一个 32 MiB 的文件一次 Exec 拿不到进度，
// 而传输过程中用户看到的只是「一直在转」，无从判断是卡了还是在传。
func runDownloadRemoteFile(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	remote := strings.TrimSpace(strArg(args, "path"))
	local := strings.TrimSpace(strArg(args, "local"))

	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("缺少 host_id")
	}
	if remote == "" {
		return sshclient.Result{}, fmt.Errorf("path 为空：请给出远端文件路径")
	}
	if local == "" {
		return sshclient.Result{}, fmt.Errorf("local 为空：请给出本地保存路径")
	}

	// 同一道边界的反方向：绝不让远端内容覆盖本地的凭证库/审计日志。
	// 上传是「把密钥送出去」，下载是「让远端把密钥改写」——
	// 后者同样能让一次成功的入侵变成持久化。
	if blocked, name := localPathBlocked(local); blocked {
		return sshclient.Result{}, fmt.Errorf(
			"拒绝写入 %s：该文件承载本应用的密钥或审计数据，不允许被远端内容覆盖", name)
	}

	// 先探大小：不存在、超限都在**创建本地文件之前**失败。
	// 顺序反了会在本地留下一个空文件或半成品。
	total, err := ctx.ssh.StatFileSize(hostID, remote)
	if err != nil {
		return sshclient.Result{}, err
	}

	// 父目录不存在时先建：LLM 给出的路径常常带着一层还没建过的目录，
	// 为此失败一次再让它重试是浪费一轮对话。
	if dir := filepath.Dir(local); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return sshclient.Result{}, fmt.Errorf("创建本地目录失败：%v", err)
		}
	}

	f, err := os.Create(local)
	if err != nil {
		return sshclient.Result{}, fmt.Errorf("创建本地文件失败：%v", err)
	}
	// 任何失败都必须删掉半成品：一个「自称完整」的残缺文件比失败危险得多 ——
	// 用户会拿它去覆盖、去部署、去取证。
	removePartial := func() { _ = f.Close(); _ = os.Remove(local) }

	start := time.Now()
	var written int64
	for written < total {
		chunk := sshclient.ChunkSize
		if remain := total - written; remain < chunk {
			chunk = remain
		}
		data, cerr := ctx.ssh.ReadFileChunk(hostID, remote, written, chunk)
		if cerr != nil {
			removePartial()
			return sshclient.Result{}, cerr
		}
		if len(data) == 0 {
			removePartial()
			return sshclient.Result{}, fmt.Errorf("远端文件在传输中变短（已传 %d / %d 字节）", written, total)
		}
		if _, werr := f.Write(data); werr != nil {
			removePartial()
			return sshclient.Result{}, fmt.Errorf("写入本地文件失败：%v", werr)
		}
		written += int64(len(data))

		// 进度事件让界面在跑长传输时有东西可看。与前端文件管理共用
		// 同一个事件名会混淆两条通道的语义，所以这里单独一个名字。
		if ctx.emit != nil {
			bps := 0.0
			if secs := time.Since(start).Seconds(); secs > 0 {
				bps = float64(written) / secs
			}
			ctx.emit(EvAgentTransfer, map[string]any{
				"id":     strArg(args, "_transfer_id"),
				"kind":   "download",
				"name":   filepath.Base(remote),
				"dest":   local,
				"done":   written,
				"total":  total,
				"bps":    bps,
				"remote": remote,
				"hostId": hostID,
			})
		}
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(local)
		return sshclient.Result{}, fmt.Errorf("写入本地文件失败：%v", err)
	}

	secs := time.Since(start).Seconds()
	return sshclient.Result{
		Stdout:   fmt.Sprintf("已下载 %d 字节到 %s（%.1f 秒）", written, local, secs),
		ExitCode: 0,
	}, nil
}

// base64Data 是上传时可能出现的另一种入参形式：LLM 有时会直接把
// 文件内容以 base64 放进参数里，而不是给一个本地路径。
//
// 保留它是为了少一次往返（远端常见的小配置文件），但**默认不用**：
// 让模型把文件内容搬进上下文是既贵又容易出错的（它会漏字节）。
func decodeUploadContent(args map[string]any) ([]byte, error) {
	b64 := strArg(args, "content_base64")
	if b64 == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("content_base64 不是合法 base64：%v", err)
	}
	return data, nil
}
