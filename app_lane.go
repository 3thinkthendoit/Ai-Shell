package main

import "sync"

// sessionLane 把 Ask 与 RunShell 串到同一条互斥道上，避免：
//
//	Ask 查 Running/shellBusy → RunShell CAS → 两边都通过 → 并发抢主机。
//
// 状态：idle → agent | shell → idle。占坑与释放在同一把锁里完成。
type sessionLane struct {
	mu      sync.Mutex
	mode    string // "" | "agent" | "shell"
	shellID string // shell 占用时的 hostID，供 Stop 精准 Cancel
}

func (l *sessionLane) tryEnterAgent() (ok bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.mode {
	case "agent":
		return false, "已有会话正在运行"
	case "shell":
		return false, "人工 shell 命令正在执行或等待批准，请先结束再问 Agent"
	}
	l.mode = "agent"
	return true, ""
}

func (l *sessionLane) leaveAgent() {
	l.mu.Lock()
	if l.mode == "agent" {
		l.mode = ""
	}
	l.mu.Unlock()
}

func (l *sessionLane) tryEnterShell(hostID string) (ok bool, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.mode {
	case "agent":
		return false, "Agent 正在运行，请先结束本轮再敲命令"
	case "shell":
		return false, "已有 shell 命令在执行或等待批准"
	}
	l.mode = "shell"
	l.shellID = hostID
	return true, ""
}

func (l *sessionLane) leaveShell() {
	l.mu.Lock()
	if l.mode == "shell" {
		l.mode = ""
		l.shellID = ""
	}
	l.mu.Unlock()
}

func (l *sessionLane) shellBusy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode == "shell"
}

func (l *sessionLane) shellHostID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mode != "shell" {
		return ""
	}
	return l.shellID
}
