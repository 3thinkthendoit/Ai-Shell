package sshclient

import (
	"strings"
	"testing"
	"time"

	"ai-shell/internal/sshtest"
)

// 在真终端里，`tail -f`、`ping`、`journalctl -f` 这类命令按 Ctrl+C 就能停。
// 之前这个软件做不到：命令会一直挂到 60 秒超时，用户没有任何办法。
// 这组用例锁住「能中断」以及「中断回来的结果长什么样」。

// 等到该主机上确实有命令在跑，再中断它。
//
// 不直接 sleep 固定时长：那既慢又不稳。改成轮询 Cancel ——
// 它在命令还没登记时返回 false，正好可以当「还没开始」的信号。
func cancelWhenRunning(t *testing.T, c *Client, hostID string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Cancel(hostID) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestCancelInterruptsRunningCommand(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h20", srv.Addr)

	c := New(v)
	defer c.Close()

	type outcome struct {
		res Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		// 超时给足 30 秒，确保「提前返回」只可能来自中断，不可能是超时
		res, err := c.Exec("h20", "sleep 30", 30*time.Second)
		ch <- outcome{res, err}
	}()

	if !cancelWhenRunning(t, c, "h20") {
		t.Fatal("没能中断：Cancel 一直返回 false，说明命令根本没被登记为「正在执行」")
	}

	select {
	case o := <-ch:
		if o.err == nil {
			t.Fatal("被中断的命令应返回错误")
		}
		if !strings.Contains(o.err.Error(), "中断") {
			t.Errorf("错误信息应说明是被中断的，实得: %v", o.err)
		}
		// 137 = 128 + SIGKILL，shell 对「被强杀」的惯例退出码。
		// 用 -1 之类不存在的值会让用户对不上号。
		if o.res.ExitCode != 137 {
			t.Errorf("退出码应为 137（128+SIGKILL），实得 %d", o.res.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("中断后 Exec 没有及时返回 —— 远端进程可能还在跑")
	}
}

// 中断一条「自己会失败」的命令，不能把失败原因改写成「被中断」。
// 两者都会让 sess.Wait() 返回错误，必须靠标志位区分。
func TestCancelDoesNotMaskRealFailure(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h21", srv.Addr)

	c := New(v)
	defer c.Close()

	res, err := c.Exec("h21", "fail", 10*time.Second)
	if err != nil {
		t.Fatalf("普通失败不该返回 error，实得: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("退出码应为远端真实值 3，实得 %d", res.ExitCode)
	}
}

// 命令跑完之后 Cancel 必须返回 false —— 否则界面会以为「刚中断了一条命令」，
// 而实际上它早就正常结束了（用户按晚了）。
func TestCancelReturnsFalseWhenNothingRunning(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h22", srv.Addr)

	c := New(v)
	defer c.Close()

	if c.Cancel("h22") {
		t.Fatal("从未执行过命令的主机不该中断成功")
	}

	if _, err := c.Exec("h22", "echo done", 10*time.Second); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if c.Cancel("h22") {
		t.Fatal("命令已结束，不该再中断成功 —— 登记项必须被清掉")
	}
}

// 登记项要在所有返回路径上被清掉，包括「启动就失败」这条。
// 否则该主机会永久停留在「有命令在跑」的状态，中断会打到一条早已不存在的命令上。
func TestCancelRegistrationClearedOnStartFailure(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h23", srv.Addr)

	c := New(v)
	defer c.Close()

	// 空命令在真实 sshd 上会被拒绝；sshtest 会当成 unknown command 返回 127。
	// 这里只关心「执行结束后登记项有没有被清掉」。
	_, _ = c.Exec("h23", "", 5*time.Second)

	if c.Cancel("h23") {
		t.Fatal("命令已结束，登记项仍存在")
	}
}

// 中断之后连接要还能用 —— 被杀的只是那条命令的会话。
// 如果实现里顺手把连接也断了，用户下一条命令就要重新握手，慢且容易被误认为「主机掉了」。
func TestConnectionStillUsableAfterCancel(t *testing.T) {
	srv := sshtest.Start(t)
	v := newVault(t)
	addPasswordHost(t, v, "h24", srv.Addr)

	c := New(v)
	defer c.Close()

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		_, _ = c.Exec("h24", "sleep 30", 30*time.Second)
	}()
	if !cancelWhenRunning(t, c, "h24") {
		t.Fatal("没能中断")
	}
	<-ch

	res, err := c.Exec("h24", "echo alive", 10*time.Second)
	if err != nil {
		t.Fatalf("中断后连接应仍可用，实得: %v", err)
	}
	if !strings.Contains(res.Stdout, "alive") {
		t.Fatalf("中断后的命令应正常返回输出，实得: %q", res.Stdout)
	}
}
