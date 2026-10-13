package agent

// 新增工具：json_query / fetch_webpage / search_web /
// upload_local_file / download_remote_file。
//
// 这五个都只依赖 toolRegistry() 一个注册点 —— 正是重构前
// 「三处硬编码 switch」想解决的问题。

import (
	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

var toolJSONQuery = Tool{
	Name: "json_query",
	Def: def(
		"json_query",
		"对远端一个 JSON 文件做查询并返回结果。用于从 JSON 配置/API 响应里取出某个字段，"+
			"不必自己拼 grep/awk 命令（那些解释器类命令被安全策略刻意排除）。"+
			"路径语法：.key 取对象字段，[0] 取数组元素，[] 表示整个数组，单独一个 . 表示整个文档。",
		map[string]any{
			"host_id":   hostIDSchema(),
			"path":      strSchema("远端 JSON 文件的绝对路径"),
			"query":     strSchema("点分查询路径，如 .spec.replicas、.items[0].name、."),
			"max_bytes": intSchema("最多读取的字节数，默认 512KB"),
		},
		"host_id", "path", "query",
	),
	Evaluate: evalReadTool,
	Run:      runJSONQuery,
}

var toolFetchWebpage = Tool{
	Name: "fetch_webpage",
	Def: def(
		"fetch_webpage",
		"抓取一个网页的正文内容。",
		map[string]any{
			"url": strSchema("要抓取的完整 URL"),
		},
		"url",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		return policy.Verdict{Decision: policy.Allow, Reason: "只读出网，白名单模式下自动放行", Rule: "read_only"}
	},
	Run: runFetchWebpage,
}

var toolSearchWeb = Tool{
	Name: "search_web",
	Def: def(
		"search_web",
		"搜索互联网，返回相关结果。",
		map[string]any{
			"query": strSchema("搜索关键词"),
		},
		"query",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		return policy.Verdict{Decision: policy.Allow, Reason: "只读出网，白名单模式下自动放行", Rule: "read_only"}
	},
	Run: runSearchWeb,
}

var toolUploadLocalFile = Tool{
	Name: "upload_local_file",
	Def: def(
		"upload_local_file",
		"把**本地机器上的一个文件**上传到远端主机（会先备份远端原文件）。"+
			"注意：local 是运行本应用的那台机器上的路径，不是远端路径。"+
			"本应用的凭证库与审计日志不可上传。此操作必须经用户人工批准。",
		map[string]any{
			"host_id":        hostIDSchema(),
			"path":           strSchema("远端目标文件的绝对路径"),
			"local":          strSchema("本地源文件的绝对路径"),
			"content_base64": strSchema("可选：直接给出文件内容的 base64，用于没有本地文件可读的场景"),
		},
		"host_id", "path",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		v := policy.EvaluateWrite(view.Command)
		view.Risk = "write"
		return v
	},
	Run: runUploadLocalFile,
}

var toolDownloadRemoteFile = Tool{
	Name: "download_remote_file",
	Def: def(
		"download_remote_file",
		"把**远端主机上的一个文件**下载到本地磁盘（运行本应用的那台机器）。"+
			"用于把日志、配置拉到本地细看。上限 32 MiB，超过请改用终端里的 scp/sftp。",
		map[string]any{
			"host_id": hostIDSchema(),
			"path":    strSchema("远端文件的绝对路径"),
			"local":   strSchema("本地保存路径（绝对路径）"),
		},
		"host_id", "path", "local",
	),
	Evaluate: func(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
		return evalReadTool(view, pol)
	},
	Run: runDownloadRemoteFile,
}
