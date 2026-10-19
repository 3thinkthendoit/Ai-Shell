package agent

import (
	"strings"
	"testing"

	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

type fakeConsole struct {
	text string
	err  error
	last struct {
		host, sess string
		lines      int
	}
}

func (f *fakeConsole) ReadConsole(hostID, sessionID string, maxLines int) (string, error) {
	f.last.host, f.last.sess, f.last.lines = hostID, sessionID, maxLines
	return f.text, f.err
}

func TestRunReadConsole(t *testing.T) {
	fc := &fakeConsole{text: "root@host:~# ./start.sh\nsyntax error near unexpected token '('\n"}
	ctx := toolCtx{
		console:       fc,
		sessionHostID: "h1",
		sessionID:     "s1",
	}
	res, err := runReadConsole(ctx, map[string]any{"host_id": "h1", "lines": 40})
	if err != nil {
		t.Fatalf("runReadConsole: %v", err)
	}
	if !strings.Contains(res.Stdout, "syntax error") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
	if fc.last.host != "h1" || fc.last.sess != "s1" || fc.last.lines != 40 {
		t.Fatalf("args not forwarded: %+v", fc.last)
	}
}

func TestRunReadConsoleDefaultsSessionHost(t *testing.T) {
	fc := &fakeConsole{text: "ok"}
	ctx := toolCtx{console: fc, sessionHostID: "h-bound", sessionID: "s9"}
	if _, err := runReadConsole(ctx, map[string]any{}); err != nil {
		t.Fatalf("empty host_id should fall back: %v", err)
	}
	if fc.last.host != "h-bound" || fc.last.sess != "s9" {
		t.Fatalf("fallback host/session: %+v", fc.last)
	}
}

func TestRunReadConsoleRequiresConsole(t *testing.T) {
	_, err := runReadConsole(toolCtx{sessionHostID: "h1"}, map[string]any{"host_id": "h1"})
	if err == nil {
		t.Fatal("nil console should error")
	}
}

func TestReadConsoleInRegistry(t *testing.T) {
	tool, ok := lookupTool("read_console")
	if !ok {
		t.Fatal("read_console missing from registry")
	}
	if tool.Local {
		t.Fatal("read_console needs host_id, should not be Local")
	}
	v := tool.Evaluate(&ToolCallView{Name: "read_console"}, vault.PolicySettings{})
	if v.Decision != policy.Allow {
		t.Fatalf("decision=%v", v.Decision)
	}
}

func TestSimpleToolWithoutPlanIncludesReadConsole(t *testing.T) {
	if !simpleToolWithoutPlan("read_console", "", nil) {
		t.Fatal("read_console should be plan-free observation")
	}
}

func TestReadConsoleAllowedInEveryPlanClass(t *testing.T) {
	for _, c := range []PlanClass{classDiagnose, classChange, classRollback, classLookup} {
		if !classAllows(c, "read_console") {
			t.Fatalf("%s should allow read_console", c)
		}
	}
}
