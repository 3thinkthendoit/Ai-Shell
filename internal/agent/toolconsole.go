package agent

// read_console：读取本任务常驻控制台的可见屏幕（VT 模型），不是再跑一条命令。
//
// 用户在终端里敲的 ./start.sh 报错不会自动进完整对话历史；模型看到
// 「解释刚才的错误」时应主动调本工具取屏，而不是凭更早一轮闲聊瞎猜。
// 失败命令只会自动塞一句短提示（见 InjectConsoleFailureHint），完整内容靠这里。

import (
	"fmt"
	"strings"

	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// consoleReader 读某任务常驻终端的可见屏幕。由 App 接线（持有 termVT）；
// 测试可换假实现。nil 表示本进程没有控制台管线（工具应明确报错）。
type consoleReader interface {
	ReadConsole(hostID, sessionID string, maxLines int) (string, error)
}

const (
	consoleDefaultLines = 80
	consoleMaxLines     = 200
)

var toolReadConsole = Tool{
	Name: "read_console",
	Def: def(
		"read_console",
		"读取当前任务控制台（交互终端）的纯文本，用于查看用户刚敲过的命令与输出。"+
			"用户说「刚才的错误」「上面报错了」「解释一下终端输出」时优先调用本工具，不要靠对话记忆猜。"+
			"这不是在远端再执行命令。终端进程刚结束时仍可读到退出前保留的最后一屏；用户主动关闭终端后该画面会被丢掉，此时应提示用户先打开终端。",
		map[string]any{
			"host_id": hostIDSchema(),
			"lines":   intSchema("从屏幕底部向上取多少行，默认 80，最大 200"),
		},
		"host_id",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		return policy.Verdict{Decision: policy.Allow, Reason: "读取本机已打开的控制台画面", Rule: "console_read"}
	},
	Run: runReadConsole,
}

func runReadConsole(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	if ctx.console == nil {
		return sshclient.Result{}, fmt.Errorf("当前环境没有控制台管线，无法读取屏幕")
	}
	hostID := strArg(args, "host_id")
	if hostID == "" {
		hostID = ctx.sessionHostID
	}
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	// 默认读本轮会话的那块表面；跨会话不靠猜。
	sessionID := ctx.sessionID
	if sessionID == "" {
		sessionID = DefaultSessionID
	}
	lines := numArg(args, "lines", consoleDefaultLines)
	if lines < 1 {
		lines = consoleDefaultLines
	}
	if lines > consoleMaxLines {
		lines = consoleMaxLines
	}
	text, err := ctx.console.ReadConsole(hostID, sessionID, lines)
	if err != nil {
		return sshclient.Result{}, err
	}
	text = strings.TrimRight(text, " \t\n")
	if text == "" {
		return sshclient.Result{
			Stdout: "控制台屏幕当前为空（可能刚打开、或输出已被清屏）。",
		}, nil
	}
	return sshclient.Result{Stdout: text}, nil
}
