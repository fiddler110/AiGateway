package handlers

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// responseText collects every model-generated string of one response for
// the response pipeline and writes the pipeline's rewrites back (P0.16).
// The non-streaming handler and buffered streaming share it, so both scan
// and reverse-pseudonymize the same fields the same way: message content,
// reasoning_content, reasoning, refusal (every string field but role), and
// tool-call arguments.
type responseText struct {
	fields  []pipeline.ResponseField
	sinks   []func(string) // sinks[i] receives fields[i]'s rewrite; nil discards it
	after   []func()       // run once every sink has been called
	changed bool
}

func (t *responseText) add(text string, derived bool, sink func(string)) {
	t.fields = append(t.fields, pipeline.ResponseField{Text: text, Derived: derived})
	t.sinks = append(t.sinks, sink)
}

// addString adds a plain string field; set stores its rewrite.
func (t *responseText) addString(text string, set func(string)) {
	t.add(text, false, set)
}

// addStringFields adds every string value of m except role, in key order.
func (t *responseText) addStringFields(m map[string]any) {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if _, ok := v.(string); ok && k != "role" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.addString(m[k].(string), func(s string) { m[k] = s })
	}
}

// addArguments adds tool-call arguments, which are JSON source. When they
// are valid JSON, each string and number token is its own field, scanned
// and rewritten as its decoded value and spliced back re-encoded, so a
// redaction or a restored real value containing `"` or `\` keeps the
// arguments valid JSON (a rewritten number becomes a JSON string). The raw
// source is also scanned, so a pattern spanning a key and its value still
// blocks, but its rewrite is discarded. Arguments that aren't valid JSON
// (e.g. cut off by max_tokens) are scanned and rewritten as raw text.
func (t *responseText) addArguments(args string, set func(string)) {
	tokens, ok := jsonScalars(args)
	if !ok {
		t.addString(args, set)
		return
	}
	t.add(args, false, nil)
	vals := make([]string, len(tokens))
	for i, tok := range tokens {
		vals[i] = tok.value
		t.add(tok.value, true, func(s string) { vals[i] = s })
	}
	t.after = append(t.after, func() {
		var b strings.Builder
		prev, rewritten := 0, false
		for i, tok := range tokens {
			if vals[i] == tok.value {
				continue
			}
			enc, err := marshalJSON(vals[i])
			if err != nil {
				continue // unreachable: encoding a string can't fail
			}
			b.WriteString(args[prev:tok.start])
			b.Write(enc)
			prev, rewritten = tok.end, true
		}
		if rewritten {
			b.WriteString(args[prev:])
			set(b.String())
		}
	})
}

// run runs pipe over the collected fields and, unless it blocked, applies
// the rewrites. changed reports whether any field's text changed.
func (t *responseText) run(ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext) error {
	out, err := pipe.RunResponse(ctx, t.fields, gctx)
	if err != nil || gctx.Blocked {
		return err
	}
	for i, f := range out {
		if f.Text == t.fields[i].Text || t.sinks[i] == nil {
			continue
		}
		t.sinks[i](f.Text)
		t.changed = true
	}
	for _, fn := range t.after {
		fn()
	}
	return nil
}

// jsonScalar is one string or number token of JSON source: src[start:end]
// is the token, value its decoded string (a number's literal text).
type jsonScalar struct {
	start, end int
	value      string
}

// jsonScalars returns the string and number tokens of src (object keys
// included), or ok=false if src isn't valid JSON.
func jsonScalars(src string) (tokens []jsonScalar, ok bool) {
	if !json.Valid([]byte(src)) {
		return nil, false
	}
	for i := 0; i < len(src); {
		switch c := src[i]; {
		case c == '"':
			j := i + 1
			for src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			var v string
			if err := json.Unmarshal([]byte(src[i:j+1]), &v); err != nil {
				return nil, false // unreachable: src is valid JSON
			}
			tokens = append(tokens, jsonScalar{start: i, end: j + 1, value: v})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(src) && strings.IndexByte("+-.eE0123456789", src[j]) >= 0 {
				j++
			}
			tokens = append(tokens, jsonScalar{start: i, end: j, value: src[i:j]})
			i = j
		default:
			i++
		}
	}
	return tokens, true
}
