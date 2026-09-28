package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ai-shell/internal/vault"
)

// newTestLogger 用真实的 vault 作为 Sealer —— 顺带验证两个包的接口能对接上。
func newTestLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	t.Setenv("AISHELL_KEYFILE", "1")
	dir := t.TempDir()

	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}
	l, err := New(dir, v)
	if err != nil {
		t.Fatalf("创建审计日志失败: %v", err)
	}
	return l, dir
}

func TestAppendAndRead(t *testing.T) {
	l, _ := newTestLogger(t)

	for i := 0; i < 3; i++ {
		if err := l.Append(Entry{
			Kind: KindTool, HostName: "web01", Tool: "run_command",
			Command: "systemctl status nginx", Decision: "confirm", Rule: "manual",
		}); err != nil {
			t.Fatalf("追加失败: %v", err)
		}
	}

	entries, err := l.ReadAll()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(entries))
	}
	for i, e := range entries {
		if e.Seq != i+1 {
			t.Fatalf("第 %d 条序号错误: %d", i+1, e.Seq)
		}
		if e.Time == "" {
			t.Fatalf("第 %d 条缺少时间戳", i+1)
		}
		if e.Hash == "" {
			t.Fatalf("第 %d 条缺少哈希", i+1)
		}
	}
	if entries[0].PrevHash != "" {
		t.Fatal("首条的 PrevHash 应为空")
	}
	if entries[1].PrevHash != entries[0].Hash {
		t.Fatal("链式 PrevHash 未正确串联")
	}
}

// 核心断言：命令原文不得以明文出现在磁盘上。
func TestEncryptedAtRest(t *testing.T) {
	l, dir := newTestLogger(t)
	const secretCmd = "mysql -u root -pSuperSecret123"

	if err := l.Append(Entry{Kind: KindDirect, Command: secretCmd}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretCmd, "SuperSecret123", "mysql", "run_command"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("审计日志泄露了明文片段 %q", leak)
		}
	}
}

func TestVerifyIntactChain(t *testing.T) {
	l, _ := newTestLogger(t)
	for i := 0; i < 5; i++ {
		_ = l.Append(Entry{Kind: KindTool, Command: "ls -la"})
	}
	res := l.Verify()
	if !res.OK {
		t.Fatalf("完整链条应校验通过，实际: %s", res.Message)
	}
	if res.Count != 5 {
		t.Fatalf("期望 5 条，实际 %d", res.Count)
	}
}

func TestVerifyDetectsDeletion(t *testing.T) {
	l, dir := newTestLogger(t)
	for i := 0; i < 5; i++ {
		_ = l.Append(Entry{Kind: KindTool, Command: "ls"})
	}

	// 攻击者删掉中间一条
	path := filepath.Join(dir, "audit.log")
	lines := readLines(t, path)
	if len(lines) != 5 {
		t.Fatalf("期望 5 行，实际 %d", len(lines))
	}
	writeLines(t, path, append(lines[:2], lines[3:]...))

	res := l.Verify()
	if res.OK {
		t.Fatal("删除条目后校验不应通过")
	}
	if res.BrokenAt == 0 {
		t.Fatal("应定位到断点位置")
	}
}

func TestVerifyDetectsReordering(t *testing.T) {
	l, dir := newTestLogger(t)
	for i := 0; i < 4; i++ {
		_ = l.Append(Entry{Kind: KindTool, Command: "ls"})
	}

	path := filepath.Join(dir, "audit.log")
	lines := readLines(t, path)
	lines[1], lines[2] = lines[2], lines[1] // 调换顺序
	writeLines(t, path, lines)

	if res := l.Verify(); res.OK {
		t.Fatal("重排条目后校验不应通过")
	}
}

func TestVerifyDetectsForeignEntry(t *testing.T) {
	l, dir := newTestLogger(t)
	_ = l.Append(Entry{Kind: KindTool, Command: "ls"})

	// 攻击者用自己的密钥伪造一条追加进去 —— 解密阶段就应该失败
	path := filepath.Join(dir, "audit.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("Zm9yZ2VkLWVudHJ5LW5vdC1lbmNyeXB0ZWQtd2l0aC10aGUtcmlnaHQta2V5\n")
	_ = f.Close()

	if res := l.Verify(); res.OK {
		t.Fatal("伪造条目后校验不应通过")
	}
}

func TestRecentLimits(t *testing.T) {
	l, _ := newTestLogger(t)
	for i := 0; i < 10; i++ {
		_ = l.Append(Entry{Kind: KindTool, Command: "cmd"})
	}
	got, err := l.Recent(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(got))
	}
	// 应返回最近的，序号为 8/9/10
	if got[0].Seq != 8 || got[2].Seq != 10 {
		t.Fatalf("返回的不是最近记录: %d..%d", got[0].Seq, got[2].Seq)
	}
}

func TestExportPlaintext(t *testing.T) {
	l, dir := newTestLogger(t)
	_ = l.Append(Entry{Kind: KindTool, Command: "uptime"})

	dest := filepath.Join(dir, "export.jsonl")
	n, err := l.ExportPlaintext(dest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("期望导出 1 条，实际 %d", n)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "uptime") {
		t.Fatalf("导出内容应含明文命令: %s", string(b))
	}
}

func TestReopenRestoresChain(t *testing.T) {
	l, dir := newTestLogger(t)
	_ = l.Append(Entry{Kind: KindTool, Command: "a"})
	_ = l.Append(Entry{Kind: KindTool, Command: "b"})

	// 模拟应用重启：重新打开同一目录
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	l2, err := New(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Append(Entry{Kind: KindTool, Command: "c"}); err != nil {
		t.Fatal(err)
	}

	entries, err := l2.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(entries))
	}
	if entries[2].Seq != 3 {
		t.Fatalf("重启后序号未接续: %d", entries[2].Seq)
	}
	if res := l2.Verify(); !res.OK {
		t.Fatalf("重启后链条应完整: %s", res.Message)
	}
}

func TestEmptyLogVerifies(t *testing.T) {
	l, _ := newTestLogger(t)
	if res := l.Verify(); !res.OK {
		t.Fatalf("空日志应校验通过: %s", res.Message)
	}
}

// ---- 辅助 ----

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
