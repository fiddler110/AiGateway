// Package chatmodel defines the canonical OpenAI-shaped chat request/response
// types that the entire gateway (middleware, cache, providers) operates on.
package chatmodel

import "encoding/json"

// ChatMessage mirrors an OpenAI chat message. Unknown fields (tool_calls,
// tool_call_id, function_call, and any future provider-specific fields) are
// preserved verbatim in Extra so multi-turn tool-call transcripts round-trip
// losslessly through the gateway.
type ChatMessage struct {
	Role    string                     `json:"role"`
	Content json.RawMessage            `json:"content,omitempty"`
	Name    string                     `json:"name,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// knownMessageFields lists the JSON keys handled explicitly above; everything
// else is captured into Extra by the custom UnmarshalJSON/MarshalJSON pair.
var knownMessageFields = map[string]struct{}{
	"role": {}, "content": {}, "name": {},
}

func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["role"]; ok {
		if err := json.Unmarshal(v, &m.Role); err != nil {
			return err
		}
	}
	if v, ok := raw["content"]; ok {
		m.Content = v
	}
	if v, ok := raw["name"]; ok {
		if err := json.Unmarshal(v, &m.Name); err != nil {
			return err
		}
	}
	m.Extra = map[string]json.RawMessage{}
	for k, v := range raw {
		if _, known := knownMessageFields[k]; !known {
			m.Extra[k] = v
		}
	}
	return nil
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range m.Extra {
		out[k] = v
	}
	roleJSON, err := json.Marshal(m.Role)
	if err != nil {
		return nil, err
	}
	out["role"] = roleJSON
	if m.Content != nil {
		out["content"] = m.Content
	}
	if m.Name != "" {
		nameJSON, err := json.Marshal(m.Name)
		if err != nil {
			return nil, err
		}
		out["name"] = nameJSON
	}
	return json.Marshal(out)
}

// ChatRequest mirrors an OpenAI /v1/chat/completions request body. Unknown
// top-level fields (tools, tool_choice, thinking, etc.) are preserved in Extra.
type ChatRequest struct {
	Model       string                     `json:"model"`
	Messages    []ChatMessage              `json:"messages"`
	Stream      bool                       `json:"stream,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"`
	MaxTokens   *int                       `json:"max_tokens,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

var knownRequestFields = map[string]struct{}{
	"model": {}, "messages": {}, "stream": {}, "temperature": {}, "max_tokens": {},
}

func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &r.Model); err != nil {
			return err
		}
	}
	if v, ok := raw["messages"]; ok {
		if err := json.Unmarshal(v, &r.Messages); err != nil {
			return err
		}
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &r.Stream); err != nil {
			return err
		}
	}
	if v, ok := raw["temperature"]; ok {
		if err := json.Unmarshal(v, &r.Temperature); err != nil {
			return err
		}
	}
	if v, ok := raw["max_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return err
		}
	}
	r.Extra = map[string]json.RawMessage{}
	for k, v := range raw {
		if _, known := knownRequestFields[k]; !known {
			r.Extra[k] = v
		}
	}
	return nil
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	for k, v := range r.Extra {
		out[k] = v
	}
	modelJSON, err := json.Marshal(r.Model)
	if err != nil {
		return nil, err
	}
	out["model"] = modelJSON
	msgsJSON, err := json.Marshal(r.Messages)
	if err != nil {
		return nil, err
	}
	out["messages"] = msgsJSON
	if r.Stream {
		out["stream"] = json.RawMessage("true")
	}
	if r.Temperature != nil {
		v, err := json.Marshal(r.Temperature)
		if err != nil {
			return nil, err
		}
		out["temperature"] = v
	}
	if r.MaxTokens != nil {
		v, err := json.Marshal(r.MaxTokens)
		if err != nil {
			return nil, err
		}
		out["max_tokens"] = v
	}
	return json.Marshal(out)
}
