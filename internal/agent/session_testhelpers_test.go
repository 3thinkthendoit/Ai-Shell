package agent

import (
	"context"

	"ai-shell/internal/llm"
)

// ---- 会话分区的测试便利函数 ----
//
// 绝大多数用例不关心「哪条会话」，只关心 agent 的行为本身
// （工具循环、审批、裁剪、压缩……）。这三个短名字把「默认会话」
// 那层参数收起来，让用例读起来仍然是「在这台主机上跑一轮」，
// 而不是每处都写一遍会话 ID。
//
// 关心会话分区本身的用例（session_admin_test.go、session_store_test.go
// 里的多会话部分）**不要**用它们 —— 那些用例要断言的正是
// 「哪条会话收到了什么」，绕开会话参数就等于绕开了被测对象。
//
// 低频调用（ClearSession / CompactSession / sessionSummaryFor）刻意
// 没有对应包装：它们总共二十来处，直接写参数更省事，也让
// 「这里显式指定了会话」这件事留在调用点上。

func runDefault(ag *Agent, ctx context.Context, hostID, prompt string) error {
	return ag.Run(ctx, hostID, DefaultSessionID, prompt)
}

func histDefault(ag *Agent, hostID string) []llm.Message {
	return ag.historyFor(hostID, DefaultSessionID)
}

func rememberDefault(ag *Agent, hostID string, turn []llm.Message, l sessionLimits) {
	ag.remember(hostID, DefaultSessionID, turn, l)
}
