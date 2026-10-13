package agent

import (
	"strings"
	"testing"

	"ai-shell/internal/llm"
)

// 中文必须比「字符数 / 4」估得高。
//
// 这是整个估算的意义所在：BPE 对汉字的切分比英文碎得多（约 1.5 字符 1 token，
// 而英文是 4 字符 1 token）。用同一套口径会把中文低估 2~3 倍 ——
// 于是「窗口占用 40%」的提示在真实的 90% 面前等于没有，用户照旧撞 400。
func TestEstimateTokensCountsCJKDenserThanASCII(t *testing.T) {
	en := EstimateTokens(strings.Repeat("a", 400))
	zh := EstimateTokens(strings.Repeat("中", 400))

	if en != 100 {
		t.Errorf("英文应约 4 字符/token，实得 %d", en)
	}
	if zh <= en {
		t.Errorf("同等字符数下中文的 token 应明显更多，实得 %d vs %d", zh, en)
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("空串应为 0，实得 %d", got)
	}
}

// 工具调用的名字与参数必须算进去。
//
// 一轮排查里它往往是最大的一块（JSON 参数 + 命令原文）。漏掉它，
// 多轮工具调用后的用量会被严重低估 —— 而那正是最需要预警的时刻。
func TestEstimateRequestTokensCountsToolCalls(t *testing.T) {
	plain := []llm.Message{{Role: "user", Content: "看看负载"}}
	withTool := []llm.Message{
		{Role: "user", Content: "看看负载"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID:       "c1",
			Function: llm.FunctionCall{Name: "run_command", Arguments: `{"command":"` + strings.Repeat("x", 2000) + `"}`},
		}}},
	}

	if got := estimateRequestTokens(withTool); got <= estimateRequestTokens(plain) {
		t.Errorf("工具调用应显著增加估算，实得 %d vs %d", got, estimateRequestTokens(plain))
	}
}

// 请求开销必须计入：工具定义（toolDefs）约 700~900 token，
// 漏掉它等于每轮都少算一整块固定成本。
func TestEstimateRequestTokensIncludesOverhead(t *testing.T) {
	if got := estimateRequestTokens(nil); got != requestOverhead {
		t.Errorf("空请求也应计入固定开销 %d，实得 %d", requestOverhead, got)
	}
}

// 窗口表：已知模型要认得出，且更具体的 key 优先。
//
// 「gpt-4 抢在 gpt-4o 之前命中」是这份表最容易踩的坑 —— 两者窗口差 16 倍，
// 猜错方向会让「占用 90%」显示成「占用 6%」，用户于是继续加命令直到撞墙。
func TestContextWindowFor(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-4o-mini", 128_000}, // 必须走 gpt-4o 而不是 gpt-4
		{"gpt-4o", 128_000},      //
		{"gpt-4", 8_192},         //
		{"gpt-4-turbo", 128_000}, //
		{"deepseek-chat", 65_536},
		{"deepseek-reasoner", 65_536},
		{"qwen-max", 131_072},
		{"Qwen2.5-14B-Instruct", 131_072}, // 大小写不敏感
		{"qwen2:14b", 32_768},             // 本地 Ollama：qwen2 不是 qwen2.5
		{"claude-sonnet-4-5", 200_000},
		{"gemini-2.0-flash", 1_000_000},
		{"my-private-model", 0}, // 认不出 → 0，调用方据此不显示百分比
		{"", 0},
	}
	for _, c := range cases {
		if got := contextWindowFor(c.model); got != c.want {
			t.Errorf("contextWindowFor(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// 窗口未知时不能编百分比 —— 没有分母的百分比比不显示更糟。
func TestContextNoteWithoutWindowOmitsPercentage(t *testing.T) {
	note := contextNote([]llm.Message{{Role: "user", Content: "hi"}}, "unknown-model")
	if strings.Contains(note, "%") {
		t.Errorf("窗口未知时不该出现百分比，实得: %s", note)
	}
	if !strings.Contains(note, "未能识别") {
		t.Errorf("应说明「未能识别窗口」，实得: %s", note)
	}
}

func TestContextNoteWithWindowReportsPercentage(t *testing.T) {
	// 4000 个 ASCII 字符 ≈ 1000 tokens，占 gpt-4o（128K）不到 1%。
	note := contextNote([]llm.Message{{Role: "user", Content: strings.Repeat("a", 4000)}}, "gpt-4o")
	if !strings.Contains(note, "占模型窗口") {
		t.Errorf("已知窗口应给出占比，实得: %s", note)
	}
	if !strings.Contains(note, "128,000") {
		t.Errorf("应带千分位的窗口值，实得: %s", note)
	}
}

// 分档阈值：接近上限时必须升级措辞，让 LLM 与用户都知道该收手了。
func TestContextNoteLevels(t *testing.T) {
	// 造一条占 gpt-4o（128K）约 90% 的消息链：约 115K tokens ≈ 46 万 ASCII 字符。
	big := strings.Repeat("a", 460_000)
	note := contextNote([]llm.Message{{Role: "user", Content: big}}, "gpt-4o")
	if !strings.Contains(note, "偏高") {
		t.Errorf("约九成占用应标记为「偏高」，实得: %s", note)
	}
}

func TestFmtInt(t *testing.T) {
	cases := map[string]int{
		"0":         0,
		"999":       999,
		"1,000":     1000,
		"12,345":    12345,
		"128,000":   128000,
		"1,000,000": 1_000_000,
	}
	for want, n := range cases {
		if got := fmtInt(n); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", n, got, want)
		}
	}
}
