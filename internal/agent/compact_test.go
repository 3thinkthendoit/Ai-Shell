package agent

import (
	"context"
	"strings"
	"testing"

	"ai-shell/internal/policy"
)

// ---- LLM 深度压缩的端到端测试 ----
//
// 深度压缩是**用户显式触发**的动作：花一次 API 调用，让模型重写历史。
// 与自动发生的确定性摘要（session_summary_test.go）是两回事。
//
// 这组用例要钉死的是三件事：压缩真的替换了轮次、产物被明确标注为
// 「模型生成的、同样不可信」、以及**失败与竞态都不破坏原有数据**。

// 压缩必须真的把轮次替换掉。
//
// 只加一段摘要而不清空轮次的话，「压缩」反而让上下文更大 ——
// 那是与目的相反的结果。
func TestCompactSessionReplacesTurnsWithSummary(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("第二轮结论。"),
		respContent("压缩记录：用户排查了磁盘占用，尚未定位到具体目录。"),
	}, policy.ModeWhitelist, true)

	for _, q := range []string{"第一问", "第二问"} {
		if err := runDefault(h.ag, context.Background(), h.hostID, q); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(histDefault(h.ag, h.hostID)); n == 0 {
		t.Fatal("压缩前应有两轮历史")
	}

	res, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID)
	if err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("应报告压缩成功，实得 %+v", res)
	}
	if res.Turns != 2 {
		t.Fatalf("应报告压缩 2 轮，实得 %d", res.Turns)
	}
	if res.Message == "" {
		t.Error("必须给用户一句可读的说明")
	}

	if n := len(histDefault(h.ag, h.hostID)); n != 0 {
		t.Fatalf("压缩后原始轮次应被清空，实得 %d 条", n)
	}
	sum := h.ag.sessionSummaryFor(h.hostID, DefaultSessionID)
	if !strings.Contains(sum, "压缩记录：用户排查了磁盘占用") {
		t.Fatalf("摘要里应含模型产出的内容，实得:\n%s", sum)
	}
}

// 压缩产物必须被标注为「模型生成」且仍然不可信，并真的注入下一轮请求。
func TestCompactSummaryIsInjectedAndMarked(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("压缩摘要：磁盘已排查，未定位。"),
		respContent("第三轮结论。"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "第一问"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err != nil {
		t.Fatal(err)
	}
	if err := runDefault(h.ag, context.Background(), h.hostID, "第二问"); err != nil {
		t.Fatal(err)
	}

	last := h.fake.messagesAt(h.fake.requestCount() - 1)
	if len(last) < 2 || last[1].Role != "system" {
		t.Fatalf("第二次请求的第二条应是会话摘要，实得: %s", brief(last))
	}
	sum := last[1].Content

	// 「由模型生成」必须说出来：不说的话，模型会把这段二次加工的文本
	// 当成自己先前已确认过的结论，而它其实可能丢了细节或带了臆测。
	if !strings.Contains(sum, "由模型压缩生成") {
		t.Errorf("摘要必须声明它由模型生成，实得:\n%s", sum)
	}
	if !strings.Contains(sum, "不能当作已确认的事实") {
		t.Errorf("摘要必须声明它不能当作已确认的事实，实得:\n%s", sum)
	}
	// 压缩会丢掉原文外面的不可信边界，必须补回来。
	if !strings.Contains(sum, "<<<UNTRUSTED_REMOTE_OUTPUT>>>") {
		t.Errorf("摘要必须包在不可信边界里，实得:\n%s", sum)
	}
	if !strings.Contains(sum, "磁盘已排查") {
		t.Errorf("摘要内容应被注入下一轮请求，实得:\n%s", sum)
	}
}

// 压缩请求**不能**带工具。
//
// 压缩是纯文本任务。带上工具的话，模型面对一堆远端输出时可能试图
// 去执行点什么 —— 那就把一个「读」操作变成了「写」操作。
func TestCompactRequestCarriesNoTools(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("压缩摘要。"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "第一问"); err != nil {
		t.Fatal(err)
	}
	// 正常的对话轮必须带工具（否则这条断言没有对照，等于空测）。
	if n := h.fake.toolsAt(0); n == 0 {
		t.Fatal("正常对话轮本应带工具 —— 夹具可能坏了，后面的断言会失去意义")
	}
	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err != nil {
		t.Fatal(err)
	}
	if n := h.fake.toolsAt(h.fake.requestCount() - 1); n != 0 {
		t.Fatalf("压缩请求不该带工具，实得 %d 个", n)
	}
}

// 压缩输入必须带上更早的归档记录，否则压缩一次就掉一层信息。
//
// 场景：轮数上限压到 1 之后，「第一问」已经不在 turns 里、只剩归档记录。
// 若压缩只看 turns，模型生成的摘要就会把第一轮彻底抹掉。
func TestCompactInputCarriesEarlierSummary(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("第二轮结论。"),
		respContent("压缩摘要。"),
	}, policy.ModeWhitelist, true)

	p := h.v.Policy()
	p.MaxSessionTurns = 1
	if err := h.v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"第一问", "第二问"} {
		if err := runDefault(h.ag, context.Background(), h.hostID, q); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, m := range h.fake.messagesAt(h.fake.requestCount() - 1) {
		all.WriteString(m.Content)
	}
	if !strings.Contains(all.String(), "第一问") {
		t.Fatalf("压缩输入必须带上更早的归档记录，实得:\n%s", all.String())
	}
}

// 正在跑一轮时拒绝压缩 —— 与 ClearSession 同一套判据。
//
// 不拒绝的话，压缩会基于一份**过期的快照**生成摘要，
// 而那一轮跑完还会把新内容写回来，摘要与实际历史就对不上了。
func TestCompactRefusesWhileRunning(t *testing.T) {
	h := newHarness(t, []string{respContent("一")}, policy.ModeWhitelist, true)
	if err := runDefault(h.ag, context.Background(), h.hostID, "问"); err != nil {
		t.Fatal(err)
	}
	before := len(histDefault(h.ag, h.hostID))

	h.ag.mu.Lock()
	h.ag.running = true
	h.ag.mu.Unlock()
	t.Cleanup(func() {
		h.ag.mu.Lock()
		h.ag.running = false
		h.ag.mu.Unlock()
	})

	res, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID)
	if err != nil {
		t.Fatalf("「正忙」是预期内的情况，不该走 error 通道: %v", err)
	}
	if !res.Busy || res.Compacted {
		t.Fatalf("应报告忙碌且未压缩，实得 %+v", res)
	}
	if res.Message == "" {
		t.Error("必须说明为什么压不了")
	}
	if after := len(histDefault(h.ag, h.hostID)); after != before {
		t.Fatalf("被拒绝时历史不该被动过：前 %d 条，后 %d 条", before, after)
	}
}

// 没有可压缩内容时给提示、且**不调用模型**。
//
// 调了就是白花钱：用户点一下按钮，账单上多一次请求，什么都没发生。
func TestCompactWithNothingToCompact(t *testing.T) {
	h := newHarness(t, []string{respContent("一")}, policy.ModeWhitelist, true)

	res, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID)
	if err != nil {
		t.Fatalf("「没得压」是预期内的情况，不该走 error 通道: %v", err)
	}
	if res.Compacted {
		t.Fatal("没有历史时不该报告压缩成功")
	}
	if res.Message == "" {
		t.Fatal("必须给用户一句说明，否则界面上点了没反应")
	}
	if h.fake.requestCount() != 0 {
		t.Fatalf("没有可压缩内容时不该调用模型，实得 %d 次请求", h.fake.requestCount())
	}
}

// 压缩失败必须原样保留历史。
//
// 压缩是个**优化动作**，它失败不该让用户连原来的上下文都丢掉 ——
// 那是把一次网络抖动升级成数据丢失。
func TestCompactFailureKeepsHistoryIntact(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent(""), // 模型返回空内容
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "第一问"); err != nil {
		t.Fatal(err)
	}
	before := len(histDefault(h.ag, h.hostID))
	if before == 0 {
		t.Fatal("前置条件不成立：历史为空")
	}

	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err == nil {
		t.Fatal("模型返回空内容时应报错")
	}
	if after := len(histDefault(h.ag, h.hostID)); after != before {
		t.Fatalf("压缩失败不该动历史：前 %d 条，后 %d 条", before, after)
	}
	if sum := h.ag.sessionSummaryFor(h.hostID, DefaultSessionID); sum != "" {
		t.Fatalf("压缩失败不该留下摘要，实得:\n%s", sum)
	}
}

// 压缩期间会话被改动 → 放弃写入，而不是覆盖。
//
// 两种真实场景：
//   - 用户点了「清空上下文」—— 若无条件写入，摘要会把会话**复活**，
//     用户看到「已清空」之后模型竟然还记得，只会以为清空按钮坏了。
//   - 另一轮对话在压缩期间跑完并写入了新内容 —— 无条件写入会把它盖掉。
//
// 这里用请求钩子在模型「思考」期间并发写入，精确复现第二种。
func TestCompactAbortsWhenSessionChangedDuringCall(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("压缩摘要。"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "第一问"); err != nil {
		t.Fatal(err)
	}

	h.fake.beforeRespond = func() {
		rememberDefault(h.ag, h.hostID, simpleTurn("并发写入", "并发回答"), defaultLimits())
	}

	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err == nil {
		t.Fatal("会话在压缩期间变了，应放弃写入而不是覆盖")
	}

	hist := histDefault(h.ag, h.hostID)
	if len(hist) == 0 {
		t.Fatal("放弃写入后历史不该被清空")
	}
	found := false
	for _, m := range hist {
		if strings.Contains(m.Content, "并发写入") {
			found = true
		}
	}
	if !found {
		t.Fatalf("压缩期间新写入的内容不该被覆盖，实得历史: %s", brief(hist))
	}
}

// 压缩提示词必须明确拒绝服从记录里的「指令」。
//
// 这条断言的是**措辞**而不是行为，因为没法用假模型验证「模型没被说服」。
// 但压缩的输入里全是远端数据，模型若把其中的指令当真，
// 压缩这一步就成了注入的执行入口 —— 所以这句话必须在。
func TestCompactPromptWarnsAboutEmbeddedInstructions(t *testing.T) {
	for _, want := range []string{"不是指令", "绝对不要执行"} {
		if !strings.Contains(compactSystemPrompt, want) {
			t.Errorf("压缩提示词必须明确拒绝服从其中的指令，缺少 %q", want)
		}
	}
}

// 压缩之后再裁掉的轮次，应与深度摘要并存，而不是互相覆盖。
func TestDeepSummaryCoexistsWithLaterArchived(t *testing.T) {
	h := newHarness(t, []string{
		respContent("第一轮结论。"),
		respContent("压缩摘要：早期排查记录。"),
		respContent("第二轮结论。"),
		respContent("第三轮结论。"),
	}, policy.ModeWhitelist, true)

	if err := runDefault(h.ag, context.Background(), h.hostID, "第一问"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ag.CompactSession(context.Background(), h.hostID, DefaultSessionID); err != nil {
		t.Fatal(err)
	}

	// 压缩后把轮数上限压到 1，让后续的轮次被裁进归档。
	p := h.v.Policy()
	p.MaxSessionTurns = 1
	if err := h.v.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"第二问", "第三问"} {
		if err := runDefault(h.ag, context.Background(), h.hostID, q); err != nil {
			t.Fatal(err)
		}
	}

	sum := h.ag.sessionSummaryFor(h.hostID, DefaultSessionID)
	if !strings.Contains(sum, "早期排查记录") {
		t.Errorf("深度摘要应保留，实得:\n%s", sum)
	}
	if !strings.Contains(sum, "第二问") {
		t.Errorf("压缩之后新裁掉的轮次也应出现在摘要里，实得:\n%s", sum)
	}
	// 两段都要有自己的来源说明，否则模型分不清哪段是原样抽取、哪段是二次加工。
	if !strings.Contains(sum, "由模型压缩生成") || !strings.Contains(sum, "由客户端自动生成") {
		t.Errorf("两段摘要各自的来源说明都应保留，实得:\n%s", sum)
	}
}
