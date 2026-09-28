// Package policy 是命令执行的「刹车」。所有由 LLM 发起的工具调用，以及
// 交互式会话里人直接敲下的 shell 命令，都必须先过这里。它负责：
//
//  1. 硬拒绝：读取 Linux 登录配置/密钥（/etc/shadow、~/.ssh/id_* …）——这是需求 4 的强制边界；
//  2. 硬拒绝：不可逆的破坏性命令（mkfs、dd of=/dev/*、rm -rf / …）；
//  3. LLM 路径（Evaluate）：手动模式一律确认；白名单模式命中只读命令库才自动执行；
//     高危变更（删/建/改等）即使命中白名单也需确认。
//  4. 人工 shell 路径（EvaluateHuman）：仅高危需确认，其余自动放行；
//     白名单在这里是「已知命令库」，给未知第三方做存在性检查用，不是放行闸门。
//
// 注意：交互终端（PTY）不走这里 —— 那是完整伪终端，人是所有者，界面会明确提示风险。
package policy

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Mode 与 vault.PolicyMode 取值一致（用字符串避免包循环依赖）。
type Mode string

const (
	ModeManual    Mode = "manual"
	ModeWhitelist Mode = "whitelist"
)

// Decision 是策略裁决结果。
type Decision string

const (
	Allow   Decision = "allow"   // 直接执行
	Confirm Decision = "confirm" // 挂起，等人工批准
	Deny    Decision = "deny"    // 拒绝，不执行
)

// Verdict 是一次裁决的完整结果。
type Verdict struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason"`
	Rule     string   `json:"rule"`
}

func (v Verdict) String() string {
	return fmt.Sprintf("%s (%s)", v.Decision, v.Rule)
}

// protectedPaths 命中即拒绝：这些路径承载 Linux 的登录凭据与密钥。
// 需求 4 要求 LLM 绝对不允许读取它们，因此无论什么模式都硬拒绝。
//
// 设计取舍：按「目录级」而非「文件名级」拦截（整个 ~/.ssh、~/.aws…），
// 因为文件名级规则很容易被通配符绕过（cat /etc/sha*、cat ~/.ssh/id_*）。
var protectedPaths = []*regexp.Regexp{
	// 账户与提权数据库（含 glob 截断形式，如 /etc/sha*、/etc/gsh*）
	regexp.MustCompile(`(?i)/etc/g?shadow\b`),
	regexp.MustCompile(`(?i)/etc/g?sh\w*\*`),
	regexp.MustCompile(`(?i)/etc/sudoers\b`),
	regexp.MustCompile(`(?i)/etc/sudo\w*\*`),
	// SSH 主机私钥
	regexp.MustCompile(`(?i)ssh_host_\w*_key`),
	// 整个凭据目录 —— 目录级拦截，杜绝通配符绕过
	regexp.MustCompile(`(?i)(?:^|[\s'"=(~/])\.ssh(?:/|\b)`),
	regexp.MustCompile(`(?i)(?:^|[\s'"=(~/])\.(?:aws|kube|docker|gnupg)(?:/|\b)`),
	regexp.MustCompile(`(?i)(?:^|[\s'"=(~/])\.(?:netrc|pgpass|my\.cnf|pypirc|git-credentials|npmrc)\b`),
	// 进程环境变量
	regexp.MustCompile(`(?i)/proc/(?:\d+|self)/environ\b`),
	// 私钥文件名（任何位置出现都拒绝）
	regexp.MustCompile(`(?i)\bid_(?:rsa|dsa|ecdsa|ed25519)\b`),
	regexp.MustCompile(`(?i)(?:^|[\s'"=(~/])\S*\.pem\b`),
	// 读取类命令 + 敏感关键词 —— 覆盖变量拼接等绕过
	regexp.MustCompile(`(?i)(?:^|[\s|;(&])(?:cat|less|more|head|tail|grep|egrep|strings|xxd|od|base64|dd|cp|scp|rsync|tar|zip|awk|sed|cut|tr|sort|uniq|vi|vim|nano)\b[^|;\n]*\b(?:g?shadow|private[_-]?key|credentials|\.netrc|\.pgpass|environ)\b`),
}

// appCredentialPaths 是「应用级凭据文件」—— .env、credentials.json 之类。
//
// 与 protectedPaths 的区别在于**作用域**，这个区别是有意的：
//   - 读取：必须硬拒绝。这些文件里通常同时放着数据库口令、云厂商 token、
//     第三方 API key，泄露后果与系统凭据等同，而需求 4 的本意正是「LLM 不得接触凭据」。
//   - 写入：只做人工确认，不硬拒绝。轮换口令、改配置本来就要动这些文件，
//     一刀切禁掉会让工具在最需要它的场景里不可用。
//
// 因此这一组只在 EvaluateProtected（命令文本 / 读路径）里生效，
// 不参与 EvaluateWrite。
var appCredentialPaths = []*regexp.Regexp{
	// .env / .env.local / .env.production
	//
	// 前缀要求路径边界（含 ; | & < > 等命令分隔符，因为 `cat .env; echo done`
	// 里的 .env 同样是路径）；后缀用「**不是**路径字符」来判定结束 —— 这样
	// 分隔符天然算边界，而 foo.environment 里 env 后面跟的是字母，不会误伤。
	regexp.MustCompile(`(?i)(?:^|[\s'"=~(;|&<>/])\.env(?:\.[A-Za-z0-9_]+)?(?:$|[^A-Za-z0-9_.\-/])`),
	// credentials.json / .credentials / /srv/credentials / .htpasswd
	// 要求紧邻路径分隔符或位于串首，否则命令行里的普通单词
	// （例如 `journalctl --since "secret"`）会被误判成文件路径。
	regexp.MustCompile(`(?i)(?:/|^)\.?(?:credentials?|secrets?|\.htpasswd)(?:\.[A-Za-z0-9]+)?(?:$|[^A-Za-z0-9_.\-/])`),
}

// envDumps 命中即拒绝：整块 dump 环境变量，而环境变量里常有 token/口令。
var envDumps = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?:sudo\s+)?printenv\b`),
	regexp.MustCompile(`(?i)^\s*(?:sudo\s+)?env\s*(?:$|[|;&>])`),
	regexp.MustCompile(`(?i)(?:^|[|;&])\s*(?:sudo\s+)?export\s+-p\b`),
}

// destructive 命中即拒绝：不可逆或会导致大面积中断的操作。
var destructive = []struct {
	re   *regexp.Regexp
	rule string
}{
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`), "格式化文件系统"},
	{regexp.MustCompile(`(?i)\bwipefs\b`), "擦除文件系统签名"},
	{regexp.MustCompile(`(?i)\bdd\b[^|;]*\bof=/dev/(?:sd|nvme|hd|vd|mmcblk)`), "向块设备写数据"},
	{regexp.MustCompile(`(?i)>\s*/dev/(?:sd|nvme|hd|vd|mmcblk)`), "覆写块设备"},
	{regexp.MustCompile(`(?i)\brm\b[^|;]*\s-{1,2}[a-z]*r[a-z]*f?[^|;]*\s/(?:\s|$|\*)`), "递归删除根目录"},
	{regexp.MustCompile(`(?i)\brm\b[^|;]*\s-{1,2}[a-z]*f[a-z]*r?[^|;]*\s/(?:etc|usr|var|boot|lib|bin|sbin|home|root)(?:\s|/|\*|$)`), "递归删除系统目录"},
	{regexp.MustCompile(`(?i)\b(?:shutdown|reboot|halt|poweroff)\b`), "关机/重启"},
	{regexp.MustCompile(`(?i)\binit\s+[06]\b`), "切换运行级别"},
	{regexp.MustCompile(`(?i)\bsystemctl\s+(?:poweroff|reboot|halt|kexec)\b`), "关机/重启"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{.*\};\s*:`), "fork bomb"},
	{regexp.MustCompile(`(?i)\bchmod\s+-R\s+0?777\s+/(?:\s|$)`), "全盘 777 授权"},
	{regexp.MustCompile(`(?i)\bchown\s+-R\b[^|;]*\s/(?:\s|$)`), "全盘改属主"},
	{regexp.MustCompile(`(?i)\bkill\s+-9\s+-1\b`), "杀光所有进程"},
	{regexp.MustCompile(`(?i)\b(?:iptables|nft)\s+-(?:F|X)\b`), "清空防火墙规则"},
	{regexp.MustCompile(`(?i)\btruncate\s+-s\s*0\s+/etc/`), "清空系统配置"},
	{regexp.MustCompile(`(?i)\buserdel\s+-r\b`), "删除用户及家目录"},
}

// EvaluateProtected 只跑硬拒绝规则（凭据路径、环境变量导出、破坏性命令），
// 不涉及模式与白名单。用于读类工具的路径检查 —— 这类工具没有 shell 命令，
// 但仍必须受凭据保护规则约束。
//
// 空输入视为「无路径可查」，返回 Allow，由调用方按模式决定是否需确认。
func EvaluateProtected(cmd string) Verdict {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return Verdict{Allow, "无路径需要检查", "skip"}
	}
	for _, re := range protectedPaths {
		if re.MatchString(c) {
			return Verdict{
				Deny,
				"该操作试图读取 Linux 登录凭据文件，已被硬拒绝（需求 4：LLM 不得接触登录配置与密钥）",
				"protected_path",
			}
		}
	}
	// 应用级凭据文件（.env / credentials.json …）：读取同样硬拒绝，理由见该变量的注释。
	for _, re := range appCredentialPaths {
		if re.MatchString(c) {
			return Verdict{
				Deny,
				"该操作试图读取应用凭据文件（如 .env / credentials），其中常含数据库口令与云 token，已被硬拒绝",
				"app_credential",
			}
		}
	}
	for _, re := range envDumps {
		if re.MatchString(c) {
			return Verdict{
				Deny,
				"该操作会整块导出环境变量，可能泄露 token/口令，已被硬拒绝",
				"env_dump",
			}
		}
	}
	for _, d := range destructive {
		if d.re.MatchString(c) {
			return Verdict{Deny, "破坏性操作（" + d.rule + "）已被硬拒绝", "destructive"}
		}
	}
	return Verdict{Allow, "未命中任何硬拒绝规则", "clean"}
}

// Evaluate 对 agent（LLM）提出的一条 shell 命令做裁决。
func Evaluate(cmd string, mode Mode, whitelist []string) Verdict {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return Verdict{Deny, "空命令", "empty"}
	}

	if v := EvaluateProtected(c); v.Decision == Deny {
		return v
	}

	// 高危变更优先于白名单：即便有人把 rm 写进白名单，也不能无人确认就执行。
	if reason := HighRiskReason(c); reason != "" {
		return Verdict{Confirm, "高危变更（" + reason + "）需人工确认", "high_risk"}
	}

	if mode == ModeWhitelist && matchWhitelist(c, whitelist) {
		return Verdict{Allow, "命中只读白名单，自动执行", "whitelist"}
	}
	if mode == ModeManual {
		return Verdict{Confirm, "手动模式：所有命令需人工确认", "manual"}
	}
	return Verdict{Confirm, "未命中白名单，需人工确认", "not_whitelisted"}
}

// EvaluateWrite 对写文件类工具做裁决 —— 写操作永远需要人工确认。
func EvaluateWrite(raw string) Verdict {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Verdict{Deny, "空路径", "empty"}
	}

	// 先规范化再比对。早先是直接对原始字符串做 HasPrefix，于是
	// `/etc/../boot/grub/grub.cfg` 能绕过「禁止写 /boot」这条硬拒绝。
	//
	// 用 path 而不是 filepath：被审查的是**远端 Linux 路径**，而本进程可能跑在
	// Windows 上 —— filepath 会把 `/` 当普通字符，Clean 的结果在 Linux 上不成立。
	// 局限：Clean 只做词法规范化，不解析符号链接；指向 /boot 的软链接仍可绕过，
	// 但这类路径会落到下面的 Confirm 分支，仍需人工确认，不构成静默写入。
	p := path.Clean(raw)

	// 系统关键目录：按路径边界比对，避免 /bootloader 被误判成 /boot。
	for _, pre := range []string{"/boot", "/dev", "/proc", "/sys"} {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return Verdict{Deny, "禁止写入系统关键目录 " + pre, "protected_write"}
		}
	}
	for _, re := range protectedPaths {
		if re.MatchString(p) {
			return Verdict{Deny, "禁止写入登录凭据文件（需求 4）", "protected_path"}
		}
	}
	return Verdict{Confirm, "写操作需人工确认", "write"}
}

// matchWhitelist 判断命令是否命中白名单。按管道/逻辑分隔符切成命令段，
// 逐段检查是否以某个白名单条目开头（按词边界）。
//
// 安全前提：白名单只放行「段首是白名单词」的命令，因此必须保证段内不存在
// 其它可执行/可展开的语法，否则段首合法就等同于整体合法。这一步由
// segmentAllowed 开头的元字符闸门负责 —— 改动这里时务必保持该闸门在最前面。
func matchWhitelist(cmd string, whitelist []string) bool {
	if len(whitelist) == 0 {
		return false
	}
	segs := splitSegments(cmd)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if !segmentAllowed(seg, whitelist) {
			return false
		}
	}
	return true
}

func splitSegments(cmd string) []string {
	f := func(r rune) bool { return r == '|' || r == ';' || r == '&' || r == '\n' }
	var out []string
	for _, s := range strings.FieldsFunc(cmd, f) {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func segmentAllowed(seg string, whitelist []string) bool {
	s := strings.TrimSpace(seg)

	// shell 元字符闸门 —— 必须在剥离前缀之前、作用于**原始段**。
	//
	// 为什么必须放在最前面：下面会剥掉 `VAR=value ` 前缀，而 `FOO=$(id) uptime`
	// 里的 `$(id)` 恰好落在被剥掉的那部分。若先剥离再检查，替换体就随前缀一起
	// 消失了，闸门形同虚设。所以检查先于剥离。
	//
	// 拒绝的语法：
	//   $() ``   命令替换 —— 段首合法即可执行任意命令，这是最容易漏的一条
	//   $VAR ${} 变量展开 —— 可以拼出被拦截的字面量
	//   ()       子 shell —— 与命令替换等价
	//   {}       花括号展开 —— 可构造出意料之外的参数
	//   <>       重定向 —— 读或写的副作用，白名单只承诺「只读诊断」
	//
	// 注意：这是「不放行」而非「硬拒绝」—— 命令会退回人工确认，人有权批准。
	if strings.ContainsAny(s, "$`(){}<>") {
		return false
	}

	// 跳过 sudo / 变量赋值前缀
	for {
		switch {
		case strings.HasPrefix(s, "sudo "):
			s = strings.TrimSpace(s[5:])
			continue
		case regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s`).MatchString(s):
			s = strings.TrimSpace(regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s`).ReplaceAllString(s, ""))
			continue
		}
		break
	}
	low := strings.ToLower(s)
	for _, w := range whitelist {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if low == w {
			return true
		}
		if strings.HasPrefix(low, w) {
			rest := low[len(w):]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
				return true
			}
		}
	}
	return false
}

// Risk 给命令打一个风险标签，仅用于 UI 展示。
func Risk(cmd string) string {
	if v := EvaluateProtected(cmd); v.Decision == Deny {
		switch v.Rule {
		case "protected_path", "app_credential":
			return "credential"
		case "destructive":
			return "destructive"
		case "env_dump":
			return "credential"
		default:
			return "denied"
		}
	}
	if IsHighRisk(cmd) {
		return "high"
	}
	return "normal"
}
