package main

import "testing"

func TestSessionLaneMutex(t *testing.T) {
	var l sessionLane
	ok, _ := l.tryEnterShell("h1")
	if !ok {
		t.Fatal("shell 应能进入")
	}
	if !l.shellBusy() || l.shellHostID() != "h1" {
		t.Fatalf("busy=%v host=%q", l.shellBusy(), l.shellHostID())
	}
	if ok, reason := l.tryEnterAgent(); ok {
		t.Fatal("shell 占用时 Ask 应失败")
	} else if reason == "" {
		t.Fatal("应有拒绝原因")
	}
	if ok, _ := l.tryEnterShell("h2"); ok {
		t.Fatal("不可双 shell")
	}
	l.leaveShell()
	if l.shellBusy() || l.shellHostID() != "" {
		t.Fatal("leave 后应空闲")
	}
	ok, _ = l.tryEnterAgent()
	if !ok {
		t.Fatal("agent 应能进入")
	}
	if ok, _ := l.tryEnterShell("h1"); ok {
		t.Fatal("agent 占用时 shell 应失败")
	}
	l.leaveAgent()
}
