package agent

// 工具注册表。
//
// 为什么要有它：在它之前，新增一个工具必须**同步手改三处** ——
// toolDefs（LLM 看到的 Schema）、executeTool 里的策略 switch、
// dispatch 里的执行 switch。三处都是硬编码，而漏改任何一处的后果
// **不是编译失败，而是静默失效**：LLM 能声明、能调用，但路由不进
// 执行分支，用户看到的是「工具调用了但什么都没发生」。
//
// 注册表把这三处合成一处声明：每个工具在一个 Tool 值里同时给出
// Schema、策略评估函数与执行函数，注册即同时获得三份能力。
// 新增工具 = 在 toolRegistry() 里加一个条目，没有别的地方要改。

import (
	"net/http"
	"strings"
	"time"

	"ai-shell/internal/llm"
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

// Tool 是一个暴露给 LLM 的工具的完整声明。
//
// 三个字段一一对应原先那三处 switch，缺一个就少一份能力：
//   - Def：LLM 看到的 JSON Schema（原 toolDefs）；
//   - Evaluate：策略裁决（原 executeTool 的策略 switch）；
//   - Run：真正执行（原 dispatch）。
type Tool struct {
	// Name 是工具名，也是 LLM 在 function.name 里填的字符串。
	Name string

	// Def 是给 LLM 的工具声明。
	Def llm.Tool

	// Local 为真表示这个工具不需要主机、也不接触凭证 —— 它会在
	// 跨主机闸门**之前**短路执行（原 executeTool 里那个
	// `switch tc.Function.Name { case "list_hosts": … }`）。
	Local bool

	// Evaluate 对一次调用做策略裁决，返回 Allow / Confirm / Deny。
	//
	// 入参里的 view 已填好 ID / Name / Args / HostID / HostName / Command，
	// 评估函数可以读取它们（Command 对无命令工具已回落为 path），
	// 也可以回写 view.Risk 之类的展示字段。
	Evaluate func(view *ToolCallView, pol vault.PolicySettings) policy.Verdict

	// Run 真正执行，返回的 Result 由调用方统一 formatResult + 脱敏 + 审计。
	//
	// 返回 error 表示「这次调用没跑成」（参数缺失、连接失败等），
	// 与 Result.ExitCode != 0 是两回事 —— 后者是远端命令自己失败了，
	// 输出照样要回传给 LLM。
	Run func(ctx toolCtx, args map[string]any) (sshclient.Result, error)
}

// toolCtx 是工具执行时能拿到的运行时依赖。
//
// 之所以要传这个而不是让每个工具直接持有 Agent：Agent 上大部分字段
// 是会话状态（sessions / approvals / running），暴露给工具等于把
// 内部状态敞开。这里只给工具**真正需要的**那几个。
type toolCtx struct {
	// ssh 是远端执行通道。
	ssh sshRunner

	// v 用于读配置（如 list_hosts 要列主机、搜索工具要读 API Key）。
	v *vault.Vault

	// emit 让工具自己发事件（如文件传输进度）。
	emit Emitter

	// http 是出网客户端，nil 表示该 Agent 没有配出网能力。
	http httpDoer
}

// sshRunner 是工具用到的 sshclient 能力子集。抽成接口是为了让测试
// 能换一个假的，也让「哪些工具真的碰远端」一目了然。
type sshRunner interface {
	Exec(hostID, cmd string, timeout time.Duration) (sshclient.Result, error)
	ReadFile(hostID, path string, maxBytes int) (sshclient.Result, error)
	ListDir(hostID, path string) (sshclient.Result, error)
	WriteFile(hostID, path, content string) (sshclient.Result, error)
	SystemInfo(hostID string) (sshclient.Result, error)
	WriteFileBinary(hostID, path string, data []byte) (sshclient.Result, error)
	ReadFileChunk(hostID, path string, offset, size int64) ([]byte, error)
	StatFileSize(hostID, path string) (int64, error)
}

// httpDoer 是出网请求的能力子集（*http.Client 天然满足它）。
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// toolRegistry 返回本 Agent 支持的全部工具，顺序即 LLM 看到的顺序。
//
// 新增工具只改这里。顺序不是随意的：先元信息、再只读诊断、后变更类 ——
// 与系统提示里「先读后写」的叙述一致，也让模型在选工具时有个自然的优先级。
func toolRegistry() []Tool {
	return []Tool{
		toolListHosts,
		toolSystemInfo,
		toolCollectDiagnostics,
		toolJournalQuery,
		toolSystemdStatus,
		toolNetListen,
		toolSnapshotState,
		toolDiffState,
		toolVerifyChecks,
		toolRunCommand,
		toolReadFile,
		toolListDir,
		toolJSONQuery,
		toolWriteFile,
		toolListFileBackups,
		toolRestoreFileBackup,
		toolFetchWebpage,
		toolSearchWeb,
		toolUploadLocalFile,
		toolDownloadRemoteFile,
	}
}

// lookupTool 按名字取工具定义，第二个返回值为 false 表示没有这个工具。
//
// 每次调用都重建表而不是缓存：注册表只有十余项，而 Agent.Run 每一步
// 最多调它几十次；为一个 O(n) 的线性查找加缓存与加锁，收益是负的。
func lookupTool(name string) (Tool, bool) {
	for _, t := range toolRegistry() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// toolDefsFor 生成发给 LLM 的工具声明列表。
//
// allowCrossHost 会动态改写 host_id 的描述：工具描述与系统提示必须说
// 同一套跨主机规则 —— 两处不一致时部分模型会优先信工具描述，
// 于是用户在策略里禁掉了跨主机，模型却照样往别的主机发命令。
//
// 实现上**从注册表现算一遍描述**，而不是把 Def 拷出来再改：
// Def 里的 properties / host_id 都是注册表共享的 map，就地改写会让
// 后缀一次次累积（第一次调用后描述里就多一句话，第二次多两句），
// 且污染后面每一次请求 —— 那不只是难看，冗余描述会持续吃掉上下文，
// 而症状要到用户注意到「描述怎么变长了」才会被发现。
func toolDefsFor(allowCrossHost bool) []llm.Tool {
	reg := toolRegistry()
	out := make([]llm.Tool, 0, len(reg))
	for _, t := range reg {
		out = append(out, t.Def)
	}
	if allowCrossHost {
		return out
	}
	for i := range out {
		fn := &out[i].Function
		props, _ := fn.Parameters["properties"].(map[string]any)
		if props == nil {
			continue
		}
		h, ok := props["host_id"].(map[string]any)
		if !ok {
			// 没有 host_id 的工具（本地工具）不参与改写。
			continue
		}
		desc, _ := h["description"].(string)
		if desc == "" || strings.HasSuffix(desc, hostIDCrossHostSuffix) {
			// 已经带过后缀就不重复加。走到这里说明注册表本身
			// 就带着后缀（不该发生），加第二次只会更糟。
			continue
		}
		// 换一个新 map 而不是改原来的：props 与 h 都是注册表共享的。
		nh := make(map[string]any, len(h)+1)
		for k, v := range h {
			nh[k] = v
		}
		nh["description"] = desc + hostIDCrossHostSuffix
		np := make(map[string]any, len(props))
		for k, v := range props {
			np[k] = v
		}
		np["host_id"] = nh
		// 必须保留原来的 required：objSchema 只在显式传参时才写这个键，
		// 而这里只拿到了 properties。丢了它，host_id 就从必填变成可选，
		// 模型于是会发出不带 host_id 的调用，再由各工具报「缺少 host_id」
		//—— 一次本可避免的失败往返。
		params := map[string]any{
			"type":       "object",
			"properties": np,
		}
		if req, ok := fn.Parameters["required"]; ok {
			params["required"] = req
		}
		fn.Parameters = params
	}
	return out
}

// hostIDCrossHostSuffix 是禁止跨主机时追加到 host_id 描述后面的说明。
//
// 写成常量而不是在定义处拼接：定义处（toolssh.go）拿不到策略开关，
// 而这里拿得到。分成「基础描述 + 开关决定的后缀」两半，
// 正是为了让同一份定义能服务两种策略。
const hostIDCrossHostSuffix = "当前策略禁止跨主机执行：只能填当前会话所属的主机 id。"
