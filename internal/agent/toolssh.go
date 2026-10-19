package agent

// 走 SSH 的工具：原有的六个，全部从三处硬编码 switch 迁移到这里。
//
// 每个工具都是一个 Tool 值，同时携带 Schema、策略评估与执行。
// 迁移时**刻意逐条搬注释**：那些注释记录的是踩过的坑
// （required 空数组被网关 400、system_info 的空命令误判、
// protectedProbe 还原等价命令），删掉它们等于让下一个改这里的人
// 重新踩一遍。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ai-shell/internal/llm"
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// ---- JSON Schema 构造辅助 ----

// objSchema 构造一个 function 参数的 JSON Schema。
//
// required 必须**省略**而不是给空数组：无参数的工具（如 list_hosts）
// 原本会生成 "required": []。这看着无害，实则部分严格校验的
// OpenAI 兼容网关会直接以 400 / Invalid request parameters 拒掉
// 整个请求 —— 表现是「设置页测试能过（不带 tools），控制台一问就 400」，
// 极难自行定位。省略字段后谁都挑不出毛病。
func objSchema(props map[string]any, required ...string) map[string]any {
	o := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

// hostIDSchema 是 host_id 参数的 Schema。
//
// 描述里刻意声明「无法从中获取任何登录信息」：模型有时会试图从 host_id
// 反推地址再自己 ssh 上去。说清楚它只是个不透明标识，比让它试了再失败省事。
func hostIDSchema() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "目标主机的 id，来自 list_hosts 的返回值。注意：这是一个不透明标识，无法从中获取任何登录信息。",
	}
}

// strSchema / intSchema 是字符串与整数参数的简写。
func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// def 是 Tool 的简写构造：只给 Schema 的工具（本地工具）用。
func def(name, desc string, props map[string]any, required ...string) llm.Tool {
	return llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  objSchema(props, required...),
		},
	}
}

// ---- list_hosts ----

var toolListHosts = Tool{
	Name:  "list_hosts",
	Local: true,
	Def: def(
		"list_hosts",
		"列出当前已配置的 Linux 主机（只返回 id/名称/地址/用户名，不含任何密钥）。开始任何排查前先调用它确认可用主机。",
		map[string]any{},
	),
	// 本地元信息工具：不过策略引擎。它只回吐 vault 里已配置的主机清单，
	// 而那些字段（id/name/addr/user/authMethod）本来就不含任何密钥 ——
	// 让用户点「主机管理」看到的东西与模型看到的东西完全一致。
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		return policy.Verdict{Decision: policy.Allow, Reason: "本地元信息，无需策略裁决", Rule: "local"}
	},
	Run: func(ctx toolCtx, _ map[string]any) (sshclient.Result, error) {
		hosts := ctx.v.ListHosts()
		if len(hosts) == 0 {
			return sshclient.Result{Stdout: "当前没有任何已配置的主机。请提示用户先在「主机管理」里添加。", ExitCode: 0}, nil
		}
		var sb strings.Builder
		sb.WriteString("已配置的主机（注意：你只能通过 id 引用它们，无法获得任何登录凭据）：\n")
		for _, h := range hosts {
			sb.WriteString(fmt.Sprintf("- id=%s  name=%s  addr=%s  user=%s  auth=%s\n",
				h.ID, h.Name, h.Addr, h.User, h.AuthMethod))
		}
		return sshclient.Result{Stdout: sb.String(), ExitCode: 0}, nil
	},
}

// ---- system_info ----

var toolSystemInfo = Tool{
	Name: "system_info",
	Def: def(
		"system_info",
		"一次性采集系统概览：发行版、内核、负载、内存、磁盘、失败的服务单元、占用最高的进程、监听端口。排查故障时优先调用。",
		map[string]any{"host_id": hostIDSchema()},
		"host_id",
	),
	Evaluate: evalReadTool,
	Run: func(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
		return ctx.ssh.SystemInfo(strArg(args, "host_id"))
	},
}

// ---- run_command ----

var toolRunCommand = Tool{
	Name: "run_command",
	Def: def(
		"run_command",
		"在目标主机上执行一条 shell 命令并返回 stdout/stderr/退出码。"+
			"只读诊断命令可直接提出；写操作、服务重启、安装软件等变更类命令需要用户逐条批准。"+
			"注意：读取登录凭据（/etc/shadow、~/.ssh/id_*、.aws/credentials 等）与破坏性命令会被硬拒绝。"+
			"不要加 -i/--interactive：本通道没有终端，交互确认会挂死整轮；删除用 rm -f -- 路径。",
		map[string]any{
			"host_id":     hostIDSchema(),
			"command":     strSchema("要执行的完整 shell 命令"),
			"timeout_sec": intSchema("超时秒数，默认 60"),
		},
		"host_id", "command",
	),
	Evaluate: func(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
		v := policy.Evaluate(view.Command, policy.Mode(pol.Mode), pol.Whitelist)
		view.Risk = policy.Risk(view.Command)
		return v
	},
	Run: func(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
		cmd := strArg(args, "command")
		if strings.TrimSpace(cmd) == "" {
			return sshclient.Result{}, fmt.Errorf("命令为空")
		}
		timeout := time.Duration(numArg(args, "timeout_sec", 60)) * time.Second
		return ctx.ssh.Exec(strArg(args, "host_id"), cmd, timeout)
	},
}

// ---- read_file ----

var toolReadFile = Tool{
	Name: "read_file",
	Def: def(
		"read_file",
		"读取目标主机上的文本文件内容（默认最多 256KB）。用于查看配置文件与日志。",
		map[string]any{
			"host_id":   hostIDSchema(),
			"path":      strSchema("绝对路径，如 /etc/nginx/nginx.conf"),
			"max_bytes": intSchema("最多读取的字节数"),
		},
		"host_id", "path",
	),
	Evaluate: evalReadTool,
	Run: func(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
		p := strArg(args, "path")
		if p == "" {
			return sshclient.Result{}, fmt.Errorf("path 为空")
		}
		return ctx.ssh.ReadFile(strArg(args, "host_id"), p, numArg(args, "max_bytes", 256*1024))
	},
}

// ---- list_dir ----

var toolListDir = Tool{
	Name: "list_dir",
	Def: def(
		"list_dir",
		"列出目标主机上某个目录的详细内容。",
		map[string]any{
			"host_id": hostIDSchema(),
			"path":    strSchema("目录绝对路径"),
		},
		"host_id", "path",
	),
	Evaluate: evalReadTool,
	Run: func(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
		p := strArg(args, "path")
		if p == "" {
			p = "."
		}
		return ctx.ssh.ListDir(strArg(args, "host_id"), p)
	},
}

// ---- write_file ----

var toolWriteFile = Tool{
	Name: "write_file",
	Def: def(
		"write_file",
		"覆盖写入目标主机上的文件（会自动备份原文件）。此操作必须经用户人工批准。",
		map[string]any{
			"host_id": hostIDSchema(),
			"path":    strSchema("文件绝对路径"),
			"content": strSchema("要写入的完整内容"),
		},
		"host_id", "path", "content",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		v := policy.EvaluateWrite(view.Command)
		// 写操作的风险标签是固定的：策略引擎已经为它单独开了一条
		// 「永远 Confirm」的入口（EvaluateWrite），白名单也拦不住它，
		// 所以这里不需要再按命令内容细分。
		view.Risk = "write"
		return v
	},
	Run: func(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
		p := strArg(args, "path")
		if p == "" {
			return sshclient.Result{}, fmt.Errorf("path 为空")
		}
		content := strArg(args, "content")
		return ctx.ssh.WriteFile(strArg(args, "host_id"), p, content)
	},
}

// ---- 共用的策略评估 ----

// evalReadTool 是三个只读工具（system_info / read_file / list_dir）的共用裁决。
//
// 这类工具本身是只读的，但路径仍须受凭据保护规则约束 —— 做法是把工具
// 调用**还原成一条等价命令**（protectedProbe），让 /etc/shadow、
// ~/.ssh/id_rsa 的防护规则对没有 command 参数的工具同样生效。
//
// 注意 system_info 没有路径参数：此时 EvaluateProtected 返回 Allow，
// 由下面按模式决定是否需要人工确认。（早期版本会把空串判成「空命令」
// 直接拒绝，导致这个最常用的诊断工具完全不可用。）
func evalReadTool(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
	v := policy.EvaluateProtected(protectedProbe(view.Name, view.Command))
	if v.Decision != policy.Allow {
		view.Risk = policy.Risk(view.Command)
		return v
	}
	if policy.Mode(pol.Mode) == policy.ModeManual {
		v = policy.Verdict{
			Decision: policy.Confirm,
			Reason:   "手动模式：读取操作需人工确认",
			Rule:     "manual",
		}
	} else {
		v = policy.Verdict{
			Decision: policy.Allow,
			Reason:   "只读工具，白名单模式下自动放行",
			Rule:     "read_only",
		}
	}
	view.Risk = policy.Risk(view.Command)
	return v
}

// protectedProbe 把读类工具还原成一条等价命令，交给统一策略判定，
// 这样 /etc/shadow、~/.ssh/id_rsa 之类的保护规则对 read_file 同样生效。
func protectedProbe(tool, path string) string {
	if path == "" {
		return ""
	}
	switch tool {
	case "read_file", "list_file_backups", "restore_file_backup",
		"diff_state", "snapshot_state":
		return "cat " + path
	case "list_dir":
		return "ls " + path
	}
	return ""
}

// ---- 参数读取辅助 ----

// strArg 取一个字符串参数；缺失或类型不对时返回空串。
//
// 刻意不报错：参数缺失交给每个工具自己的校验（它们给出的错误信息
// 更具体，如「path 为空」），LLM 收到的是「怎么改参数」而不是
// 泛泛的「参数类型错误」。
func strArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// numArg 取一个整数参数。
//
// JSON 数字在 Go 里一律是 float64（encoding/json 的行为），所以这里
// 按 float64 断言而不是 int。写成 int 断言会永远走默认值分支，
// 表现为「设了 timeout_sec 完全没生效」。
func numArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	}
	return def
}

// jsonArgs 把工具调用的参数串解成 map。解析失败时返回空 map 而不是错误：
// 调用方会用各自的必填校验给出更具体的提示。
func jsonArgs(raw string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]any{}
	}
	return args
}
