package sshclient

import (
	"strings"
	"testing"
	"time"

	"ai-shell/internal/sshtest"
)

// Exec 会把输出整段读进内存，而输出大小完全由远端决定。
// 用户敲一句 `cat /var/log/huge` 就能让后端吃满内存，
// 再让整个字符串经 IPC 进到 WebView 的 DOM 里。
// 这组用例锁住「有上限」以及「超限之后命令仍然会正常结束」。

// withCaptureLimit 把捕获上限临时压小，让测试跑得快。
func withCaptureLimit(t *testing.T, n int) {
	t.Helper()
	old := MaxCaptureBytes
	MaxCaptureBytes = n
	t.Cleanup(func() { MaxCaptureBytes = old })
}

func TestOutputIsCappedAndFlagged(t *testing.T) {
	withCaptureLimit(t, 8*1024)
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h30", srv.Addr)

	c := New(v)
	defer c.Close()

	// 请求 1 MiB，上限 8 KiB
	res, err := c.Exec("h30", "bigout 1048576", 20*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(res.Stdout) != 8*1024 {
		t.Fatalf("应恰好保留上限字节数 8192，实得 %d", len(res.Stdout))
	}
	if !res.Truncated {
		t.Fatal("超过上限时必须置 Truncated —— 否则用户以为「就这么多输出」")
	}
}

// 这条是整个设计的要害：达到上限后必须**继续排空**。
//
// 破坏这个行为有两种改法，都会被这条用例挡住（已用变异实测）：
//   - 超限后阻塞读端（`select{}`）→ 拷贝 goroutine 永不返回，
//     ssh 的 Wait() 卡在等待拷贝结束时，本用例 8 秒后报「命令没有结束」；
//   - 超限后返回错误 → 命令会**失败**，本用例在 err 分支报错。
//
// 两种都比正确实现差：用户要么看到「卡住」，要么看到一条本来能成功的命令报错。
func TestOutputBeyondLimitDoesNotHangTheCommand(t *testing.T) {
	withCaptureLimit(t, 4*1024)
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h31", srv.Addr)

	c := New(v)
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		// 输出量是上限的 200 倍
		_, err := c.Exec("h31", "bigout 819200", 10*time.Second)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("超限的输出不该导致失败，实得: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("命令没有结束 —— 达到上限后停止了读取，远端被管道阻塞")
	}
}

// stderr 也要有上限：`command 2>&1 >/dev/null` 这类用法同样能刷爆。
func TestStderrIsCappedToo(t *testing.T) {
	withCaptureLimit(t, 4*1024)
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h32", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h32", "bigerr 65536", 20*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(res.Stderr) != 4*1024 {
		t.Fatalf("stderr 应恰好保留上限字节数 4096，实得 %d", len(res.Stderr))
	}
	if !res.Truncated {
		t.Fatal("stderr 超限同样要置 Truncated")
	}
}

// 正常大小的输出绝不能被打上截断标记 —— 误报会让用户白白去找不存在的内容。
func TestSmallOutputNotFlagged(t *testing.T) {
	withCaptureLimit(t, 64*1024)
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h33", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h33", "echo hello", 10*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Truncated {
		t.Fatalf("小输出不该标记为截断（实得 %d 字节）", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("输出内容不对: %q", res.Stdout)
	}
}

// 恰好等于上限时不算截断：边界差一错会让正常命令莫名多出一行提示。
func TestExactlyAtLimitNotFlagged(t *testing.T) {
	withCaptureLimit(t, 4096)
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h34", srv.Addr)

	c := New(v)
	defer c.Close()

	// writeBulk 以 4097 字节为一整块，这里要一个正好 4096 的量。
	// 用 head -c 在远端截，比在这里拼更直接。
	res, err := c.Exec("h34", "head -c 4096 -- /dev/zero", 10*time.Second)
	if err != nil {
		// sshtest 的 head 走的是内存文件系统，路径不存在会返回 1；
		// 那就退回到「输出量正好等于上限」的等价检查。
		t.Logf("head 路径不可用（%v），改用整块写入验证边界", err)
	}
	_ = res

	// 用 bigout 写 4096 字节：等于上限，不该标记截断
	res2, err := c.Exec("h34", "bigout 4096", 10*time.Second)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res2.Truncated {
		t.Fatalf("正好等于上限不该标记截断（实得 %d 字节）", len(res2.Stdout))
	}
}

// 上限是变量，取值必须合理：太小会让正常命令频繁被截断，
// 太大就起不到保护内存的作用。这条防止有人手滑改成 0 或负数。
func TestDefaultCaptureLimitIsSane(t *testing.T) {
	// 注意这里读的是默认值，前面几条用例的临时修改会被 t.Cleanup 还原。
	if MaxCaptureBytes < 64*1024 {
		t.Errorf("默认上限太小（%d），正常命令会被频繁截断", MaxCaptureBytes)
	}
	if MaxCaptureBytes > 16<<20 {
		t.Errorf("默认上限太大（%d），起不到保护内存的作用", MaxCaptureBytes)
	}
}

// cappedBuffer 必须报告「全部写入成功」。
// 报短写会让 ssh 的拷贝循环当成错误而中断会话 ——
// 表现是命令莫名失败，而不是输出被截断。
func TestCappedBufferReportsFullWrite(t *testing.T) {
	b := newCappedBuffer(4)
	p := []byte("0123456789")

	n, err := b.Write(p)
	if err != nil {
		t.Fatalf("不该返回错误: %v", err)
	}
	if n != len(p) {
		t.Fatalf("必须报告写入全部 %d 字节，实得 %d —— 短写会中断会话", len(p), n)
	}
	if b.String() != "0123" {
		t.Fatalf("只应保留前 4 字节，实得 %q", b.String())
	}
	if b.dropped != 6 {
		t.Fatalf("丢弃字节数应为 6，实得 %d", b.dropped)
	}

	// 后续写入全部丢弃，但仍然报告成功
	n, _ = b.Write([]byte("more"))
	if n != 4 {
		t.Fatalf("超限后仍须报告全部写入，实得 %d", n)
	}
	if b.String() != "0123" {
		t.Fatalf("超限后内容不该再变，实得 %q", b.String())
	}
	if b.dropped != 10 {
		t.Fatalf("丢弃字节数应为 10，实得 %d", b.dropped)
	}
}
