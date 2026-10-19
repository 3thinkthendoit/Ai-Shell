package agent

// 本轮执行计划。
//
// 模型每一步本来可以临时决定下一个工具（ReAct）。这里加一层运行时状态，
// 只约束「稍微复杂」的任务：
//
//   - 单步观测（ls / df / list_dir / system_info …）：无计划也能跑；
//     仅一条 diagnose/lookup 的琐碎计划会被拒绝，避免为 ls 刷卡。
//   - 变更 / 回滚 / 写文件 / 非只读命令 / 基线验收：必须先有 active 计划项，
//     且工具名落在该项的风险类里。
//
// 类到工具名的映射写死在本文件，模型只能选择类，不能自报一份工具名单
// —— 自报名单可以被写成「全部工具」，强制遵循就失效了。
//
// 未完成项可以改写（证据变了，后面的步骤就该变）。已完成和已放弃的项
// 冻结：否则模型可以把做过的事改写成另一套说法。

import (
	"encoding/json"
	"fmt"
	"strings"

	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

const (
	toolRevisePlan = "revise_plan"

	classDiagnose PlanClass = "diagnose"
	classChange   PlanClass = "change"
	classRollback PlanClass = "rollback"
	classLookup   PlanClass = "lookup"

	planPending = "pending"
	planActive  = "active"
	planDone    = "done"
	planDropped = "dropped"

	maxPlanItems = 8
)

// PlanClass 是运行时规定的风险类。
type PlanClass string

type planItem struct {
	ID         string
	Intent     string
	Class      PlanClass
	Status     string
	DropReason string
	ToolRan    bool
}

// runPlan 是一轮 Run 里的计划。不进会话历史：下一轮用户提问重新建计划。
type runPlan struct {
	items      []planItem
	revisions  int
	failures   int
	maxRevise  int
	lastReason string
}

// PlanEvent 推给界面的计划快照。
type PlanEvent struct {
	Reason string          `json:"reason"`
	Items  []PlanEventItem `json:"items"`
}

// PlanEventItem 是界面上的一条计划。
type PlanEventItem struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
	Class  string `json:"class"`
	Status string `json:"status"`
}

func newRunPlan(maxSteps int) runPlan {
	n := maxSteps * 2
	if n < 8 {
		n = 8
	}
	return runPlan{maxRevise: n}
}

func (p *runPlan) event() PlanEvent {
	ev := PlanEvent{Reason: p.lastReason, Items: make([]PlanEventItem, 0, len(p.items))}
	for _, it := range p.items {
		ev.Items = append(ev.Items, PlanEventItem{
			ID: it.ID, Intent: it.Intent, Class: string(it.Class), Status: it.Status,
		})
	}
	return ev
}

func (p *runPlan) hasOpen() bool {
	for _, it := range p.items {
		if it.Status == planPending || it.Status == planActive {
			return true
		}
	}
	return false
}

// openNote 在模型试图收尾、但仍有未完成项时回喂给模型。
func (p *runPlan) openNote() string {
	var b strings.Builder
	b.WriteString("【计划未收口】还有未完成的计划项，不能给出终局结论。请继续执行活动项，或 revise_plan：把已经跑过工具的项标为 done，把要放弃的项标为 dropped 并写明原因。\n")
	for _, it := range p.items {
		if it.Status == planPending || it.Status == planActive {
			fmt.Fprintf(&b, "- %s [%s/%s] %s\n", it.ID, it.Class, it.Status, it.Intent)
		}
	}
	return b.String()
}

func (p *runPlan) exhausted() bool {
	return p.revisions+p.failures >= p.maxRevise
}

func (p *runPlan) noteToolRan() {
	for i := range p.items {
		if p.items[i].Status == planActive {
			p.items[i].ToolRan = true
			return
		}
	}
}

// allows 判断这个工具能不能执行。
//
// 无活动计划时：只放行观测类工具（见 simpleToolWithoutPlan）；变更/基线/回滚必须先 revise_plan。
// 有活动计划时：工具必须落在活动项的风险类里；诊断类的 run_command 还须命中只读命令库。
// 硬拒绝（凭据路径等）仍交给策略层，以便留下 protected_path 审计。
func (p *runPlan) allows(name, command string, whitelist []string) (bool, string) {
	ro := planReadonlyWhitelist(whitelist)
	active := p.active()
	if active == nil {
		if simpleToolWithoutPlan(name, command, ro) {
			return true, ""
		}
		return false, "这项操作需要先提交执行计划。请调用 revise_plan，标出一条 status=active 的 change/rollback 项（可附带诊断步骤），然后再调用该工具。单步观测（ls、df、list_dir、system_info）不必先计划。"
	}
	if !classAllows(active.Class, name) {
		return false, fmt.Sprintf("活动项 %q 的类别是 %s，不能调用 %s。若证据表明需要换一类工具，先 revise_plan 改写未完成项。", active.ID, active.Class, name)
	}
	if name == "run_command" && active.Class == classDiagnose {
		if policy.HighRiskReason(command) != "" || !policy.ReadOnlyCommand(command, ro) {
			return false, "诊断项只允许只读命令库里的 run_command。请改用只读诊断工具，或 revise_plan 把活动项改为 change。"
		}
	}
	return true, ""
}

// planReadonlyWhitelist：分流/计划共用的只读命令库 = 内置 DefaultWhitelist + 用户白名单扩展。
// 不用「仅用户当前白名单」——清空策略白名单时 ls 仍须能免计划执行。
func planReadonlyWhitelist(user []string) []string {
	wl := vault.DefaultWhitelist()
	if len(user) == 0 {
		return wl
	}
	return append(wl, user...)
}

// simpleToolWithoutPlan：无计划时允许的观测工具（不写基线、不验收、不改系统）。
// snapshot/diff/verify 与变更闭环绑定，必须先有计划。
func simpleToolWithoutPlan(name, command string, readonlyWL []string) bool {
	switch name {
	case "list_hosts", "read_console", "system_info", "collect_diagnostics", "journal_query", "systemd_status",
		"net_listen", "read_file", "list_dir", "list_file_backups", "json_query",
		"fetch_webpage", "search_web":
		return true
	case "run_command":
		return policy.HighRiskReason(command) == "" && policy.ReadOnlyCommand(command, readonlyWL)
	default:
		return false
	}
}

func (p *runPlan) active() *planItem {
	for i := range p.items {
		if p.items[i].Status == planActive {
			return &p.items[i]
		}
	}
	return nil
}

type reviseBody struct {
	Reason string       `json:"reason"`
	Items  []reviseItem `json:"items"`
}

type reviseItem struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
	Class  string `json:"class"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// revise 用模型提交的完整清单替换未完成项。成功返回给模型的摘要。
// 失败不改计划。成功与失败都计入修订预算，避免空转。
func (p *runPlan) revise(raw string) (string, bool) {
	if p.exhausted() {
		return "修订次数已用尽。请基于当前计划收尾，不要再调用 revise_plan。", false
	}
	var body reviseBody
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		p.failures++
		return "revise_plan 参数不是合法 JSON。", false
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		p.failures++
		return "revise_plan 必须给出 reason，说明这次为什么改计划。", false
	}
	if len(body.Items) == 0 {
		p.failures++
		return "revise_plan 的 items 不能为空。要放弃全部未完成项，把它们标为 dropped，不要提交空清单。", false
	}
	if len(body.Items) > maxPlanItems {
		p.failures++
		return fmt.Sprintf("计划最多 %d 项。", maxPlanItems), false
	}
	// 拒绝「为 ls 开一张计划」：仅一条未完成的 diagnose/lookup。
	// 多步只读请直接连续调工具，或提交至少两条；变更/回滚用 change/rollback。
	if trivialReadonlyPlan(body.Items) {
		p.failures++
		return "单步观测不必提交执行计划。请直接调用 list_dir / run_command / system_info 等工具；" +
			"若确需计划，至少两条步骤，或使用 change/rollback 类。", false
	}

	existing := make(map[string]planItem, len(p.items))
	for _, it := range p.items {
		existing[it.ID] = it
	}
	seen := map[string]bool{}
	frozenSeen := map[string]bool{}
	next := make([]planItem, 0, len(body.Items))
	activeN := 0

	for _, in := range body.Items {
		id := strings.TrimSpace(in.ID)
		intent := strings.TrimSpace(in.Intent)
		class := PlanClass(strings.TrimSpace(in.Class))
		status := strings.TrimSpace(in.Status)
		if !validPlanID(id) || intent == "" || !validPlanClass(class) || !validPlanStatus(status) {
			p.failures++
			return "每一项都需要 id、intent、class（diagnose|change|rollback|lookup）、status（active|pending|done|dropped）。", false
		}
		intent = truncateRunes(intent, 240)
		if seen[id] {
			p.failures++
			return "计划项 id 重复: " + id, false
		}
		seen[id] = true
		if status == planActive {
			activeN++
		}

		old, ok := existing[id]
		frozen := ok && (old.Status == planDone || old.Status == planDropped)
		if frozen {
			if intent != old.Intent || class != old.Class || status != old.Status {
				p.failures++
				return "已完成或已放弃的项不能改写: " + id, false
			}
			frozenSeen[id] = true
			next = append(next, old)
			continue
		}
		if status == planDone {
			if !ok || !old.ToolRan || old.Intent != intent || old.Class != class {
				p.failures++
				return "只有已经执行过工具、且意图与类别都没改的项才能标为 done: " + id, false
			}
			old.Status = planDone
			next = append(next, old)
			continue
		}
		if status == planDropped {
			why := strings.TrimSpace(in.Reason)
			if why == "" {
				why = reason
			}
			if ok && (old.Intent != intent || old.Class != class) {
				p.failures++
				return "放弃一项时不能同时改写它的意图或类别: " + id, false
			}
			item := planItem{ID: id, Intent: intent, Class: class, Status: planDropped, DropReason: why}
			if ok {
				item.ToolRan = old.ToolRan
			}
			next = append(next, item)
			continue
		}
		item := planItem{ID: id, Intent: intent, Class: class, Status: status}
		// 意图或类别变了，原先那次执行不再算数，否则可以改完类别再标 done。
		if ok && old.Intent == intent && old.Class == class {
			item.ToolRan = old.ToolRan
		}
		next = append(next, item)
	}
	if activeN > 1 {
		p.failures++
		return "同时只能有一条 status=active 的计划项。", false
	}
	for _, old := range p.items {
		if (old.Status == planDone || old.Status == planDropped) && !frozenSeen[old.ID] {
			p.failures++
			return "已完成或已放弃的项必须原样保留: " + old.ID, false
		}
	}

	p.items = next
	p.revisions++
	p.lastReason = reason
	return p.summary(), true
}

func (p *runPlan) summary() string {
	var b strings.Builder
	b.WriteString("计划已更新。\n")
	for _, it := range p.items {
		fmt.Fprintf(&b, "- %s [%s/%s] %s\n", it.ID, it.Class, it.Status, it.Intent)
	}
	b.WriteString("接下来只执行 status=active 那一项允许的工具。要换类或改后续步骤，再次调用 revise_plan。")
	return b.String()
}

func validPlanID(id string) bool {
	if id == "" || len(id) > 40 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validPlanClass(c PlanClass) bool {
	switch c {
	case classDiagnose, classChange, classRollback, classLookup:
		return true
	default:
		return false
	}
}

func validPlanStatus(s string) bool {
	switch s {
	case planPending, planActive, planDone, planDropped:
		return true
	default:
		return false
	}
}

// trivialReadonlyPlan：整份清单只有一条未完成的 diagnose/lookup。
// 那就是「为一次 ls 开计划」——拒绝，逼模型直接调观测工具。
func trivialReadonlyPlan(items []reviseItem) bool {
	if len(items) != 1 {
		return false
	}
	it := items[0]
	status := strings.TrimSpace(it.Status)
	if status != planActive && status != planPending {
		return false
	}
	switch PlanClass(strings.TrimSpace(it.Class)) {
	case classDiagnose, classLookup:
		return true
	default:
		return false
	}
}

func classAllows(class PlanClass, name string) bool {
	switch class {
	case classDiagnose:
		switch name {
		case "list_hosts", "read_console", "system_info", "collect_diagnostics", "journal_query", "systemd_status",
			"net_listen", "snapshot_state", "diff_state", "verify_checks",
			"read_file", "list_dir", "list_file_backups", "json_query", "run_command":
			return true
		default:
			return false
		}
	case classChange:
		switch name {
		case "run_command", "write_file", "upload_local_file", "download_remote_file",
			"snapshot_state", "diff_state", "verify_checks", "read_console":
			return true
		default:
			return false
		}
	case classRollback:
		switch name {
		case "list_file_backups", "restore_file_backup", "diff_state", "verify_checks", "read_console":
			return true
		default:
			return false
		}
	case classLookup:
		switch name {
		case "list_hosts", "fetch_webpage", "search_web", "read_console":
			return true
		default:
			return false
		}
	default:
		return false
	}
}
