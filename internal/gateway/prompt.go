package gateway

import (
	"encoding/json"
	"strings"
)

// PromptText flattens a chat or completions request into the text whose prefix
// the worker will cache: model, then tools (templates render them up front),
// then each message in order. It only has to be stable and prefix-preserving —
// a conversation's next turn must extend the previous turn's text — not match
// the model's real chat template.
func PromptText(req map[string]any) string {
	var b strings.Builder
	if m, ok := req["model"].(string); ok {
		b.WriteString(m)
		b.WriteByte(0)
	}
	if tools, ok := req["tools"]; ok {
		// encoding/json sorts map keys, so this is canonical.
		if j, err := json.Marshal(tools); err == nil {
			b.Write(j)
		}
		b.WriteByte(0)
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, raw := range msgs {
			m, _ := raw.(map[string]any)
			role, _ := m["role"].(string)
			b.WriteString(role)
			b.WriteByte(1)
			writeContent(&b, m["content"])
			if tc, ok := m["tool_calls"]; ok {
				if j, err := json.Marshal(tc); err == nil {
					b.Write(j)
				}
			}
			b.WriteByte(2)
		}
	}
	switch p := req["prompt"].(type) {
	case string:
		b.WriteString(p)
	case []any:
		for _, s := range p {
			if str, ok := s.(string); ok {
				b.WriteString(str)
			}
		}
	}
	return b.String()
}

func writeContent(b *strings.Builder, c any) {
	switch v := c.(type) {
	case string:
		b.WriteString(v)
	case []any: // multimodal parts
		for _, p := range v {
			part, _ := p.(map[string]any)
			if t, ok := part["text"].(string); ok {
				b.WriteString(t)
			} else if j, err := json.Marshal(part); err == nil {
				b.Write(j)
			}
		}
	}
}
