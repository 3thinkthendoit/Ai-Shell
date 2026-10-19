package agent

// revise_plan：本轮执行计划的提交与改写。
//
// 它是本地工具（不碰主机、不碰凭证），但走注册表：LLM 才能在 tool_calls
// 里看到它。真正改写逻辑在 runPlan.revise；这里只做 Schema 与放行。

import (
	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

var toolRevisePlanDef = Tool{
	Name:  toolRevisePlan,
	Local: true,
	Def: def(
		toolRevisePlan,
		"提交或改写本轮执行计划。变更、回滚、snapshot/diff/verify 之前必须有一条 status=active 的项；"+
			"单步观测（ls / df / list_dir / system_info）不要调用本工具——仅一条 diagnose/lookup 会被拒绝；"+
			"多步只读可直接连调工具。证据变化时改写未完成项（已完成/已放弃不可改）。"+
			"class 只能是 diagnose|change|rollback|lookup，运行时按类决定允许哪些工具，不能自报工具名单。",
		map[string]any{
			"reason": strSchema("这次提交/改写计划的原因（必填）"),
			"items": map[string]any{
				"type": "array",
				"description": "完整计划清单。同时最多一条 active。" +
					"done 只能标在已经执行过工具、且意图与类别都未改的项上；dropped 必须能说清原因。",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":     strSchema("稳定短 id，如 p1"),
						"intent": strSchema("这一步要做什么（意图，不是具体命令）"),
						"class": map[string]any{
							"type":        "string",
							"description": "diagnose|change|rollback|lookup",
						},
						"status": map[string]any{
							"type":        "string",
							"description": "active|pending|done|dropped",
						},
						"reason": strSchema("status=dropped 时的放弃原因；其它状态可省略"),
					},
					"required": []string{"id", "intent", "class", "status"},
				},
			},
		},
		"reason", "items",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		view.Risk = "low"
		return policy.Verdict{Decision: policy.Allow, Reason: "计划修订", Rule: "plan"}
	},
	// Run 不会被 executeTool 走到：Agent.Run 在分发前拦截 revise_plan。
	// 仍提供一个实现，避免注册表完整性测试失败。
	Run: func(_ toolCtx, _ map[string]any) (sshclient.Result, error) {
		return sshclient.Result{Stdout: "revise_plan 由运行时直接处理"}, nil
	},
}
