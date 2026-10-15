package llm

import (
	"encoding/json"
	"fmt"
)

// ContentPart 是 OpenAI 兼容的多模态 content 数组元素。
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 是 vision 模型的图片引用（通常是 data URL）。
type ImageURL struct {
	URL string `json:"url"`
}

// MarshalJSON 在 Parts 非空时把 content 序列化成数组，否则保持字符串。
//
// 历史消息、工具结果仍是纯文本 Content；只有当前用户轮可能带 Parts。
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role       string     `json:"role"`
		Content    any        `json:"content,omitempty"`
		Reasoning  string     `json:"reasoning_content,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		Name       string     `json:"name,omitempty"`
	}
	w := wire{
		Role:       m.Role,
		Reasoning:  m.Reasoning,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	switch {
	case len(m.Parts) > 0:
		w.Content = m.Parts
	case m.Content != "":
		w.Content = m.Content
	}
	return json.Marshal(w)
}

// UnmarshalJSON 同时接受字符串 content 与多模态数组。
//
// 假 LLM / 网关回放 / 测试抓包都会走这里；只有 Marshal 没有 Unmarshal
// 时，数组 content 会静默丢进 Content 字符串字段（解码失败），断言永远绿不了。
func (m *Message) UnmarshalJSON(data []byte) error {
	type wire struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Reasoning  string          `json:"reasoning_content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Role = w.Role
	m.Reasoning = w.Reasoning
	m.ToolCalls = w.ToolCalls
	m.ToolCallID = w.ToolCallID
	m.Name = w.Name
	m.Content = ""
	m.Parts = nil

	raw := bytesTrimSpace(w.Content)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch raw[0] {
	case '"':
		return json.Unmarshal(raw, &m.Content)
	case '[':
		if err := json.Unmarshal(raw, &m.Parts); err != nil {
			return err
		}
		// 便于审计/展示：把首个 text part 回填到 Content。
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				m.Content = p.Text
				break
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported message content JSON")
	}
}

func bytesTrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}
