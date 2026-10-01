package sshclient

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-shell/internal/policy"
	"ai-shell/internal/sshtest"
	"ai-shell/internal/vault"
)

// 本文件是**真实的端到端集成测试**。
//
// 之前所有测试都在验证「逻辑符合预期」，但没有验证「真的能连上 Linux」。
// 这里用 internal/sshtest 在 127.0.0.1 上起一个真实 SSH 服务器，
// 走完整的 TCP + SSH 握手 + 会话 + exec 通道，把下列链路一次跑通：
//
//	vault 加密存取 → sshclient 拨号认证 → TOFU 指纹 → 命令执行 →
//	退出码/stdout/stderr 分离 → 超时 → 策略裁决 → 输出脱敏

func newVault(t *testing.T) *vault.Vault {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}
	return v
}

// addPasswordHost 注册一台走密码认证的测试主机。
func addPasswordHost(t *testing.T, v *vault.Vault, id, addr string) {
	t.Helper()
	if err := v.SaveHost(vault.Host{
		ID: id, Name: id, Addr: addr,
		User: sshtest.User, AuthMethod: vault.AuthPassword,
	}, &vault.HostSecret{Password: sshtest.Password}); err != nil {
		t.Fatal(err)
	}
}

// 端到端主链路：密码认证 + 真实 TCP/SSH 握手 + 命令执行。
func TestEndToEndPasswordAuth(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h1", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h1", "echo hello-from-ssh", 10*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码应为 0，实际 %d（stderr: %s）", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-from-ssh") {
		t.Fatalf("stdout 不符: %q", res.Stdout)
	}
	// 注意：DurationMs 对本地环回上的 echo 可能就是 0（不足 1ms），这是正确行为。
	// 真实环境里 SSH 往返通常在数十毫秒量级。
	if res.DurationMs < 0 {
		t.Fatalf("耗时不应为负: %d", res.DurationMs)
	}
}

// 公钥认证路径：走 ssh.ParsePrivateKey 解析 OpenSSH 私钥。
func TestEndToEndPrivateKeyAuth(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)

	if err := v.SaveHost(vault.Host{
		ID: "h2", Name: "key-host", Addr: srv.Addr,
		User: sshtest.KeyUser, AuthMethod: vault.AuthPrivateKey,
	}, &vault.HostSecret{PrivateKey: sshtest.UserKeyPEM(t)}); err != nil {
		t.Fatal(err)
	}

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h2", "echo key-auth-ok", 10*time.Second)
	if err != nil {
		t.Fatalf("公钥认证失败: %v", err)
	}
	if !strings.Contains(res.Stdout, "key-auth-ok") {
		t.Fatalf("stdout 不符: %q", res.Stdout)
	}
}

// 错误的密码必须被拒绝。
func TestEndToEndWrongPasswordRejected(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	_ = v.SaveHost(vault.Host{
		ID: "h3", Name: "bad", Addr: srv.Addr,
		User: sshtest.User, AuthMethod: vault.AuthPassword,
	}, &vault.HostSecret{Password: "wrong-password"})

	c := New(v)
	defer c.Close()

	if _, err := c.Exec("h3", "echo nope", 10*time.Second); err == nil {
		t.Fatal("错误密码不应连接成功")
	}
}

// stdout / stderr / 退出码三者必须正确分离。
func TestEndToEndStreamsAndExitCode(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h4", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h4", "fail", 10*time.Second)
	if err != nil {
		t.Fatalf("非零退出码不应作为传输错误返回: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("退出码应为 3，实际 %d", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("stderr 未正确分离: %q", res.Stderr)
	}
	if strings.Contains(res.Stdout, "boom") {
		t.Fatalf("stderr 内容混进了 stdout: %q", res.Stdout)
	}
}

// TOFU：首次连接应记录主机指纹。
func TestTOFURecordsFingerprint(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h5", srv.Addr)

	if _, ok := v.HostKey(srv.Addr); ok {
		t.Fatal("首次连接前不应有指纹记录")
	}

	c := New(v)
	defer c.Close()
	if _, err := c.Exec("h5", "echo first", 10*time.Second); err != nil {
		t.Fatalf("首次连接失败: %v", err)
	}

	fp, ok := v.HostKey(srv.Addr)
	if !ok {
		t.Fatal("首次连接后应记录主机指纹")
	}
	if fp != srv.HostKeyFP {
		t.Fatalf("指纹不符：记录 %s，实际 %s", fp, srv.HostKeyFP)
	}
}

// 指纹不匹配必须中断连接 —— 这是防中间人的核心断言。
func TestTOFUMismatchAbortsConnection(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h6", srv.Addr)

	// 伪造一个已记录的指纹（模拟主机密钥被替换）
	if err := v.SetHostKey(srv.Addr, "SHA256:AAAAtotallyWrongFingerprintAAAA"); err != nil {
		t.Fatal(err)
	}

	c := New(v)
	defer c.Close()
	_, err := c.Exec("h6", "echo should-not-run", 10*time.Second)
	if err == nil {
		t.Fatal("指纹不匹配时必须中断连接")
	}
	if !strings.Contains(err.Error(), "指纹") {
		t.Fatalf("错误信息应说明是指纹问题: %v", err)
	}
}

// 超时必须能真正中断长命令，而不是干等。
func TestEndToEndTimeout(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h7", srv.Addr)

	c := New(v)
	defer c.Close()

	start := time.Now()
	_, err := c.Exec("h7", "sleep 30", 1*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("超长命令应返回超时错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应说明超时: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时未被及时中断，实际耗时 %s", elapsed)
	}
}

// system_info 必须能真正跑通 —— 这是第二轮修掉的那个 bug 的端到端回归。
func TestEndToEndSystemInfo(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h8", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.SystemInfo("h8")
	if err != nil {
		t.Fatalf("system_info 失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("system_info 退出码应为 0，实际 %d", res.ExitCode)
	}
	for _, want := range []string{"== os ==", "== memory ==", "== listening =="} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("system_info 输出缺少 %q", want)
		}
	}
}

// 完整安全链路：策略裁决 → 真实执行 → 输出脱敏。
// 这是本项目三条核心防线在一次真实 SSH 往返里的联合验证。
func TestEndToEndPolicyAndRedaction(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h9", srv.Addr)

	c := New(v)
	defer c.Close()

	// 防线 1：读取凭据文件的命令必须被硬拒绝，根本到不了远端
	for _, cmd := range []string{"cat /etc/shadow", "cat ~/.ssh/id_rsa", "printenv"} {
		if verdict := policy.Evaluate(cmd, policy.ModeWhitelist, []string{"cat"}); verdict.Decision != policy.Deny {
			t.Fatalf("命令 %q 应被硬拒绝，实际 %s", cmd, verdict.Decision)
		}
	}

	// 防线 2：合法命令真实执行
	res, err := c.Exec("h9", "cat "+sshtest.ConfPath, 10*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !strings.Contains(res.Stdout, sshtest.LeakedSecret) {
		t.Fatalf("测试前置条件不成立，配置中应含口令: %q", res.Stdout)
	}

	// 防线 3：回传给 LLM 前，口令必须被脱敏，且输出被标记为不可信数据。
	// 注意：LeakedSecret 并不在 vault 的密钥列表里 —— 所以必须靠模式识别兜住。
	// 这恰好验证了「不依赖已知密钥列表」的那一层。
	payload, redacted, findings := policy.PrepareForLLM(res.Stdout, v.SecretStrings(), true, 0)
	if redacted == 0 {
		t.Fatal("配置中的口令未被模式识别脱敏")
	}
	if strings.Contains(payload, sshtest.LeakedSecret) {
		t.Fatalf("回传给 LLM 的内容中仍含口令: %q", payload)
	}
	if !strings.Contains(payload, "<<<UNTRUSTED_REMOTE_OUTPUT>>>") {
		t.Fatal("输出未被标记为不可信数据")
	}
	if len(findings) != 0 {
		t.Fatalf("正常配置不应触发注入告警: %v", findings)
	}

	// 同时验证：vault 里存的已知密钥会被整段替换
	knownPayload, n, _ := policy.PrepareForLLM("leaked: "+sshtest.Password, v.SecretStrings(), true, 0)
	if n == 0 || strings.Contains(knownPayload, sshtest.Password) {
		t.Fatal("vault 中的已知密钥未被脱敏")
	}
}

// 连接复用：同一主机连续执行多条命令，不应重复握手。
func TestConnectionReuse(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h10", srv.Addr)

	c := New(v)
	defer c.Close()

	first, err := c.Exec("h10", "echo one", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Exec("h10", "echo two", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Stdout, "one") || !strings.Contains(second.Stdout, "two") {
		t.Fatalf("复用连接后结果异常: %q / %q", first.Stdout, second.Stdout)
	}

	c.mu.Lock()
	pooled := len(c.pool)
	c.mu.Unlock()
	if pooled != 1 {
		t.Fatalf("应复用同一条连接，实际池中有 %d 条", pooled)
	}
}

// Ping 是 UI「测试连接」背后的实现，必须可用。
func TestPing(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h11", srv.Addr)

	c := New(v)
	defer c.Close()

	if err := c.Ping("h11"); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
}

// 主机不存在 / 未配置凭据时，应给出清晰错误而不是 panic。
func TestErrorCases(t *testing.T) {
	v := newVault(t)
	c := New(v)
	defer c.Close()

	if _, err := c.Exec("nonexistent", "echo x", time.Second); err == nil {
		t.Fatal("不存在的主机应报错")
	}

	_ = v.SaveHost(vault.Host{
		ID: "h12", Name: "nosecret", Addr: "127.0.0.1:1",
		User: sshtest.User, AuthMethod: vault.AuthPassword,
	}, nil)
	if _, err := c.Exec("h12", "echo x", time.Second); err == nil {
		t.Fatal("未配置凭据的主机应报错")
	}
}

// 读文件 / 列目录工具的真实往返。
func TestReadFileAndListDir(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h13", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.ReadFile("h13", sshtest.NginxPath, 4096)
	if err != nil {
		t.Fatalf("read_file 失败: %v", err)
	}
	if !strings.Contains(res.Stdout, "worker_processes") {
		t.Fatalf("read_file 内容不符: %q", res.Stdout)
	}

	dir, err := c.ListDir("h13", "/srv")
	if err != nil {
		t.Fatalf("list_dir 失败: %v", err)
	}
	if !strings.Contains(dir.Stdout, "app.conf") {
		t.Fatalf("list_dir 内容不符: %q", dir.Stdout)
	}
}

// ---- 写文件：全流程里破坏性最强的操作，此前完全没有覆盖 ----
//
// 下面 4 条针对的都是「覆写远端生产配置」这个动作。它们此前全是空白，
// 而 write_file 恰恰是唯一会直接改坏远端文件的工具。

// 覆写已存在的文件时，必须先产生一份内容为旧值的备份。
func TestWriteFileBacksUpBeforeOverwrite(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "w1", srv.Addr)

	c := New(v)
	defer c.Close()

	const target = "/srv/app.conf"
	const oldBody = "listen = 127.0.0.1:9000\n"
	const newBody = "listen = 0.0.0.0:8080\n"
	srv.SetRemoteFile(target, oldBody)

	res, err := c.WriteFile("w1", target, newBody)
	if err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("退出码应为 0，实际 %d（stderr: %s）", res.ExitCode, res.Stderr)
	}

	got, ok := srv.ReadRemoteFile(target)
	if !ok || got != newBody {
		t.Fatalf("远端内容未被正确写入：%q", got)
	}

	// 备份必须真的存在，且内容是被覆盖前的旧值 —— 否则「备份」只是句空话。
	baks := srv.Backups(target)
	if len(baks) != 1 {
		t.Fatalf("应恰好产生 1 份备份，实际 %d 份：%v", len(baks), baks)
	}
	if bak, _ := srv.ReadRemoteFile(baks[0]); bak != oldBody {
		t.Fatalf("备份内容应为旧值 %q，实际 %q", oldBody, bak)
	}

	// 备份路径要回报给用户/LLM，而不是被静默丢弃。
	if !strings.Contains(res.Stdout, baks[0]) {
		t.Fatalf("stdout 应回报备份路径 %q，实际 %q", baks[0], res.Stdout)
	}
	// 内部协议标记不得泄漏到界面。
	if strings.Contains(res.Stdout, "__AISHELL") {
		t.Fatalf("内部标记泄漏到 stdout：%q", res.Stdout)
	}
}

// 目标不存在时应正常新建，并明确回报「无需备份」，而不是被误读成备份失败。
func TestWriteFileNewFileReportedAsNew(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "w2", srv.Addr)

	c := New(v)
	defer c.Close()

	const target = "/srv/brand-new.conf"
	srv.RemoveRemoteFile(target)

	res, err := c.WriteFile("w2", target, "hello\n")
	if err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("新建文件应成功，实际退出码 %d（stderr: %s）", res.ExitCode, res.Stderr)
	}
	if got, ok := srv.ReadRemoteFile(target); !ok || got != "hello\n" {
		t.Fatalf("文件未创建或内容不符：%q", got)
	}
	if baks := srv.Backups(target); len(baks) != 0 {
		t.Fatalf("新建文件不应产生备份，实际：%v", baks)
	}
	if !strings.Contains(res.Stdout, "新建") {
		t.Fatalf("应明确回报为新建，实际 %q", res.Stdout)
	}
}

// 备份失败必须中止写入。
//
// 这是本组里最重要的一条：旧实现是 `cp … 2>/dev/null; cat > …`，
// 备份失败会被完全吞掉，然后在没有任何退路的情况下覆写目标文件。
// 正确行为是「备份不成功就不写」，且失败原因要能传到用户眼前。
func TestWriteFileAbortsWhenBackupFails(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "w3", srv.Addr)

	c := New(v)
	defer c.Close()

	const target = "/srv/app.conf"
	const oldBody = "listen = 127.0.0.1:9000\n"
	srv.SetRemoteFile(target, oldBody)
	srv.FailBackup(true)

	res, err := c.WriteFile("w3", target, "SHOULD NOT BE WRITTEN\n")
	if err != nil {
		t.Fatalf("备份失败应作为退出码返回，而不是 Go 层错误: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("备份失败时不应返回成功")
	}

	// 关键断言：目标文件必须原封不动。
	if got, _ := srv.ReadRemoteFile(target); got != oldBody {
		t.Fatalf("备份失败后目标文件被改动了，原内容应为 %q，实际 %q", oldBody, got)
	}
	// 失败原因不能被吞掉。
	if !strings.Contains(res.Stderr, "No space left") {
		t.Fatalf("备份失败的原因应出现在 stderr，实际 %q", res.Stderr)
	}
	// 内部协议标记在失败路径上同样不得泄漏。
	if strings.Contains(res.Stdout, "__AISHELL") {
		t.Fatalf("内部标记泄漏到 stdout：%q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "备份原文件失败") {
		t.Fatalf("应明确告知备份失败并已中止，实际 %q", res.Stdout)
	}
}

// 同一秒内连续写两次，必须产生两份**内容不同**的备份。
//
// 旧实现的备份后缀只有 `$(date +%s)`（秒级），同一秒内第二次写入会生成
// 同一个备份名，把第一份备份覆盖掉 —— 而「连续改两次同一个配置」恰恰是
// agent 修配置时最常见的形态，等于第一次的原值被永久丢失。
func TestWriteFileBackupNamesDoNotCollide(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "w4", srv.Addr)

	c := New(v)
	defer c.Close()

	const target = "/srv/app.conf"
	srv.SetRemoteFile(target, "v0\n")

	if _, err := c.WriteFile("w4", target, "v1\n"); err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	if _, err := c.WriteFile("w4", target, "v2\n"); err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}

	baks := srv.Backups(target)
	if len(baks) != 2 {
		t.Fatalf("两次写入应产生 2 份备份（互不覆盖），实际 %d 份：%v", len(baks), baks)
	}

	// 两份备份应分别保存 v0 与 v1 —— 若备份名撞了，这里只会剩一份。
	seen := map[string]bool{}
	for _, b := range baks {
		body, _ := srv.ReadRemoteFile(b)
		seen[body] = true
	}
	for _, want := range []string{"v0\n", "v1\n"} {
		if !seen[want] {
			t.Fatalf("备份 %q 丢失，说明备份名发生碰撞被覆盖；现有备份内容：%v", want, seen)
		}
	}
}

// 含单引号与空格的路径必须能被正确引用并解析（shellQuote ↔ 脚本解析的往返）。
func TestWriteFileQuotesTrickyPath(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "w5", srv.Addr)

	c := New(v)
	defer c.Close()

	const target = "/srv/my app's config.conf"
	srv.RemoveRemoteFile(target)

	res, err := c.WriteFile("w5", target, "ok\n")
	if err != nil {
		t.Fatalf("write_file 失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("含引号的路径应能写入，实际退出码 %d（stderr: %s）", res.ExitCode, res.Stderr)
	}
	if got, ok := srv.ReadRemoteFile(target); !ok || got != "ok\n" {
		t.Fatalf("路径含单引号时写入失败，实际内容 %q（ok=%v）", got, ok)
	}
}

// 冷启动时多个 goroutine 同时向同一主机发命令。
//
// connect() 刻意不在持有锁的情况下拨号（否则一次慢拨号会卡住所有其他主机），
// 代价是同一主机可能被并发拨号多次。dial() 用「新连接覆盖旧连接、并关掉旧的」
// 来收敛 —— 但如果某个调用方已经拿到那个被关掉的连接，它的命令就会失败。
//
// 触发场景很现实：界面轮询 + 用户手动执行，或 agent 在同一主机上并发跑多个工具。
func TestConcurrentFirstUseSameHost(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "cc1", srv.Addr)

	c := New(v)
	defer c.Close()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时出发
			res, err := c.Exec("cc1", "echo hello", 10*time.Second)
			if err != nil {
				errs <- fmt.Errorf("#%d 执行失败: %w", i, err)
				return
			}
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
				errs <- fmt.Errorf("#%d 结果异常: exit=%d out=%q", i, res.ExitCode, res.Stdout)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("并发首次使用同一主机时失败: %v", err)
	}
}

// 在命令执行途中并发断开连接：不应 panic，也不应泄漏。
func TestConcurrentExecAndDisconnect(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "cc2", srv.Addr)

	c := New(v)
	defer c.Close()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 允许失败（连接被别的 goroutine 断掉了），但绝不能 panic。
			_, _ = c.Exec("cc2", "echo x", 5*time.Second)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Disconnect("cc2")
		}()
	}
	wg.Wait()

	// 断开之后仍应能重新建立连接并正常工作。
	res, err := c.Exec("cc2", "echo after", 10*time.Second)
	if err != nil {
		t.Fatalf("并发断开后无法恢复: %v", err)
	}
	if !strings.Contains(res.Stdout, "after") {
		t.Fatalf("恢复后的结果异常: %q", res.Stdout)
	}
}

// Exec 必须把 stdin 关死（发 EOF）：这个客户端永远不会给命令喂交互输入，
// 那么读 stdin 的程序（无 TTY 的 vim 退化成 ex 模式等输入、裸 cat、
// 要密码的 sudo）就该当场读到 EOF 报错退出，而不是挂满整个超时窗口。
// 回归场景：用户跑 `vi x.txt`，等了整整 60 秒超时才看到 TTY 提示。
func TestExecClosesStdinWithEOF(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "heof", srv.Addr)
	c := New(v)
	t.Cleanup(c.Close)

	start := time.Now()
	res, err := c.Exec("heof", "hungry", 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("hungry 应该读到 EOF 秒退，实得错误: %v", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("命令是等到超时才结束的（%s）——stdin 没有发 EOF", elapsed)
	}
	if !strings.Contains(res.Stdout, "read 0 bytes") {
		t.Errorf("远端应读到 0 字节（只发 EOF，不写内容），实得 %q", res.Stdout)
	}
}
