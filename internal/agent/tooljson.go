package agent

// json_query 工具：在 Go 侧做结构化文本处理。
//
// 为什么要有它：白名单**刻意排除**了 awk / find / sed / xargs / eval /
// sh / bash / perl / python（理由见 internal/vault/whitelist.go 的注释）——
// 这类命令不含任何分隔符，段首就是白名单词，元字符闸门也拦不住
// （`awk -f script.awk` 就漏了），在白名单里出现即等于一条
// **无人确认的任意命令执行通道**。
//
// 于是模型遇到「从 JSON 里取个字段」这类需求时原本无路可走，
// 只能退回人工确认一条它自己拼的 shell 命令。这个工具把那类需求
// 收回 Go 侧：数据不出进程，没有解释器，也就没有执行出口。
//
// 安全边界：输入是**不可信数据**（远端文件内容），输出同样按不可信
// 数据处理（调用方统一走 PrepareForLLM）。这里不做任何「清理」——
// 只做解析与投影，绝不 eval。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"ai-shell/internal/sshclient"
)

// jsonQueryMaxDepth 限制递归深度。
//
// 递归下降解析恶意构造的深层嵌套会耗尽栈。Go 的栈会增长到很大
// 而不是立刻崩，表现是内存暴涨；而这里只需要取几个字段，
// 深到 64 层已经完全够用。
const jsonQueryMaxDepth = 64

// runJSONQuery 是 json_query 的执行体。
//
// 流程：读远端文件 → 解析 JSON → 按 query 投影 → 序列化结果。
// 三步都可能失败，而每一种失败的反馈都要能让模型改对参数。
func runJSONQuery(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	path := strArg(args, "path")
	query := strings.TrimSpace(strArg(args, "query"))

	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("缺少 host_id")
	}
	if path == "" {
		return sshclient.Result{}, fmt.Errorf("path 为空")
	}
	if query == "" {
		return sshclient.Result{}, fmt.Errorf("query 为空：请给出要查询的路径，如 .spec.replicas 或 .items[0].name")
	}

	// 读取内容。上限沿用 read_file 的默认 256KB，但允许调用方指定 ——
	// 大 JSON 是常态，256KB 常常只够读到一半。
	res, err := ctx.ssh.ReadFile(hostID, path, numArg(args, "max_bytes", jsonQueryMaxBytes))
	if err != nil {
		return sshclient.Result{}, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("远端退出码 %d", res.ExitCode)
		}
		return sshclient.Result{ExitCode: res.ExitCode, Stderr: msg}, fmt.Errorf("读取文件失败：%s", msg)
	}

	val, err := parseJSONBounded(res.Stdout)
	if err != nil {
		return sshclient.Result{}, fmt.Errorf("%s 不是合法 JSON：%v", path, err)
	}

	out, err := queryJSON(val, query)
	if err != nil {
		return sshclient.Result{}, fmt.Errorf("查询失败：%v", err)
	}

	// 序列化时带缩进：结果常直接回给用户看，而压缩成一行既读不出
	// 结构，也让模型在复述时容易丢字段。
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return sshclient.Result{}, fmt.Errorf("序列化查询结果失败：%v", err)
	}
	return sshclient.Result{
		Stdout:   string(buf) + "\n",
		ExitCode: 0,
	}, nil
}

// jsonQueryMaxBytes 是 json_query 读取文件的上限（512 KiB）。
//
// 比 read_file 的默认值大一倍：JSON 的换行与缩进占比高，
// 同样信息量的文件字节数更大。
const jsonQueryMaxBytes = 512 * 1024

// parseJSONBounded 解析一段 JSON 文本，深层嵌套会被拒绝而不是递归爆栈。
//
// encoding/json 自己对深度有限制（10000），但那个上限仍然足以
// 让一次解析吃掉可观的栈与时间。这里用自定义的深度检查提前拦下。
func parseJSONBounded(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	// 不用 UseNumber：数字按 float64 走会在 64 位整数上丢精度
	// （时间戳、ID 常见），而这里的结果经常原样回显给人看。
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// 尾部还有内容说明这不是单个 JSON 文档（可能是拼接的多段、
	// 或 JSON Lines）。原样接受会静默丢掉后半段，让模型以为看全了。
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("文件包含多个 JSON 值，请先取出其中一个再查询")
	}
	if err := checkDepth(v, 0); err != nil {
		return nil, err
	}
	return v, nil
}

// checkDepth 递归检查嵌套深度。
func checkDepth(v any, depth int) error {
	if depth > jsonQueryMaxDepth {
		return fmt.Errorf("嵌套超过 %d 层，已拒绝解析", jsonQueryMaxDepth)
	}
	switch t := v.(type) {
	case map[string]any:
		for _, e := range t {
			if err := checkDepth(e, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkDepth(e, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// queryJSON 按一条点分路径取出子节点。
//
// 支持的语法（刻意保持极小）：
//
//	.key             取对象的字段
//	.key[0]          取数组元素（下标从 0 起）
//	[0]              根就是数组时直接下标
//	.key[]           取数组的全部元素（投影）
//	.                整个文档（用于「只校验格式并美化输出」）
//
// 刻意不支持：函数调用、通配符、过滤表达式、正则。
// 每多一种语法就多一处可以出错的地方，而模型用点分路径
// 就足以覆盖「取个字段看看」的绝大多数场景；真的需要复杂处理时，
// 它应该把文件读回来在本地分析。
func queryJSON(doc any, query string) (any, error) {
	q := strings.TrimSpace(query)
	if q == "" || q == "." {
		return doc, nil
	}
	// 允许省略最前面的点（.foo 与 foo 等价）—— 模型经常这么写，
	// 为这个报错只会浪费一轮对话。
	q = strings.TrimPrefix(q, ".")

	cur := doc
	for q != "" {
		switch {
		case q[0] == '.':
			q = q[1:]
			continue
		case q[0] == '[':
			end := strings.IndexByte(q, ']')
			if end < 0 {
				return nil, fmt.Errorf("缺少配对的 ]：%q", query)
			}
			inner := strings.TrimSpace(q[1:end])
			q = q[end+1:]
			if inner == "" {
				// `[]` 只对数组有意义：它表示「不要下标，要整个数组」。
				// 对对象使用时给出明确反馈，而不是静默返回对象本身。
				if _, ok := cur.([]any); !ok {
					return nil, fmt.Errorf("[] 只能用于数组，当前节点是 %s", jsonKind(cur))
				}
				continue
			}
			idx, err := strconv.Atoi(inner)
			if err != nil {
				return nil, fmt.Errorf("数组下标必须是整数，实得 %q", inner)
			}
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("对非数组节点用了下标：当前是 %s", jsonKind(cur))
			}
			if idx < 0 || idx >= len(arr) {
				return nil, fmt.Errorf("下标 %d 越界：数组长度 %d", idx, len(arr))
			}
			cur = arr[idx]

		default:
			// 取一段字段名：到下一个 . 或 [ 为止。
			i := 0
			for i < len(q) && q[i] != '.' && q[i] != '[' {
				i++
			}
			key := q[:i]
			q = q[i:]
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("对非对象节点取字段 %q：当前是 %s", key, jsonKind(cur))
			}
			v, ok := obj[key]
			if !ok {
				return nil, fmt.Errorf("字段 %q 不存在（该对象的字段：%s）", key, jsonKeys(obj))
			}
			cur = v
		}
	}
	return cur, nil
}

// jsonKind 返回节点的类型名，用在错误信息里。
func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "对象"
	case []any:
		return "数组"
	case string:
		return "字符串"
	case json.Number:
		return "数字"
	case bool:
		return "布尔"
	case nil:
		return "null"
	}
	return "未知类型"
}

// jsonKeys 列出对象的字段名，让模型知道该取什么。
//
// 上限 20 个：字段很多的对象（如 kubectl 的完整输出）全列出来
// 会把错误信息撑得比结果还长，而模型看到前几个通常就知道结构了。
func jsonKeys(obj map[string]any) string {
	if len(obj) == 0 {
		return "（空对象）"
	}
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	// 排序让输出稳定：map 遍历顺序随机，同一个错误每次字段顺序不同，
	// 会让人以为是不同的错误。
	sortStrings(names)
	if len(names) > 20 {
		names = append(names[:20], fmt.Sprintf("…等 %d 个", len(obj)-20))
	}
	return strings.Join(names, ", ")
}

// sortStrings 是 sort.Strings 的薄封装（避免在本文件引入 sort 只为一处）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
