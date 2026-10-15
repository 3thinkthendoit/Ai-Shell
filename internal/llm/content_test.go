package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageMarshalTextOnly(t *testing.T) {
	raw, err := json.Marshal(Message{Role: "user", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":"hello"`) {
		t.Fatalf("got %s", raw)
	}
}

func TestMessageMarshalMultimodalParts(t *testing.T) {
	m := Message{
		Role:    "user",
		Content: "看这张图",
		Parts: []ContentPart{
			{Type: "text", Text: "看这张图"},
			{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,aaa"}},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	arr, ok := probe["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("content should be array of 2, got %#v", probe["content"])
	}

	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Content != "看这张图" {
		t.Fatalf("unmarshal Content=%q", back.Content)
	}
	if len(back.Parts) != 2 || back.Parts[1].Type != "image_url" ||
		back.Parts[1].ImageURL == nil || back.Parts[1].ImageURL.URL == "" {
		t.Fatalf("unmarshal Parts=%+v", back.Parts)
	}
}

func TestMessageUnmarshalTextString(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Content != "hi" || len(m.Parts) != 0 {
		t.Fatalf("got %+v", m)
	}
}
