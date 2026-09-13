package handlers

import (
	"bufio"
	"bytes"
	"context"
	"maps"
	"sort"
	"strings"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// collapsedStream is a buffered upstream SSE stream collapsed to one message
// per choice, so the response pipeline can rewrite each choice's content
// before any of it is re-emitted.
type collapsedStream struct {
	template   map[string]any // top-level fields (id, model, created, ...) of the first chunk
	choices    map[int]*collapsedChoice
	usage      any
	usageAlone bool // upstream sent usage in a chunk with no choices
	// errorEvents counts upstream in-stream error payloads. They are never
	// re-emitted: their text is unscanned upstream output (P0.16, P0.8).
	errorEvents int
}

type collapsedChoice struct {
	role   string
	fields map[string]*strings.Builder // string delta fields, content included
	calls  map[int]*collapsedCall
	finish any
}

type collapsedCall struct {
	id, typ, name string
	args          strings.Builder
}

// collapseStream parses raw upstream SSE. Data payloads that aren't JSON
// objects are dropped: in buffer mode nothing the pipeline didn't see may
// reach the client. A line over the scanner cap is an error (bufio.ErrTooLong)
// rather than a silent stop, so a truncated stream is never re-emitted (P0.9).
func collapseStream(raw []byte) (*collapsedStream, error) {
	s := &collapsedStream{choices: map[int]*collapsedChoice{}}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		payload, ok := ssePayload(scanner.Bytes())
		if !ok || len(payload) == 0 || string(payload) == "[DONE]" {
			continue
		}
		chunk, err := decodeChunk(payload)
		if err != nil {
			continue
		}
		if _, isErr := chunk["error"]; isErr {
			s.errorEvents++
			continue
		}
		if s.template == nil {
			s.template = topLevel(chunk)
		}
		choices, _ := chunk["choices"].([]any)
		if u := chunk["usage"]; u != nil {
			s.usage, s.usageAlone = u, len(choices) == 0
		}
		for pos, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				s.add(indexOf(choice["index"], pos), choice)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *collapsedStream) add(idx int, choice map[string]any) {
	c := s.choices[idx]
	if c == nil {
		c = &collapsedChoice{fields: map[string]*strings.Builder{}, calls: map[int]*collapsedCall{}}
		s.choices[idx] = c
	}
	if f := choice["finish_reason"]; f != nil {
		c.finish = f
	}
	delta, _ := choice["delta"].(map[string]any)
	for k, v := range delta {
		str, ok := v.(string)
		switch {
		case !ok:
		case k == "role":
			if str != "" {
				c.role = str
			}
		default:
			b := c.fields[k]
			if b == nil {
				b = &strings.Builder{}
				c.fields[k] = b
			}
			b.WriteString(str)
		}
	}
	calls, _ := delta["tool_calls"].([]any)
	for pos, tc := range calls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		ci := indexOf(call["index"], pos)
		cc := c.calls[ci]
		if cc == nil {
			cc = &collapsedCall{}
			c.calls[ci] = cc
		}
		if v, _ := call["id"].(string); v != "" {
			cc.id = v
		}
		if v, _ := call["type"].(string); v != "" {
			cc.typ = v
		}
		fn, _ := call["function"].(map[string]any)
		if v, _ := fn["name"].(string); v != "" {
			cc.name = v
		}
		if v, ok := fn["arguments"].(string); ok {
			cc.args.WriteString(v)
		}
	}
}

func (s *collapsedStream) indexes() []int {
	idxs := make([]int, 0, len(s.choices))
	for idx := range s.choices {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	return idxs
}

// responseText collects every string delta field (content, reasoning,
// refusal, ...) and every tool call's arguments of every choice, in index
// order, rewriting the stream in place when the pipeline runs. This is the
// same field set the non-streaming handler scans, so the response pipeline
// (pseudonym reversal included) treats both identically (P0.16).
func (s *collapsedStream) responseText() *responseText {
	rt := &responseText{}
	for _, idx := range s.indexes() {
		c := s.choices[idx]
		keys := make([]string, 0, len(c.fields))
		for k := range c.fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rt.addString(c.fields[k].String(), func(v string) {
				b := &strings.Builder{}
				b.WriteString(v)
				c.fields[k] = b
			})
		}
		for _, ci := range c.callOrder() {
			cc := c.calls[ci]
			rt.addArguments(cc.args.String(), func(v string) {
				cc.args.Reset()
				cc.args.WriteString(v)
			})
		}
	}
	return rt
}

// runResponsePipeline runs the response pipeline once over the whole
// stream's fields, stopping at a block. Accounting middleware finalizes even
// when the stream has no text at all.
func (s *collapsedStream) runResponsePipeline(ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext) error {
	return s.responseText().run(ctx, pipe, gctx)
}

// sse re-emits the stream as OpenAI chat.completion.chunk events: for each
// choice one chunk with its whole delta, then one with its finish_reason
// (usage rides on the last of those unless upstream sent it separately), then
// [DONE]. Every string it emits other than role, tool call ids, names, and
// types has been through the response pipeline, which also reverses
// pseudonymization. Token-level chunk boundaries, logprobs, and non-string
// delta fields other than tool_calls are not preserved.
func (s *collapsedStream) sse() []byte {
	var out bytes.Buffer
	emit := func(choices []any, usage any) {
		chunk := maps.Clone(s.template)
		if chunk == nil {
			chunk = map[string]any{}
		}
		chunk["choices"] = choices
		if usage != nil {
			chunk["usage"] = usage
		}
		data, err := marshalJSON(chunk)
		if err != nil {
			return // unreachable: every value came from decoding JSON
		}
		out.WriteString("data: ")
		out.Write(data)
		out.WriteString("\n\n")
	}
	choice := func(idx int, delta map[string]any, finish any) []any {
		return []any{map[string]any{"index": idx, "delta": delta, "finish_reason": finish}}
	}

	idxs := s.indexes()
	var finished []int
	for _, idx := range idxs {
		c := s.choices[idx]
		if c.finish != nil {
			finished = append(finished, idx)
		}
		delta := map[string]any{}
		if c.role != "" {
			delta["role"] = c.role
		}
		for k, b := range c.fields {
			delta[k] = b.String()
		}
		if len(c.calls) > 0 {
			delta["tool_calls"] = c.toolCalls()
		}
		if len(delta) > 0 {
			emit(choice(idx, delta, nil), nil)
		}
	}

	usage := s.usage
	for i, idx := range finished {
		var u any
		if i == len(finished)-1 && !s.usageAlone {
			u, usage = usage, nil
		}
		emit(choice(idx, map[string]any{}, s.choices[idx].finish), u)
	}
	if usage != nil {
		emit([]any{}, usage)
	}

	out.WriteString("data: [DONE]\n\n")
	return out.Bytes()
}

// callOrder returns the choice's tool call indexes in ascending order.
func (c *collapsedChoice) callOrder() []int {
	order := make([]int, 0, len(c.calls))
	for ci := range c.calls {
		order = append(order, ci)
	}
	sort.Ints(order)
	return order
}

func (c *collapsedChoice) toolCalls() []any {
	order := c.callOrder()
	calls := make([]any, 0, len(order))
	for _, ci := range order {
		cc := c.calls[ci]
		fn := map[string]any{"arguments": cc.args.String()}
		if cc.name != "" {
			fn["name"] = cc.name
		}
		call := map[string]any{"index": ci, "function": fn}
		if cc.id != "" {
			call["id"] = cc.id
		}
		if cc.typ != "" {
			call["type"] = cc.typ
		}
		calls = append(calls, call)
	}
	return calls
}
