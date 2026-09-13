package chatmodel

import "encoding/json"

// MapMessageText applies fn to every text segment of a message's content,
// preserving structure (multimodal image parts are left untouched), and
// returns the new content plus whether anything changed.
func MapMessageText(content json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	if len(content) == 0 {
		return content, false
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		newS := fn(s)
		if newS == s {
			return content, false
		}
		out, _ := json.Marshal(newS)
		return out, true
	}
	var parts []contentPart
	if err := json.Unmarshal(content, &parts); err == nil {
		changed := false
		for i, p := range parts {
			if p.Type != "text" {
				continue
			}
			newText := fn(p.Text)
			if newText != p.Text {
				parts[i].Text = newText
				changed = true
			}
		}
		if !changed {
			return content, false
		}
		out, _ := json.Marshal(parts)
		return out, true
	}
	return content, false
}

// mapJSONStrings recursively applies fn to every string value inside an
// arbitrary decoded JSON value (map/slice/string), leaving other types as-is.
func mapJSONStrings(v any, fn func(string) string) (any, bool) {
	switch t := v.(type) {
	case string:
		newS := fn(t)
		return newS, newS != t
	case map[string]any:
		changed := false
		for k, val := range t {
			newVal, ch := mapJSONStrings(val, fn)
			if ch {
				t[k] = newVal
				changed = true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, val := range t {
			newVal, ch := mapJSONStrings(val, fn)
			if ch {
				t[i] = newVal
				changed = true
			}
		}
		return t, changed
	default:
		return v, false
	}
}

// mapArgumentsString parses a tool-call arguments JSON string, applies fn to
// every string value inside it, and re-serializes only if something changed.
// Falls back to treating the whole blob as opaque text if it doesn't parse.
func mapArgumentsString(args string, fn func(string) string) string {
	var parsed any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return fn(args)
	}
	newParsed, changed := mapJSONStrings(parsed, fn)
	if !changed {
		return args
	}
	out, err := json.Marshal(newParsed)
	if err != nil {
		return args
	}
	return string(out)
}

// MapMessageScanText applies fn to a message's content AND recursively to
// every string value inside its tool-call argument JSON, returning updated
// Extra fields (tool_calls/function_call) when arguments changed.
func MapMessageScanText(m *ChatMessage, fn func(string) string) {
	if newContent, changed := MapMessageText(m.Content, fn); changed {
		m.Content = newContent
	}
	if raw, ok := m.Extra["tool_calls"]; ok {
		var calls []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &calls); err == nil {
			changedAny := false
			for i, c := range calls {
				fnRaw, ok := c["function"]
				if !ok {
					continue
				}
				var f map[string]json.RawMessage
				if err := json.Unmarshal(fnRaw, &f); err != nil {
					continue
				}
				argsRaw, ok := f["arguments"]
				if !ok {
					continue
				}
				var args string
				if err := json.Unmarshal(argsRaw, &args); err != nil {
					continue
				}
				newArgs := mapArgumentsString(args, fn)
				if newArgs != args {
					newArgsJSON, _ := json.Marshal(newArgs)
					f["arguments"] = newArgsJSON
					newFJSON, _ := json.Marshal(f)
					calls[i]["function"] = newFJSON
					changedAny = true
				}
			}
			if changedAny {
				newCallsJSON, _ := json.Marshal(calls)
				m.Extra["tool_calls"] = newCallsJSON
			}
		}
	}
}
