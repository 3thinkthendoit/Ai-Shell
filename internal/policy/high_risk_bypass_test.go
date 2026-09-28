package policy

import (
	"testing"
	"ai-shell/internal/vault"
)

func TestHighRiskBypassClosed(t *testing.T) {
	wl := vault.DefaultWhitelist()
	cases := []string{
		"FOO=1 mount /dev/sda1 /mnt",
		"command rm -f /tmp/x",
		"time rm -f /tmp/x",
		"env rm -f /tmp/x",
		"/bin/rm -f /tmp/x",
		"bash -c 'rm -f /tmp/x'",
		"true; /bin/rm -f /tmp/x",
		"FOO=1 kill 1",
	}
	for _, c := range cases {
		if v := Evaluate(c, ModeWhitelist, wl); v.Decision != Confirm || v.Rule != "high_risk" {
			t.Errorf("LLM Evaluate(%q)=%s/%s want confirm/high_risk", c, v.Decision, v.Rule)
		}
		if v := EvaluateHuman(c); v.Decision != Confirm || v.Rule != "high_risk" {
			t.Errorf("EvaluateHuman(%q)=%s/%s want confirm/high_risk", c, v.Decision, v.Rule)
		}
	}
	// 只读仍放行
	if v := Evaluate("FOO=1 ls -la", ModeWhitelist, wl); v.Decision != Allow {
		t.Errorf("FOO=1 ls should allow, got %s/%s", v.Decision, v.Rule)
	}
	if v := EvaluateHuman("FOO=1 ls -la"); v.Decision != Allow {
		t.Errorf("human FOO=1 ls should allow, got %s/%s", v.Decision, v.Rule)
	}
}
