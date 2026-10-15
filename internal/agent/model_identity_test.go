package agent

import (
	"strings"
	"testing"
)

func TestModelIdentityNoteNamesAPIModel(t *testing.T) {
	got := modelIdentityNote("deepseek-v4.1-flash", "国外")
	if got == "" {
		t.Fatal("身份说明不应为空")
	}
	for _, want := range []string{"国外", "deepseek-v4.1-flash", "历史"} {
		if !strings.Contains(got, want) {
			t.Fatalf("应包含 %q，实得: %s", want, got)
		}
	}
	bare := modelIdentityNote("gpt-4o", "")
	if !strings.Contains(bare, "gpt-4o") {
		t.Fatalf("无方案名时应报模型，实得: %s", bare)
	}
	if strings.Contains(bare, "方案「") {
		t.Fatalf("无方案名时不应出现方案字样，实得: %s", bare)
	}
}
