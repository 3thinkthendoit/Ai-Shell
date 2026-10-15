package sshtest

// Fake-remote handlers for ops diagnostic tool scripts.
// The test SSH server is not a real shell; it matches markers and returns fixtures.

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
)

func runCollectDiagnostics(c string, ch ssh.Channel) int {
	scope := "default"
	if i := strings.Index(c, "scope="); i >= 0 {
		rest := c[i+len("scope="):]
		if end := strings.IndexAny(rest, "\"'\n ;"); end > 0 {
			scope = rest[:end]
		} else if fields := strings.Fields(rest); len(fields) > 0 {
			scope = fields[0]
		}
	}
	fmt.Fprintf(ch, "== meta ==\n2026-10-15T10:00:00+08:00\ntesthost\nscope=%s\n", scope)
	switch scope {
	case "disk":
		fmt.Fprint(ch, "== df ==\nFilesystem  Type  Size Used Avail Use%\n/dev/sda1   ext4   40G  12G   28G  30% /\n"+
			"== inodes ==\nFilesystem  Inodes IUsed IFree\n/dev/sda1   1000 100 900\n"+
			"== mounts ==\n/ /dev/sda1 ext4 rw\n")
	case "network":
		fmt.Fprint(ch, "== listening ==\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"+
			"== routes ==\nlo UNKNOWN 127.0.0.1/8\n"+
			"== dns ==\nnameserver 1.1.1.1\n"+
			"== journal_net ==\nOct 15 nginx: fake net log\n")
	case "journal":
		fmt.Fprint(ch, "== journal_err ==\nOct 15 testhost: fake journal error\n")
	case "systemd":
		fmt.Fprint(ch, "== failed_units ==\n0 loaded units listed.\n== recent_starts ==\nnginx.service loaded active running\n")
	default:
		fmt.Fprint(ch, "== os ==\nNAME=\"Test Linux\"\n== load ==\nload average: 0.10\n== memory ==\nMem: 7.8Gi\n"+
			"== disk ==\n/dev/sda1 40G 12G 28G 30%\n== failed_units ==\n0 loaded units listed.\n"+
			"== top_procs ==\nPID COMMAND\n1 systemd\n== listening ==\nLISTEN 0.0.0.0:22\n"+
			"== journal_err ==\nOct 15 testhost: fake journal error\n")
	}
	return 0
}

func runSnapshotScript(c string, ch ssh.Channel, fs *memFS) int {
	id := "snap-test"
	if i := strings.Index(c, "id="); i >= 0 {
		rest := c[i+len("id="):]
		rest = strings.TrimPrefix(rest, "'")
		rest = strings.TrimPrefix(rest, "\"")
		if end := strings.IndexAny(rest, "'\" ;&"); end > 0 {
			id = rest[:end]
		}
	}
	path := "/tmp/aishell-snapshots/" + id + ".snap"
	body := "# AISHELL_SNAPSHOT id=" + id + " time=2026-10-15T10:00:00 scopes=os,load\n## os\nNAME=\"Test Linux\"\n## load\nload average: 0.10\n"
	fs.write(path, body)
	fmt.Fprintf(ch, "saved=%s\n\n%s", path, body)
	return 0
}

func runDiffScript(c string, ch ssh.Channel, fs *memFS) int {
	bf := extractQuotedAfter(c, "bf=")
	if bf == "" {
		bf = "/tmp/aishell-snapshots/missing.snap"
	}
	if _, ok := fs.read(bf); !ok {
		fmt.Fprintf(ch, "baseline snapshot missing: %s\n", bf)
		return 2
	}
	af := "/tmp/aishell-snapshots/current-test.snap"
	if q := extractQuotedAfter(c, "af="); q != "" && !strings.Contains(q, "$") {
		af = q
	}
	body := "# AISHELL_SNAPSHOT id=current-test scopes=os,load\n## os\nNAME=\"Test Linux\"\n## load\nload average: 0.20\n"
	fs.write(af, body)
	fmt.Fprintf(ch, "before=%s\nafter=%s\n\n--- %s\n+++ %s\n@@ load @@\n-load average: 0.10\n+load average: 0.20\n", bf, af, bf, af)
	return 0
}

func runVerifyScript(c string, ch ssh.Channel) int {
	// Port 1 is treated as not listening for the failure test case.
	if strings.Contains(c, "p=1;") || strings.Contains(c, ":1[[:space:]]") || strings.Contains(c, ":1$") {
		fmt.Fprint(ch, "FAIL check[0] port port=1 not listening\n\nVERIFY_RESULT=FAIL\n")
		return 1
	}
	fmt.Fprint(ch, "PASS check[0]\n\nVERIFY_RESULT=PASS\n")
	return 0
}

func runListBackups(c string, ch ssh.Channel, fs *memFS) int {
	p := extractQuotedAfter(c, "p=")
	if p == "" {
		fmt.Fprint(ch, "(no backups)\n")
		return 0
	}
	fmt.Fprintf(ch, "target=%s\n", p)
	baks := fs.backups(p)
	if len(baks) == 0 {
		fmt.Fprint(ch, "(no backups)\n")
		return 0
	}
	sort.Strings(baks)
	for i := len(baks) - 1; i >= 0; i-- {
		fmt.Fprintf(ch, "-rw-r--r-- 1 root root 4 Oct 15 10:00 %s\n", baks[i])
	}
	return 0
}

func runRestoreBackup(c string, ch ssh.Channel, fs *memFS) int {
	p := extractQuotedAfter(c, "p=")
	if p == "" {
		fmt.Fprint(ch, "path missing\n")
		return 2
	}
	bak := extractQuotedAfter(c, "bak=")
	if bak == "" || strings.Contains(bak, "$") {
		baks := fs.backups(p)
		if len(baks) == 0 {
			fmt.Fprintf(ch, "no backups found: %s.bak.*\n", p)
			return 2
		}
		sort.Strings(baks)
		bak = baks[len(baks)-1]
	}
	content, ok := fs.read(bak)
	if !ok {
		fmt.Fprintf(ch, "backup missing: %s\n", bak)
		return 2
	}
	if cur, ok := fs.read(p); ok {
		pre := p + ".bak.pre-restore.test"
		fs.write(pre, cur)
		fmt.Fprintf(ch, "current_saved=%s\n", pre)
	}
	fs.write(p, content)
	fmt.Fprintf(ch, "restored_from=%s\ntarget=%s\n%d\n", bak, p, len(content))
	return 0
}

func runSystemctl(c string, ch ssh.Channel) int {
	unit := "unknown"
	for _, key := range []string{"is-active ", "is-enabled ", "status --no-pager -l ", "status "} {
		if i := strings.Index(c, key); i >= 0 {
			rest := strings.TrimSpace(c[i+len(key):])
			rest = strings.Trim(rest, "'\"")
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				unit = strings.Trim(fields[0], "'\"")
				break
			}
		}
	}
	fmt.Fprintf(ch, "== %s ==\nactive\nenabled\n* %s - fake unit\n   Active: active (running)\n", unit, unit)
	return 0
}

func extractQuotedAfter(c, key string) string {
	i := strings.Index(c, key)
	if i < 0 {
		return ""
	}
	rest := c[i+len(key):]
	if rest == "" {
		return ""
	}
	switch rest[0] {
	case '\'', '"':
		q := rest[0]
		end := strings.IndexByte(rest[1:], q)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	default:
		end := strings.IndexAny(rest, " ;&|\n")
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
}
