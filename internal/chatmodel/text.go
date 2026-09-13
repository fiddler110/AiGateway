package chatmodel

import "encoding/json"

// contentPart mirrors an OpenAI multimodal content part, e.g.
// {"type":"text","text":"..."} or {"type":"image_url","image_url":{...}}.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// MessageText extracts the plain-text portion of a message's content, which
// is either a bare JSON string or a list of multimodal content parts (only
// "text" parts contribute; image parts contribute nothing).
func MessageText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err == nil {
		out := ""
		for i, p := range parts {
			if p.Type != "text" {
				continue
			}
			if i > 0 && out != "" {
				out += "\n"
			}
			out += p.Text
		}
		return out
	}
	return ""
}

// toolCallFunction mirrors {"function":{"arguments":"..."}} inside tool_calls.
type toolCallFunction struct {
	Arguments string `json:"arguments"`
}
type toolCall struct {
	Function toolCallFunction `json:"function"`
}

// ToolCallArgumentStrings returns every tool_call[].function.arguments string
// found in a message's Extra fields (both the modern tool_calls array and the
// legacy function_call field).
func ToolCallArgumentStrings(extra map[string]json.RawMessage) []string {
	var out []string
	if raw, ok := extra["tool_calls"]; ok {
		var calls []toolCall
		if err := json.Unmarshal(raw, &calls); err == nil {
			for _, c := range calls {
				if c.Function.Arguments != "" {
					out = append(out, c.Function.Arguments)
				}
			}
		}
	}
	if raw, ok := extra["function_call"]; ok {
		var fc toolCallFunction
		if err := json.Unmarshal(raw, &fc); err == nil && fc.Arguments != "" {
			out = append(out, fc.Arguments)
		}
	}
	return out
}

// MessageScanText is the DLP-relevant superset of a message's text: its
// plain content plus every tool-call argument string, joined with newlines.
// This is what secrets_scanner/pii_redactor/pseudonymizer actually scan —
// without the tool-call portion, agentic tool arguments (paths, commands,
// credentials) would bypass DLP entirely.
func MessageScanText(m ChatMessage) string {
	out := MessageText(m.Content)
	for _, s := range ToolCallArgumentStrings(m.Extra) {
		if out != "" {
			out += "\n"
		}
		out += s
	}
	return out
}
