package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"ai-shell/internal/llm"
	"ai-shell/internal/vault"
)

// token 估算与上下文窗口预算。
//
// 为什么是「估算」而不是精确值：OpenAI 兼容协议里 prompt_tokens 只在响应的
// usage 字段里，而①流式响应常不带 usage（部分服务端只在非流式时给）；
// ②本项目的请求以流式为主；③真正需要预警的时刻是「发请求之前」——
// 那时还没有任何 usage 可读。所以只能本地估。
//
// 估算口径：英文约 4 字符 1 token，中文约 1.5 字符 1 token（BPE 对汉字的
// 切分比英文碎）。按字符类型分别计数再加一点协议固定开销，比「总字符 / 4」
// 在中文场景下准得多 —— 后者会把中文低估 2~3 倍，预警等于没有。
//
// 宁可高估不要低估：高估只会让预警提前一点，低估则会在真的爆窗口时
// 服务端才报错，界面只能显示一条看不懂的 400。

// perMessageOverhead 是每条消息的固定协议开销（role、分隔符等）。
// OpenAI 的实测经验值约 4 token/条，取 4。
const perMessageOverhead = 4

// requestOverhead 是整次请求的固定开销（tools 定义、system 包裹等）。
// 工具定义占大头：诊断/快照/验收工具加入后约 1400~1800 token。
const requestOverhead = 2000

// EstimateTokens 估算一段文本的 token 数。
//
// 口径见文件头：英文 4 字符/token，中文 1.5 字符/token，其余字符按英文算。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			// 宽字符（CJK、emoji）按 2 个「英文字符」计 —— 它们既是主要体积来源，
			// 也是 BPE 切得最碎的部分。
			if utf8.RuneLen(r) >= 3 {
				other += 2
			} else {
				other++
			}
		}
	}
	// 1.5 字符/token → 除以 1.5；用整数运算避免浮点，等价于 *2/3。
	return ascii/4 + (other*2)/3
}

// estimateRequestTokens 估算一次请求（整条消息链）的输入 token。
func estimateRequestTokens(msgs []llm.Message) int {
	total := requestOverhead
	for _, m := range msgs {
		total += perMessageOverhead
		if len(m.Parts) > 0 {
			for _, p := range m.Parts {
				switch p.Type {
				case "text":
					total += EstimateTokens(p.Text)
				case "image_url":
					// vision 计费因模型而异；给一个偏高的保守估，避免圆环严重低估。
					total += 800
					if p.ImageURL != nil {
						total += len(p.ImageURL.URL) / 100
					}
				}
			}
		} else {
			total += EstimateTokens(m.Content)
		}
		// 工具调用的名字与参数也是 token —— 一轮排查里它往往是最大的一块，
		// 漏掉等于上限失效。
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
		}
	}
	return total
}

// contextWindowFor 返回模型的上下文窗口大小。取不到时返回 0，
// 由调用方决定是否跳过窗口百分比相关的提示。
//
// 匹配按「模型串里含有关键字」做大小写不敏感的包含匹配 —— 服务商在
// 模型名里嵌自己的标识（deepseek-chat、qwen-max、gpt-4o-mini…），
// 不需要维护一份精确的模型清单。未知模型返回 0 而不是猜一个小值：
// 猜错会让「窗口占用 60%」这种提示变成误导，比不显示更糟。
//
// 具体匹配规则见 tier 表与 lookupWindow：更长的 key 优先，
// 于是 gpt-4o 不会被 gpt-4 抢先命中。
func contextWindowFor(model string) int {
	low := strings.ToLower(strings.TrimSpace(model))
	if low == "" {
		return 0
	}
	return lookupWindow(low)
}

// contextWindowOf 优先用方案里用户配置的窗口；未配置（0）再按模型名内置表推断。
func contextWindowOf(v *vault.Vault, profileID, model string) int {
	if v != nil {
		id := strings.TrimSpace(profileID)
		if id != "" {
			if p, ok := v.LLMProfile(id); ok && p.ContextWindow > 0 {
				return p.ContextWindow
			}
		} else {
			for _, p := range v.LLMProfiles() {
				if p.Active && p.ContextWindow > 0 {
					return p.ContextWindow
				}
			}
		}
	}
	return contextWindowFor(model)
}

// tier 是「模型名关键字 → 上下文窗口」的决策表，按窗口大小分档。
//
// 为什么用包含匹配而不是精确模型名：服务商的模型名每天都在多起来
// （gpt-4o-mini、deepseek-chat、Qwen2.5-14B-Instruct…），维护精确清单
// 等于每天追新闻。包含匹配覆盖整个系列，漏网时返回 0（见上）。
var tier = []struct {
	key    string
	window int
}{
	// 100万级
	{"qwen-long", 1_000_000},
	{"gemini", 1_000_000},
	{"glm-4-plus", 1_000_000},
	// 200K 级
	{"claude", 200_000},
	{"yi-34b", 200_000},
	{"step-1", 200_000},
	{"kimi-k", 200_000},
	// 128K 级
	{"gpt-4o", 128_000},
	{"gpt-4.1", 128_000},
	{"gpt-5", 128_000},
	{"gpt-oss", 128_000},
	// gpt-4-turbo 必须是 128K，而它的 model 串里没有 "gpt-4o" ——
	// 只靠 gpt-4 匹配会落到 8K（差 16 倍，提示会反向误导）。必须单列。
	{"gpt-4-turbo", 128_000},
	{"gpt-4o-mini", 128_000},
	{"kimi", 128_000},
	{"moonshot", 128_000},
	{"hunyuan", 131_072},
	{"sensenova", 128_000},
	{"abab6.5", 131_072},
	{"abab6", 131_072},
	{"ernie-4", 128_000},
	{"qwen-max", 131_072},
	{"qwen-plus", 131_072},
	{"qwen-turbo", 131_072},
	{"qwen2.5", 131_072},
	{"qwen3", 131_072},
	{"qwen1.5", 32_768},
	{"glm-4", 131_072},
	{"glm-3", 131_072},
	{"llama-3.1", 131_072},
	{"llama-3.3", 131_072},
	{"mistral-large", 131_072},
	{"command-r", 131_072},
	// 64K 级
	{"deepseek", 65_536},
	{"deepseek-flash", 65_536},
	{"yi-large", 16_385},
	{"qwen-coder", 131_072},
	// 32K 级
	{"qwen2", 32_768},
	{"qwen", 32_768},
	{"phi-3", 131_072},
	{"phi", 16_385},
	// 16K / 8K 级（老模型与本地小模型）
	{"gpt-3.5", 16_385},
	{"gpt-4", 8_192},
	{"llama-3", 8_192},
	{"llama", 8_192},
	{"gemma", 8_192},
	{"mistral", 32_768},
	{"mixtral", 32_768},
}

func lookupWindow(model string) int {
	low := strings.ToLower(model)
	// 先扫更具体的（key 更长），避免 gpt-4 抢在 gpt-4o 之前命中。
	best := 0
	bestLen := 0
	for _, t := range tier {
		if strings.Contains(low, t.key) && len(t.key) > bestLen {
			best, bestLen = t.window, len(t.key)
		}
	}
	return best
}

// contextNote 生成「上下文用量」提示，用于随工具结果一起回给 LLM，
// 以及在控制台上显示。
//
// 为什么回给 LLM：它不知道自己的上下文还剩多少，于是会在一轮里塞进
// 第 4、第 5 条 `journalctl`，直到服务端返回 400 才停。提前告诉它
// 「已用 82%」，它自己会收敛，或主动建议压缩上下文。
//
// 窗口未知时只报 token 数，不报百分比 —— 没有分母的百分比是编的。
func contextNote(msgs []llm.Message, model string) string {
	tokens := estimateRequestTokens(msgs)
	win := contextWindowFor(model)
	if win <= 0 {
		return fmt.Sprintf("【上下文】本次请求约 %s tokens（未能识别该模型的上下文窗口，无法给出占比）。", fmtInt(tokens))
	}
	pct := tokens * 100 / win
	level := "正常"
	switch {
	case pct >= 95:
		level = "即将超出"
	case pct >= 80:
		level = "偏高"
	case pct >= 60:
		level = "过半"
	}
	return fmt.Sprintf("【上下文】本次请求约 %s tokens，占模型窗口（%s）的 %d%%，%s。"+
		"接近或超过上限时服务端会直接报错 —— 请减少单次命令的输出量，或建议用户压缩/清空会话上下文。",
		fmtInt(tokens), fmtInt(win), pct, level)
}

// fmtInt 给数字加千分位分隔符：128000 → 128,000。
func fmtInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}
