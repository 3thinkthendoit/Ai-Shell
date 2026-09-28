package policy

import (
	"regexp"
	"strings"
	"unicode"
)

// envAssignRe 匹配段首「VAR=value 」前缀（与 segmentAllowed 同形）。
var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s`)

// transparentWrappers 是「只是套一层、真正干活的是后面那个」的前缀命令。
// 剥掉它们再做高危 / 主程序识别，避免 `command rm` / `env kill` 绕过确认。
var transparentWrappers = map[string]struct{}{
	"command": {}, "builtin": {}, "time": {}, "nice": {}, "nohup": {},
	"env": {}, "ionice": {}, "stdbuf": {}, "timeout": {},
}

// shellInterpreters 带 -c 时，整段视为高危（任意代码执行）。
var shellInterpreters = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {},
	"busybox": {},
}

// stripLeadingNoise 剥掉段首的 sudo / VAR= / 透明包装，返回剩余文本。
func stripLeadingNoise(seg string) string {
	s := strings.TrimSpace(seg)
	for {
		switch {
		case strings.HasPrefix(s, "sudo "):
			s = strings.TrimSpace(s[5:])
			continue
		case envAssignRe.MatchString(s):
			s = strings.TrimSpace(envAssignRe.ReplaceAllString(s, ""))
			continue
		}
		word, rest := firstWord(s)
		name := strings.ToLower(BaseName(word))
		if _, ok := transparentWrappers[name]; ok {
			// `command -v foo` / `command -- foo`：跳过短选项后继续
			s = rest
			for {
				w, r := firstWord(s)
				if w == "" {
					break
				}
				if w == "--" {
					s = r
					break
				}
				if strings.HasPrefix(w, "-") && len(w) > 1 {
					s = r
					continue
				}
				break
			}
			continue
		}
		break
	}
	return s
}

func firstWord(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	for i, r := range s {
		if unicode.IsSpace(r) {
			return strings.Trim(s[:i], `"'`), strings.TrimSpace(s[i:])
		}
	}
	return strings.Trim(s, `"'`), ""
}

// normalizeForRisk 把命令展开成「可匹配高危」的若干探针串：
//
//  1. 原文（保留重定向 / 管道进解释器等全串规则）
//  2. 每一段剥噪后的文本（FOO=1 mount … → mount …）
//  3. 剥噪后用 basename 替换路径形 argv0（/bin/rm -f x → rm -f x）
//  4. bash/sh -c 视为高危（任意脚本）
func normalizeForRisk(cmd string) []string {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return nil
	}
	out := []string{c}
	seen := map[string]struct{}{c: {}}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	for _, seg := range splitSegments(c) {
		clean := stripLeadingNoise(seg)
		add(clean)

		word, rest := firstWord(clean)
		if word == "" {
			continue
		}
		base := BaseName(word)
		if base != word {
			add(strings.TrimSpace(base + " " + rest))
		}
		low := strings.ToLower(base)
		if _, ok := shellInterpreters[low]; ok {
			// sh -c '…' / bash -lc '…'
			r := rest
			for {
				w, nr := firstWord(r)
				if w == "" {
					break
				}
				if w == "-c" || strings.HasPrefix(w, "-") && strings.Contains(w, "c") && !strings.HasPrefix(w, "--") {
					add("eval " + nr) // 复用 eval/source 高危规则
					break
				}
				if strings.HasPrefix(w, "-") {
					r = nr
					continue
				}
				break
			}
		}
	}
	return out
}

// PrimaryBinary 抽出命令的主程序名（第一段、剥掉 sudo / 环境赋值 / 透明包装后的词）。
// 复杂管道只取第一段；解析失败返回空串（调用方应跳过存在性检查）。
func PrimaryBinary(cmd string) string {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return ""
	}
	segs := splitSegments(c)
	if len(segs) == 0 {
		return ""
	}
	clean := stripLeadingNoise(segs[0])
	word, _ := firstWord(clean)
	if word == "" || !primaryWordRe.MatchString(word) {
		return ""
	}
	return word
}

var primaryWordRe = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)

// BaseName 返回路径最后一段（/usr/bin/foo → foo）。无斜杠则原样返回。
func BaseName(bin string) string {
	if i := strings.LastIndex(bin, "/"); i >= 0 {
		return bin[i+1:]
	}
	return bin
}

// IsKnownBinary 判断是否属于「Linux / 内建 / 只读命令库」——
// 人工 shell 对这类命令不必再做 command -v；未知第三方才探测。
func IsKnownBinary(bin string, whitelist []string) bool {
	name := strings.ToLower(BaseName(strings.TrimSpace(bin)))
	if name == "" {
		return false
	}
	if _, ok := shellBuiltins[name]; ok {
		return true
	}
	for _, w := range whitelist {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		first := w
		if i := strings.IndexAny(w, " \t"); i >= 0 {
			first = w[:i]
		}
		if name == first || name == w {
			return true
		}
	}
	return false
}

// shellBuiltins 是常见 shell 内建 —— 属于「Linux 命令库」，人工 shell 无需再做存在性探测。
var shellBuiltins = map[string]struct{}{
	"cd": {}, "echo": {}, "printf": {}, "true": {}, "false": {},
	"test": {}, "[": {}, "export": {}, "unset": {}, "readonly": {},
	"pwd": {}, "exit": {}, "shift": {}, "set": {}, "alias": {}, "unalias": {},
	"type": {}, "command": {}, "builtin": {}, "hash": {}, "ulimit": {}, "umask": {},
	"wait": {}, "jobs": {}, "fg": {}, "bg": {}, "time": {}, "read": {},
	"local": {}, "declare": {}, "typeset": {}, "let": {}, "clear": {},
	"history": {}, "source": {}, ".": {}, "eval": {}, "exec": {},
	"kill": {}, "bind": {}, "complete": {}, "compgen": {},
}
