package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"sort"

	"github.com/scottymacleod/aigateway/internal/pseudomap"
)

// streamReverser undoes context_pseudonymizer's substitutions (fake -> real)
// in text that may arrive split across stream chunks. It scans once,
// leftmost-longest (pseudomap.Matcher, shared with the middleware), so a
// restored real value is never itself rescanned.
type streamReverser struct {
	m      *pseudomap.Matcher
	maxLen int
}

// newStreamReverser returns nil when there is nothing to reverse; a nil
// *streamReverser returns text unchanged. With jsonText set, fakes and real
// values are matched and written JSON-string-escaped, for fields whose value
// is itself JSON source (tool-call arguments), so a real value containing `"`
// or `\` stays a valid string literal there.
func newStreamReverser(reverse map[string]string, jsonText bool) *streamReverser {
	mapping := reverse
	if jsonText {
		mapping = make(map[string]string, len(reverse))
		for fake, real := range reverse {
			mapping[jsonEscape(fake)] = jsonEscape(real)
		}
	}
	m := pseudomap.New(mapping)
	if m == nil {
		return nil
	}
	return &streamReverser{m: m, maxLen: m.MaxLen()}
}

// reverse replaces every fake in text with its real value. Unless final is
// set, it holds back a trailing proper prefix of some fake for the caller to
// prepend to the next piece (see pseudomap.Matcher.Replace). held is shorter
// than the longest fake and always starts on a UTF-8 rune boundary.
func (r *streamReverser) reverse(text string, final bool) (out, held string) {
	if r == nil {
		return text, ""
	}
	return r.m.Replace(text, final)
}

// passthroughReverser reverse-pseudonymizes a passthrough stream one SSE
// line at a time, on decoded chunks rather than wire bytes. Every string
// field of a choice's delta is its own text stream, as is each tool call's
// arguments. Text that could be the start of a fake is held back until the
// next chunk for that field, the choice's finish_reason, [DONE], or EOF.
type passthroughReverser struct {
	text, args *streamReverser
	held       map[heldKey]string
	template   map[string]any // top-level fields of the latest chunk, for a synthesized flush chunk
}

type heldKey struct {
	choice int
	tool   bool
	call   int    // tool call index, when tool
	field  string // delta field name, when !tool
}

// newPassthroughReverser returns nil when the request has no pseudonyms to
// reverse; a nil *passthroughReverser forwards lines untouched.
func newPassthroughReverser(reverse map[string]string) *passthroughReverser {
	text := newStreamReverser(reverse, false)
	if text == nil {
		return nil
	}
	return &passthroughReverser{
		text: text,
		args: newStreamReverser(reverse, true),
		held: map[heldKey]string{},
	}
}

// rewriteLine returns what to send in place of one upstream SSE line,
// without its trailing newline. Lines that aren't decodable data payloads go
// out unchanged. Anything still held is released in a chunk before [DONE].
func (p *passthroughReverser) rewriteLine(line []byte) []byte {
	if p == nil {
		return line
	}
	payload, ok := ssePayload(line)
	if !ok {
		return line
	}
	if string(payload) == "[DONE]" {
		if tail := p.finish(); tail != nil {
			return []byte("data: " + string(tail) + "\n\n" + string(line))
		}
		return line
	}
	out, err := p.rewrite(payload)
	if err != nil {
		return line
	}
	return append([]byte("data: "), out...)
}

func (p *passthroughReverser) rewrite(payload []byte) ([]byte, error) {
	chunk, err := decodeChunk(payload)
	if err != nil {
		return nil, err
	}
	choices, _ := chunk["choices"].([]any)
	for pos, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		idx := indexOf(choice["index"], pos)
		final := choice["finish_reason"] != nil
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta = map[string]any{}
		}
		for field, v := range delta {
			if s, ok := v.(string); ok && field != "role" {
				delta[field] = p.feed(heldKey{choice: idx, field: field}, s, final)
			}
		}
		calls, _ := delta["tool_calls"].([]any)
		for cpos, tc := range calls {
			call, _ := tc.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if s, ok := fn["arguments"].(string); ok {
				fn["arguments"] = p.feed(heldKey{choice: idx, tool: true, call: indexOf(call["index"], cpos)}, s, final)
			}
		}
		if final {
			p.flushInto(idx, delta)
		}
		if len(delta) > 0 {
			choice["delta"] = delta
		}
	}
	p.template = topLevel(chunk)
	return marshalJSON(chunk)
}

func (p *passthroughReverser) feed(k heldKey, s string, final bool) string {
	rev := p.text
	if k.tool {
		rev = p.args
	}
	out, held := rev.reverse(p.held[k]+s, final)
	if held == "" {
		delete(p.held, k)
	} else {
		p.held[k] = held
	}
	return out
}

// flushInto releases everything held for choice idx into delta.
func (p *passthroughReverser) flushInto(idx int, delta map[string]any) {
	var keys []heldKey
	for k := range p.held {
		if k.choice == idx {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.tool != b.tool {
			return !a.tool
		}
		if a.call != b.call {
			return a.call < b.call
		}
		return a.field < b.field
	})
	var calls []any
	for _, k := range keys {
		s := p.feed(k, "", true)
		if k.tool {
			calls = append(calls, map[string]any{"index": k.call, "function": map[string]any{"arguments": s}})
			continue
		}
		prev, _ := delta[k.field].(string)
		delta[k.field] = prev + s
	}
	if len(calls) > 0 {
		existing, _ := delta["tool_calls"].([]any)
		delta["tool_calls"] = append(existing, calls...)
	}
}

// finish returns a chunk releasing everything still held, or nil if nothing
// is. Call it at [DONE] (rewriteLine does) and at EOF.
func (p *passthroughReverser) finish() []byte {
	if p == nil || len(p.held) == 0 {
		return nil
	}
	var idxs []int
	for k := range p.held {
		if !slices.Contains(idxs, k.choice) {
			idxs = append(idxs, k.choice)
		}
	}
	sort.Ints(idxs)
	choices := make([]any, 0, len(idxs))
	for _, idx := range idxs {
		delta := map[string]any{}
		p.flushInto(idx, delta)
		choices = append(choices, map[string]any{"index": idx, "delta": delta, "finish_reason": nil})
	}
	chunk := maps.Clone(p.template)
	if chunk == nil {
		chunk = map[string]any{}
	}
	chunk["choices"] = choices
	out, err := marshalJSON(chunk)
	if err != nil {
		return nil
	}
	return out
}

// ssePayload returns the trimmed value of an SSE "data:" line.
func ssePayload(line []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil, false
	}
	return bytes.TrimSpace(rest), true
}

// decodeChunk decodes one JSON object, keeping numbers as json.Number so
// re-encoding doesn't alter them.
func decodeChunk(payload []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var chunk map[string]any
	if err := dec.Decode(&chunk); err != nil {
		return nil, err
	}
	if chunk == nil {
		return nil, errors.New("chunk is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after chunk")
	}
	return chunk, nil
}

// topLevel returns chunk's fields other than choices and usage.
func topLevel(chunk map[string]any) map[string]any {
	t := maps.Clone(chunk)
	delete(t, "choices")
	delete(t, "usage")
	return t
}

func indexOf(v any, def int) int {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}

func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// jsonEscape returns s as it appears inside a JSON string literal.
func jsonEscape(s string) string {
	b, _ := marshalJSON(s)
	return string(b[1 : len(b)-1])
}
