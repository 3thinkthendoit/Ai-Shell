package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"ai-shell/internal/audit"
	"ai-shell/internal/policy"
	"ai-shell/internal/vault"
)

func tinyPNGBase64() string {
	// 1x1 transparent PNG
	raw, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
	)
	return base64.StdEncoding.EncodeToString(raw)
}

func tinyJPEGBase64() string {
	// Minimal JPEG (1x1)
	raw := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
		0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
		0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
		0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
		0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20,
		0x24, 0x2e, 0x27, 0x20, 0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
		0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27, 0x39, 0x3d, 0x38, 0x32,
		0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
		0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x1f, 0x00, 0x00,
		0x01, 0x05, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0xff, 0xc4, 0x00, 0xb5, 0x10, 0x00, 0x02, 0x01, 0x03,
		0x03, 0x02, 0x04, 0x03, 0x05, 0x05, 0x04, 0x04, 0x00, 0x00, 0x01, 0x7d,
		0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06,
		0x13, 0x51, 0x61, 0x07, 0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08,
		0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0, 0x24, 0x33, 0x62, 0x72,
		0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45,
		0x46, 0x47, 0x48, 0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59,
		0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x73, 0x74, 0x75,
		0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
		0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3,
		0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6,
		0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9,
		0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
		0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4,
		0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01,
		0x00, 0x00, 0x3f, 0x00, 0x7f, 0xff, 0xd9,
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestNormalizeImagesAcceptsDataURL(t *testing.T) {
	b64 := tinyPNGBase64()
	imgs, err := NormalizeImages([]ImageIn{{
		Mime: "image/png",
		Data: "data:image/png;base64," + b64,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].Mime != "image/png" || imgs[0].Data == "" {
		t.Fatalf("got %+v", imgs)
	}
}

func TestNormalizeImagesRejectsNonImage(t *testing.T) {
	_, err := NormalizeImages([]ImageIn{{Mime: "application/pdf", Data: tinyPNGBase64()}})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeImagesRejectsSVG(t *testing.T) {
	svg := base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	_, err := NormalizeImages([]ImageIn{{Mime: "image/svg+xml", Data: svg}})
	if err == nil {
		t.Fatal("expected svg reject")
	}
}

func TestNormalizeImagesRejectsMimeMismatch(t *testing.T) {
	_, err := NormalizeImages([]ImageIn{{Mime: "image/jpeg", Data: tinyPNGBase64()}})
	if err == nil {
		t.Fatal("expected mime/content mismatch")
	}
	if !strings.Contains(err.Error(), "不符") {
		t.Fatalf("want mismatch error, got %v", err)
	}
}

func TestNormalizeImagesRejectsFakeMagic(t *testing.T) {
	// 声明 png，内容却是任意字节 —— 魔数校验必须拦。
	fake := base64.StdEncoding.EncodeToString([]byte("not-an-image-payload"))
	_, err := NormalizeImages([]ImageIn{{Mime: "image/png", Data: fake}})
	if err == nil {
		t.Fatal("expected magic reject")
	}
}

func TestNormalizeImagesSniffsWhenMimeEmpty(t *testing.T) {
	imgs, err := NormalizeImages([]ImageIn{{Mime: "", Data: tinyJPEGBase64()}})
	if err != nil {
		t.Fatal(err)
	}
	if imgs[0].Mime != "image/jpeg" {
		t.Fatalf("sniffed mime=%q", imgs[0].Mime)
	}
}

func TestNormalizeImagesRejectsTooLarge(t *testing.T) {
	big := strings.Repeat("A", ((maxImageBytes+3)/3)*4) // oversize base64-ish
	_, err := NormalizeImages([]ImageIn{{Mime: "image/png", Data: big}})
	if err == nil {
		t.Fatal("expected size error")
	}
}

func TestUserMessageWithImagesSetsParts(t *testing.T) {
	msg := UserMessageWithImages("看报错", []ImageIn{{Mime: "image/png", Data: tinyPNGBase64()}})
	if msg.Content != "看报错" {
		t.Fatalf("content=%q", msg.Content)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("parts=%d", len(msg.Parts))
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"image_url"`) {
		t.Fatalf("marshal missing image_url: %s", raw)
	}
}

func TestAskRejectsImagesWithoutVision(t *testing.T) {
	// App-level gate is in app.go; here verify Agent.Run also gates.
	h := newHarness(t, nil, policy.ModeWhitelist, true)
	defer h.ssh.Close()

	err := h.ag.Run(context.Background(), h.hostID, DefaultSessionID, "看图", "", []ImageIn{
		{Mime: "image/png", Data: tinyPNGBase64()},
	})
	if err == nil {
		t.Fatal("expected vision gate error")
	}
}

func TestAskAllowsImagesWithVision(t *testing.T) {
	h := newHarness(t, []string{respContent("看到了 nginx 错误")}, policy.ModeWhitelist, true)
	defer h.ssh.Close()

	// Enable vision on active profile.
	ps := h.v.LLMProfiles()
	if len(ps) == 0 {
		t.Fatal("no profiles")
	}
	p, ok := h.v.LLMProfile(ps[0].ID)
	if !ok {
		t.Fatal("missing profile")
	}
	p.SupportsVision = true
	if err := h.v.SaveLLMProfile(p, nil); err != nil {
		t.Fatal(err)
	}

	prompt := "看这张 nginx 报错"
	err := h.ag.Run(context.Background(), h.hostID, DefaultSessionID, prompt, "", []ImageIn{
		{Mime: "image/png", Data: tinyPNGBase64()},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// 出站请求必须带 image_url（假 LLM 经 UnmarshalJSON 还原 Parts）。
	msgs := h.fake.messagesAt(0)
	foundImage := false
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, part := range m.Parts {
			if part.Type == "image_url" && part.ImageURL != nil &&
				strings.HasPrefix(part.ImageURL.URL, "data:image/png;base64,") {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("outbound request missing image_url part: %+v", msgs)
	}

	// History must be text-only (no Parts persisted).
	hist := histDefault(h.ag, h.hostID)
	for _, m := range hist {
		if len(m.Parts) > 0 {
			t.Fatalf("history must not keep Parts: %+v", m)
		}
	}

	// 审计记文本 + 附图张数，不落 base64。
	entries, err := h.auditor.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var run *audit.Entry
	for i := range entries {
		e := &entries[i]
		if e.Kind == audit.KindAgentRun && strings.Contains(e.Command, "nginx") {
			run = e
			break
		}
	}
	if run == nil {
		t.Fatal("missing agent_run audit")
	}
	if run.Command != prompt {
		t.Fatalf("audit Command=%q want %q", run.Command, prompt)
	}
	if strings.Contains(run.Command, "base64") || strings.Contains(run.Note, "iVBORw0") {
		t.Fatalf("audit must not embed image bytes: %+v", run)
	}
	if !strings.Contains(run.Note, "附图 1 张") {
		t.Fatalf("audit Note missing image count: %q", run.Note)
	}
}

// Ensure vault field round-trips.
func TestLLMProfileSupportsVisionView(t *testing.T) {
	dir := t.TempDir()
	v := vault.New(dir)
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	ps := v.LLMProfiles()
	p, _ := v.LLMProfile(ps[0].ID)
	p.SupportsVision = true
	if err := v.SaveLLMProfile(p, nil); err != nil {
		t.Fatal(err)
	}
	views := v.LLMProfiles()
	if !views[0].SupportsVision {
		t.Fatal("view missing SupportsVision")
	}
	settings, _, ok := v.LLMFor(ps[0].ID)
	if !ok || !settings.SupportsVision {
		t.Fatalf("LLMFor missing vision: %+v", settings)
	}
}
