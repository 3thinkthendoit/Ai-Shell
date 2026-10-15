package agent

// 运维变更闭环工具：基线快照、前后 diff、验收检查、备份列举与回滚。
//
// 设计约束：
//   - 快照落在远端 /tmp/aishell-snapshots/，便于同一主机上前后对比；
//   - verify_checks 只用结构化探针（service/port/file/http），不开放任意 shell；
//   - restore_file_backup 走 EvaluateWrite，永远需人工确认。

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"ai-shell/internal/policy"
	"ai-shell/internal/sshclient"
	"ai-shell/internal/vault"
)

const snapshotDir = "/tmp/aishell-snapshots"

// ---- snapshot_state ----

var toolSnapshotState = Tool{
	Name: "snapshot_state",
	Def: def(
		"snapshot_state",
		"采集主机状态基线并保存到远端快照目录，返回 snapshot_id 与内容摘要。"+
			"变更前先拍一张，变更后再拍或用 diff_state 对比。"+
			"scopes 逗号分隔：os,load,memory,disk,failed_units,top_procs,listening,unit；默认常用集合。",
		map[string]any{
			"host_id": hostIDSchema(),
			"scopes":  strSchema("可选：要采集的段，逗号分隔；默认 os,load,memory,disk,failed_units,listening"),
			"unit":    strSchema("可选：当 scopes 含 unit 时指定 unit 名"),
			"paths":   strSchema("可选：额外纳入快照的文件绝对路径，逗号分隔，最多 5 个；只记录 sha256 与前 2KB"),
		},
		"host_id",
	),
	Evaluate: evalSnapshotTool,
	Run:      runSnapshotState,
}

func evalSnapshotTool(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
	args := jsonArgs(view.Args)
	if paths := strArg(args, "paths"); paths != "" {
		for _, p := range splitComma(paths) {
			if err := validateSafeAbsPath(p); err != nil {
				return policy.Verdict{Decision: policy.Deny, Reason: err.Error(), Rule: "bad_path"}
			}
			if v := policy.EvaluateProtected("cat " + p); v.Decision != policy.Allow {
				view.Risk = policy.Risk("cat " + p)
				return v
			}
		}
	}
	return evalReadTool(view, pol)
}

func runSnapshotState(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	scopes := parseScopes(strArg(args, "scopes"))
	unit, err := optionalUnit(strArg(args, "unit"))
	if err != nil {
		return sshclient.Result{}, err
	}
	paths, err := sanitizePaths(strArg(args, "paths"), 5)
	if err != nil {
		return sshclient.Result{}, err
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	script := snapshotCaptureScript(id, scopes, unit, paths)
	res, err := ctx.ssh.Exec(hostID, script, 90*time.Second)
	if err != nil {
		return res, err
	}
	// 给 LLM 一眼能看到的定位信息。
	prefix := fmt.Sprintf("snapshot_id=%s\nsnapshot_path=%s/%s.snap\n\n", id, snapshotDir, id)
	res.Stdout = prefix + res.Stdout
	return res, nil
}

func snapshotCaptureScript(id string, scopes []string, unit string, paths []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`mkdir -p %s && id=%s && f=%s/$id.snap && `,
		shellQuote(snapshotDir), shellQuote(id), shellQuote(snapshotDir)))
	b.WriteString(`{ `)
	b.WriteString(fmt.Sprintf(`echo "# AISHELL_SNAPSHOT id=$id time=$(date -Is 2>/dev/null || date) scopes=%s"; `,
		strings.Join(scopes, ",")))
	for _, sc := range scopes {
		b.WriteString(`echo "## ` + sc + `"; `)
		b.WriteString(scopeBody(sc, unit))
		b.WriteString(`; `)
	}
	for i, p := range paths {
		b.WriteString(fmt.Sprintf(`echo "## file_%d path=%s"; `, i, shellQuote(p)))
		b.WriteString(fmt.Sprintf(
			`if [ -e %s ]; then sha256sum -- %s 2>/dev/null || shasum -a 256 -- %s 2>/dev/null; wc -c < %s 2>/dev/null; head -c 2048 -- %s 2>/dev/null; echo; else echo "(missing)"; fi; `,
			shellQuote(p), shellQuote(p), shellQuote(p), shellQuote(p), shellQuote(p)))
	}
	b.WriteString(`} > "$f" 2>&1; echo "saved=$f"; echo; cat -- "$f"`)
	return b.String()
}

func scopeBody(scope, unit string) string {
	switch scope {
	case "os":
		return `cat /etc/os-release 2>/dev/null | head -6; uname -a`
	case "load":
		return `uptime; nproc 2>/dev/null`
	case "memory":
		return `free -h 2>/dev/null`
	case "disk":
		return `df -hT 2>/dev/null | head -16`
	case "failed_units":
		return `systemctl --failed --no-pager 2>/dev/null | head -30`
	case "top_procs":
		return `ps -eo pid,comm,%cpu,%mem --sort=-%mem 2>/dev/null | head -10`
	case "listening":
		return `(ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | head -30`
	case "unit":
		if unit == "" {
			return `echo "(no unit specified)"`
		}
		q := shellQuote(unit)
		return `systemctl is-active ` + q + ` 2>&1; systemctl status --no-pager -l ` + q + ` 2>&1 | head -40`
	default:
		return `echo "(unknown scope)"`
	}
}

func parseScopes(raw string) []string {
	defaults := []string{"os", "load", "memory", "disk", "failed_units", "listening"}
	if strings.TrimSpace(raw) == "" {
		return defaults
	}
	allowed := map[string]bool{
		"os": true, "load": true, "memory": true, "disk": true,
		"failed_units": true, "top_procs": true, "listening": true, "unit": true,
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range splitComma(raw) {
		p = strings.ToLower(strings.TrimSpace(p))
		if !allowed[p] || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

// ---- diff_state ----

var toolDiffState = Tool{
	Name: "diff_state",
	Def: def(
		"diff_state",
		"对比两份状态快照。before 填 snapshot_id 或 "+snapshotDir+" 下的 .snap 绝对路径；"+
			"after 可省略（省略则按 scopes 参数或默认集合再采一份当前状态再 diff）。"+
			"用于变更前后对比。",
		map[string]any{
			"host_id": hostIDSchema(),
			"before":  strSchema("基线 snapshot_id 或 " + snapshotDir + " 下的 .snap 路径"),
			"after":   strSchema("可选：另一份 snapshot_id 或路径；省略则现场再拍一张"),
			"scopes":  strSchema("可选：after 省略时再采快照用的 scopes，默认常用集合"),
			"unit":    strSchema("可选：scopes 含 unit 时的 unit 名"),
		},
		"host_id", "before",
	),
	Evaluate: evalDiffState,
	Run:      runDiffState,
}

func evalDiffState(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
	args := jsonArgs(view.Args)
	for _, key := range []string{"before", "after"} {
		ref := strings.TrimSpace(strArg(args, key))
		if ref == "" {
			continue
		}
		p, err := resolveSnapshotPath(ref)
		if err != nil {
			return policy.Verdict{Decision: policy.Deny, Reason: err.Error(), Rule: "bad_snapshot"}
		}
		if v := policy.EvaluateProtected("cat " + p); v.Decision != policy.Allow {
			view.Risk = policy.Risk("cat " + p)
			return v
		}
	}
	return evalReadTool(view, pol)
}

func runDiffState(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	before := strings.TrimSpace(strArg(args, "before"))
	after := strings.TrimSpace(strArg(args, "after"))
	if hostID == "" || before == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 与 before 都不能为空")
	}
	beforePath, err := resolveSnapshotPath(before)
	if err != nil {
		return sshclient.Result{}, err
	}

	afterPath := ""
	if after == "" {
		// 再采一份当前状态：scopes 只来自本机白名单参数，绝不解析远端文件内容进 shell。
		scopes := parseScopes(strArg(args, "scopes"))
		unit, err := optionalUnit(strArg(args, "unit"))
		if err != nil {
			return sshclient.Result{}, err
		}
		id := fmt.Sprintf("current-%d", time.Now().UnixNano())
		afterPath = path.Join(snapshotDir, id+".snap")
		capScript := snapshotCaptureScript(id, scopes, unit, nil)
		if _, err := ctx.ssh.Exec(hostID, capScript, 90*time.Second); err != nil {
			return sshclient.Result{}, err
		}
	} else {
		afterPath, err = resolveSnapshotPath(after)
		if err != nil {
			return sshclient.Result{}, err
		}
	}

	script := fmt.Sprintf(
		`bf=%s; af=%s; `+
			`if [ ! -f "$bf" ]; then echo "基线快照不存在: $bf"; exit 2; fi; `+
			`if [ ! -f "$af" ]; then echo "对比快照不存在: $af"; exit 2; fi; `+
			`echo "before=$bf"; echo "after=$af"; echo; `+
			`if command -v diff >/dev/null 2>&1; then diff -u "$bf" "$af" || true; else `+
			`echo "(no diff binary)"; echo "---- BEFORE ----"; cat "$bf"; echo "---- AFTER ----"; cat "$af"; fi`,
		shellQuote(beforePath), shellQuote(afterPath),
	)
	return ctx.ssh.Exec(hostID, script, 90*time.Second)
}

// resolveSnapshotPath 只接受 snapshot_id，或 Clean 后位于 snapshotDir 下的 .snap 路径。
func resolveSnapshotPath(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("快照引用为空")
	}
	if strings.HasPrefix(ref, "/") {
		clean := path.Clean(ref)
		if clean != snapshotDir && !strings.HasPrefix(clean, snapshotDir+"/") {
			return "", fmt.Errorf("快照路径必须位于 %s 下", snapshotDir)
		}
		if !strings.HasSuffix(clean, ".snap") {
			return "", fmt.Errorf("快照文件必须以 .snap 结尾")
		}
		return clean, nil
	}
	for _, r := range ref {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' || r == '_' {
			continue
		}
		return "", fmt.Errorf("snapshot_id 含非法字符")
	}
	if len(ref) > 128 {
		return "", fmt.Errorf("snapshot_id 过长")
	}
	return path.Join(snapshotDir, ref+".snap"), nil
}

// ---- verify_checks ----

var toolVerifyChecks = Tool{
	Name: "verify_checks",
	Def: def(
		"verify_checks",
		"按清单验收变更是否生效。checks 为 JSON 数组，每项含 type 与参数。"+
			"支持类型：service（unit 是否 active）、port（端口是否在听）、"+
			"file_exists、file_contains（固定子串）、http（本机 URL，仅 http/https 到 localhost/127.0.0.1）。"+
			"全部通过时退出码 0，否则非 0，并给出每项 PASS/FAIL。",
		map[string]any{
			"host_id": hostIDSchema(),
			"checks":  strSchema(`JSON 数组，例：[{"type":"service","unit":"nginx"},{"type":"port","port":80},{"type":"file_contains","path":"/etc/nginx/nginx.conf","text":"worker_processes"}]`),
		},
		"host_id", "checks",
	),
	Evaluate: evalVerifyChecks,
	Run:      runVerifyChecks,
}

type verifyCheck struct {
	Type string `json:"type"`
	Unit string `json:"unit"`
	Port int    `json:"port"`
	Path string `json:"path"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

func evalVerifyChecks(view *ToolCallView, pol vault.PolicySettings) policy.Verdict {
	args := jsonArgs(view.Args)
	checks, err := parseVerifyChecks(checksArg(args))
	if err != nil {
		// 参数错误交给 Run 返回可改的提示，避免被模型理解成「策略拒绝」。
		view.Command = "verify(invalid)"
		return evalReadTool(view, pol)
	}
	for _, c := range checks {
		if c.Path != "" {
			if v := policy.EvaluateProtected("cat " + c.Path); v.Decision != policy.Allow {
				view.Risk = policy.Risk("cat " + c.Path)
				return v
			}
		}
	}
	view.Command = fmt.Sprintf("%d checks", len(checks))
	return evalReadTool(view, pol)
}

func runVerifyChecks(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	if hostID == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 为空")
	}
	checks, err := parseVerifyChecks(checksArg(args))
	if err != nil {
		return sshclient.Result{}, err
	}
	script, err := verifyScript(checks)
	if err != nil {
		return sshclient.Result{}, err
	}
	return ctx.ssh.Exec(hostID, script, 90*time.Second)
}

func parseVerifyChecks(raw string) ([]verifyCheck, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("checks 为空")
	}
	var checks []verifyCheck
	if err := json.Unmarshal([]byte(raw), &checks); err != nil {
		return nil, fmt.Errorf("checks 不是合法 JSON 数组: %w", err)
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("checks 至少需要 1 项")
	}
	if len(checks) > 20 {
		return nil, fmt.Errorf("checks 最多 20 项")
	}
	for i := range checks {
		checks[i].Type = strings.ToLower(strings.TrimSpace(checks[i].Type))
		switch checks[i].Type {
		case "service":
			checks[i].Unit = sanitizeUnit(checks[i].Unit)
			if checks[i].Unit == "" {
				return nil, fmt.Errorf("checks[%d]: service 需要合法 unit", i)
			}
		case "port":
			if checks[i].Port <= 0 || checks[i].Port > 65535 {
				return nil, fmt.Errorf("checks[%d]: port 需要 1-65535", i)
			}
		case "file_exists", "file_contains":
			p := strings.TrimSpace(checks[i].Path)
			if err := validateSafeAbsPath(p); err != nil {
				return nil, fmt.Errorf("checks[%d]: %v", i, err)
			}
			checks[i].Path = path.Clean(p)
			if checks[i].Type == "file_contains" {
				if checks[i].Text == "" || strings.ContainsAny(checks[i].Text, "\n\r") {
					return nil, fmt.Errorf("checks[%d]: file_contains 需要单行 text", i)
				}
			}
		case "http":
			u := strings.TrimSpace(checks[i].URL)
			if !isLocalHTTPURL(u) {
				return nil, fmt.Errorf("checks[%d]: http 仅允许 http(s)://127.0.0.1 或 localhost", i)
			}
			checks[i].URL = u
		default:
			return nil, fmt.Errorf("checks[%d]: 未知 type %q（支持 service/port/file_exists/file_contains/http）", i, checks[i].Type)
		}
	}
	return checks, nil
}

func isLocalHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func verifyScript(checks []verifyCheck) (string, error) {
	var b strings.Builder
	b.WriteString(`fail=0; `)
	for i, c := range checks {
		label := fmt.Sprintf("check[%d] %s", i, c.Type)
		switch c.Type {
		case "service":
			b.WriteString(fmt.Sprintf(
				`u=%s; if [ "$(systemctl is-active "$u" 2>/dev/null)" = "active" ]; then echo "PASS %s unit=$u"; else echo "FAIL %s unit=$u active=$(systemctl is-active "$u" 2>&1)"; fail=1; fi; `,
				shellQuote(c.Unit), label, label))
		case "port":
			b.WriteString(fmt.Sprintf(
				`p=%d; if (ss -tulpn 2>/dev/null || netstat -tulpn 2>/dev/null) | grep -Eq ":%d[[:space:]]|:%d$"; then echo "PASS %s port=$p"; else echo "FAIL %s port=$p not listening"; fail=1; fi; `,
				c.Port, c.Port, c.Port, label, label))
		case "file_exists":
			b.WriteString(fmt.Sprintf(
				`f=%s; if [ -e "$f" ]; then echo "PASS %s path=$f"; else echo "FAIL %s path=$f missing"; fail=1; fi; `,
				shellQuote(c.Path), label, label))
		case "file_contains":
			b.WriteString(fmt.Sprintf(
				`f=%s; t=%s; if [ -f "$f" ] && grep -Fq -- "$t" "$f"; then echo "PASS %s path=$f"; else echo "FAIL %s path=$f missing text"; fail=1; fi; `,
				shellQuote(c.Path), shellQuote(c.Text), label, label))
		case "http":
			b.WriteString(fmt.Sprintf(
				`u=%s; code=$(curl -sS -o /dev/null -w "%%{http_code}" --max-time 5 "$u" 2>/dev/null || echo 000); `+
					`case "$code" in 2*|3*) echo "PASS %s url=$u code=$code";; *) echo "FAIL %s url=$u code=$code"; fail=1;; esac; `,
				shellQuote(c.URL), label, label))
		}
	}
	b.WriteString(`echo; if [ "$fail" -eq 0 ]; then echo "VERIFY_RESULT=PASS"; exit 0; else echo "VERIFY_RESULT=FAIL"; exit 1; fi`)
	return b.String(), nil
}

// ---- list_file_backups / restore_file_backup ----

var toolListFileBackups = Tool{
	Name: "list_file_backups",
	Def: def(
		"list_file_backups",
		"列出 write_file / upload_local_file 自动生成的备份（path.bak.时间戳.pid）。"+
			"变更失败需要回滚时先用它找到备份路径。",
		map[string]any{
			"host_id": hostIDSchema(),
			"path":    strSchema("原文件绝对路径"),
		},
		"host_id", "path",
	),
	Evaluate: evalReadTool,
	Run:      runListFileBackups,
}

func runListFileBackups(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	p := strings.TrimSpace(strArg(args, "path"))
	if hostID == "" || p == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 与 path 都不能为空")
	}
	if err := validateSafeAbsPath(p); err != nil {
		return sshclient.Result{}, err
	}
	p = path.Clean(p)
	script := fmt.Sprintf(
		`p=%s; echo "target=$p"; `+
			`ls -lt -- "$p".bak.* 2>/dev/null | head -30 || { echo "(no backups)"; exit 0; }`,
		shellQuote(p),
	)
	return ctx.ssh.Exec(hostID, script, 30*time.Second)
}

var toolRestoreFileBackup = Tool{
	Name: "restore_file_backup",
	Def: def(
		"restore_file_backup",
		"把备份文件恢复到原路径（会先再备份当前文件）。必须经用户人工批准。"+
			"backup 可省略：省略时使用最新一份 path.bak.*。",
		map[string]any{
			"host_id": hostIDSchema(),
			"path":    strSchema("要恢复的目标文件绝对路径"),
			"backup":  strSchema("可选：备份文件绝对路径；省略则取最新一份"),
		},
		"host_id", "path",
	),
	Evaluate: func(view *ToolCallView, _ vault.PolicySettings) policy.Verdict {
		v := policy.EvaluateWrite(view.Command)
		view.Risk = "write"
		return v
	},
	Run: runRestoreFileBackup,
}

func runRestoreFileBackup(ctx toolCtx, args map[string]any) (sshclient.Result, error) {
	hostID := strArg(args, "host_id")
	p := strings.TrimSpace(strArg(args, "path"))
	bak := strings.TrimSpace(strArg(args, "backup"))
	if hostID == "" || p == "" {
		return sshclient.Result{}, fmt.Errorf("host_id 与 path 都不能为空")
	}
	if err := validateSafeAbsPath(p); err != nil {
		return sshclient.Result{}, err
	}
	p = path.Clean(p)
	if bak != "" {
		if err := validateSafeAbsPath(bak); err != nil {
			return sshclient.Result{}, fmt.Errorf("backup: %w", err)
		}
		bak = path.Clean(bak)
		// 备份应是目标文件的 .bak.* 旁路，防止任意读后覆写。
		if !strings.HasPrefix(bak, p+".bak.") {
			return sshclient.Result{}, fmt.Errorf("backup 必须是该文件的自动备份（%s.bak.*）", p)
		}
	}

	var script string
	if bak == "" {
		script = fmt.Sprintf(
			`p=%s; `+
				`bak=$(ls -1t -- "$p".bak.* 2>/dev/null | head -1); `+
				`if [ -z "$bak" ] || [ ! -f "$bak" ]; then echo "未找到备份: $p.bak.*"; exit 2; fi; `+
				`pre="$p.bak.pre-restore.$(date +%%Y%%m%%d-%%H%%M%%S).$$"; `+
				`if [ -e "$p" ]; then cp -a -- "$p" "$pre" || { echo "备份当前文件失败"; exit 90; }; echo "current_saved=$pre"; fi; `+
				`cp -a -- "$bak" "$p" || { echo "恢复失败"; exit 91; }; `+
				`echo "restored_from=$bak"; echo "target=$p"; wc -c < "$p" 2>/dev/null`,
			shellQuote(p),
		)
	} else {
		script = fmt.Sprintf(
			`p=%s; bak=%s; `+
				`if [ ! -f "$bak" ]; then echo "备份不存在: $bak"; exit 2; fi; `+
				`pre="$p.bak.pre-restore.$(date +%%Y%%m%%d-%%H%%M%%S).$$"; `+
				`if [ -e "$p" ]; then cp -a -- "$p" "$pre" || { echo "备份当前文件失败"; exit 90; }; echo "current_saved=$pre"; fi; `+
				`cp -a -- "$bak" "$p" || { echo "恢复失败"; exit 91; }; `+
				`echo "restored_from=$bak"; echo "target=$p"; wc -c < "$p" 2>/dev/null`,
			shellQuote(p), shellQuote(bak),
		)
	}
	return ctx.ssh.Exec(hostID, script, 60*time.Second)
}

// ---- helpers ----


func checksArg(args map[string]any) string {
	v, ok := args["checks"]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func splitComma(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sanitizePaths(raw string, max int) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := splitComma(raw)
	if len(parts) > max {
		return nil, fmt.Errorf("paths 最多 %d 个", max)
	}
	var out []string
	for _, p := range parts {
		if err := validateSafeAbsPath(p); err != nil {
			return nil, err
		}
		out = append(out, path.Clean(p))
	}
	return out, nil
}

// validateSafeAbsPath 只允许安全的远端绝对路径字符，杜绝脚本插值注入。
func validateSafeAbsPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("非法路径: 必须是绝对路径")
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("非法路径: 不得包含 ..")
	}
	if len(p) > 512 {
		return fmt.Errorf("路径过长")
	}
	for _, r := range p {
		if r > unicode.MaxASCII {
			return fmt.Errorf("非法路径: 仅允许 ASCII")
		}
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '/' || r == '.' || r == '_' || r == '-' || r == '+' || r == '=' || r == ',' || r == '@'
		if !ok {
			return fmt.Errorf("非法路径: 含不允许的字符")
		}
	}
	return nil
}

// optionalUnit 对非空 unit 做 sanitize；非法则报错，避免静默丢弃。
func optionalUnit(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u := sanitizeUnit(raw)
	if u == "" {
		return "", fmt.Errorf("unit 非法: %q", raw)
	}
	return u, nil
}
