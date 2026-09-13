package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/middleware/tokencounter"
	"github.com/scottymacleod/aigateway/internal/testutil"
)

// modelUpstream is an OpenAI response whose message carries the given string
// fields (content, reasoning_content, refusal, ...) and, when args isn't
// empty, one tool call with those arguments. Streamed, every field and the
// arguments are split in half across two delta chunks.
func modelUpstream(stream bool, fields map[string]string, args string) testutil.Response {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if !stream {
		msg := map[string]any{"role": "assistant"}
		for _, k := range keys {
			msg[k] = fields[k]
		}
		if _, ok := fields["content"]; !ok {
			msg["content"] = nil
		}
		if args != "" {
			msg["tool_calls"] = []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "act", "arguments": args},
			}}
		}
		return testutil.JSONResponse(http.StatusOK, map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion", "created": 1700000000, "model": "fake-model",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12},
		})
	}

	deltas := []map[string]any{{"role": "assistant"}}
	for _, k := range keys {
		v := fields[k]
		deltas = append(deltas, map[string]any{k: v[:len(v)/2]}, map[string]any{k: v[len(v)/2:]})
	}
	if args != "" {
		cut := len(args) / 2
		deltas = append(deltas,
			map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "act", "arguments": args[:cut]}}}},
			map[string]any{"tool_calls": []any{map[string]any{"index": 0,
				"function": map[string]any{"arguments": args[cut:]}}}},
		)
	}
	return testutil.OpenAISSEDeltas(deltas...)
}

// modelOutput is what a client sees of one choice: its string fields, tool
// call 0's arguments, or the code of the error it got instead.
type modelOutput struct {
	fields  map[string]string
	args    string
	errCode int
	raw     string
}

func postForOutput(t *testing.T, gw *testutil.Gateway, body string, stream bool) modelOutput {
	t.Helper()
	res := gw.PostChat(t, body)
	out := modelOutput{fields: map[string]string{}, raw: res.Body}
	collect := func(m map[string]any) {
		for k, v := range m {
			if s, ok := v.(string); ok && k != "role" {
				out.fields[k] += s
			}
		}
		calls, _ := m["tool_calls"].([]any)
		for _, tc := range calls {
			fn, _ := tc.(map[string]any)["function"].(map[string]any)
			a, _ := fn["arguments"].(string)
			out.args += a
		}
	}

	if !stream {
		if res.Status != http.StatusOK {
			out.errCode = decodeError(t, res.Body).Error.Code
			return out
		}
		var resp struct {
			Choices []struct{ Message map[string]any }
		}
		if err := json.Unmarshal([]byte(res.Body), &resp); err != nil || len(resp.Choices) != 1 {
			t.Fatalf("bad response %q: %v", res.Body, err)
		}
		collect(resp.Choices[0].Message)
		return out
	}

	if res.Status != http.StatusOK {
		t.Fatalf("status %d, body %q", res.Status, res.Body)
	}
	for _, line := range strings.Split(res.Body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("invalid JSON in stream: %v; payload %q", err, payload)
		}
		if _, ok := chunk["error"]; ok {
			out.errCode = decodeError(t, payload).Error.Code
			continue
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			delta, _ := c.(map[string]any)["delta"].(map[string]any)
			collect(delta)
		}
	}
	return out
}

var responseModes = []struct {
	name   string
	stream bool
}{{"non-stream", false}, {"buffered stream", true}}

// P0.16: the response pipeline scans and rewrites tool-call arguments,
// reasoning_content, and refusal, not just content, in non-streaming and
// buffered streaming responses; rewritten arguments stay valid JSON.
func TestChatResponsePipelineCoversAllModelFields(t *testing.T) {
	const secret = "sk-abcdefghijklmnopqrstuvwxyz012345"
	cases := []struct {
		name       string
		middleware string
		fields     map[string]string
		args       string
		leak       string
		wantBlock  bool
		wantFields map[string]string
		wantArgs   map[string]any
	}{
		{
			name: "email in tool arguments", middleware: "pii_redactor",
			fields:   map[string]string{"content": "sending"},
			args:     `{"to": "bob@example.com", "count": 2, "note": "say \"hi\""}`,
			leak:     "bob@example.com",
			wantArgs: map[string]any{"to": "[EMAIL]", "count": 2.0, "note": `say "hi"`},
		},
		{
			name: "phone number as a JSON number in tool arguments", middleware: "pii_redactor",
			args:     `{"phone":5551234567}`,
			leak:     "5551234567",
			wantArgs: map[string]any{"phone": "[PHONE]"},
		},
		{
			name: "email in reasoning_content", middleware: "pii_redactor",
			fields:     map[string]string{"content": "done", "reasoning_content": "should mail bob@example.com first"},
			leak:       "bob@example.com",
			wantFields: map[string]string{"content": "done", "reasoning_content": "should mail [EMAIL] first"},
		},
		{
			name: "email in refusal", middleware: "pii_redactor",
			fields:     map[string]string{"refusal": "ask bob@example.com instead"},
			leak:       "bob@example.com",
			wantFields: map[string]string{"refusal": "ask [EMAIL] instead"},
		},
		{
			name: "secret in tool arguments", middleware: "secrets_scanner",
			fields: map[string]string{"content": "here"},
			args:   `{"api_key":"` + secret + `"}`,
			leak:   secret, wantBlock: true,
		},
		{
			name: "secret pattern spanning an arguments key and value", middleware: "secrets_scanner",
			args: `{"aws_secret_access_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`,
			leak: "wJalrXUtnFEMI", wantBlock: true,
		},
		{
			name: "secret in reasoning_content", middleware: "secrets_scanner",
			fields: map[string]string{"content": "ok", "reasoning_content": "the key is " + secret},
			leak:   secret, wantBlock: true,
		},
		{
			name: "secret in refusal", middleware: "secrets_scanner",
			fields: map[string]string{"refusal": "not sharing " + secret},
			leak:   secret, wantBlock: true,
		},
	}
	for _, mode := range responseModes {
		for _, tc := range cases {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				fake := testutil.NewFakeUpstream(t, modelUpstream(mode.stream, tc.fields, tc.args))
				gw := streamGateway(t, fake, true, tc.middleware)
				out := postForOutput(t, gw, chatBody(mode.stream), mode.stream)

				if strings.Contains(out.raw, tc.leak) {
					t.Errorf("%q reached the client: %q", tc.leak, out.raw)
				}
				if tc.wantBlock {
					if out.errCode != http.StatusBadRequest {
						t.Errorf("error code %d, want a 400 block; body %q", out.errCode, out.raw)
					}
					return
				}
				if out.errCode != 0 {
					t.Fatalf("unexpected error %d: %q", out.errCode, out.raw)
				}
				for k, want := range tc.wantFields {
					if out.fields[k] != want {
						t.Errorf("%s = %q, want %q", k, out.fields[k], want)
					}
				}
				if tc.wantArgs != nil {
					var got map[string]any
					if err := json.Unmarshal([]byte(out.args), &got); err != nil {
						t.Fatalf("tool arguments %q aren't valid JSON: %v", out.args, err)
					}
					if fmt.Sprint(got) != fmt.Sprint(tc.wantArgs) {
						t.Errorf("tool arguments %v, want %v", got, tc.wantArgs)
					}
				}
			})
		}
	}
}

// P0.16: non-streaming responses reverse pseudonyms in tool-call arguments
// (and reasoning_content), keeping arguments valid JSON when the real value
// contains `"` and `\`.
func TestChatNonStreamReversesPseudonymsInToolArguments(t *testing.T) {
	prompt := "the password is " + realSecret
	fake := fakeFor(t, prompt, realSecret)
	upstream := modelUpstream(false,
		map[string]string{"content": "logging in", "reasoning_content": "use " + fake},
		`{"user":"admin","password":"`+fake+`"}`)
	gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), false, "context_pseudonymizer")

	content, _ := json.Marshal(prompt)
	out := postForOutput(t, gw, fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s}]}`, content), false)
	if out.errCode != 0 {
		t.Fatalf("error %d: %q", out.errCode, out.raw)
	}
	var args struct{ User, Password string }
	if err := json.Unmarshal([]byte(out.args), &args); err != nil {
		t.Fatalf("tool arguments %q aren't valid JSON: %v", out.args, err)
	}
	if args.Password != realSecret || args.User != "admin" {
		t.Errorf("arguments %+v, want password %q", args, realSecret)
	}
	if out.fields["reasoning_content"] != "use "+realSecret {
		t.Errorf("reasoning_content %q", out.fields["reasoning_content"])
	}
	if strings.Contains(out.raw, fake) {
		t.Errorf("fake %q reached the client: %q", fake, out.raw)
	}
}

// P0.16: token accounting counts a response once however many fields it
// has, including a tool-call-only response, in every response mode; without
// provider usage it estimates from every field, not just content.
func TestChatResponseAccountingCountedOnce(t *testing.T) {
	modes := []struct {
		name             string
		stream, buffered bool
	}{{"non-stream", false, false}, {"buffered stream", true, true}, {"passthrough stream", true, false}}
	shapes := []struct {
		name   string
		fields map[string]string
		args   string
	}{
		{"content, reasoning, and tool call", map[string]string{"content": "hello", "reasoning_content": "thinking"}, `{"a":"b"}`},
		{"tool call only", nil, `{"city":"Paris"}`},
	}
	for _, mode := range modes {
		for _, shape := range shapes {
			t.Run(mode.name+"/"+shape.name, func(t *testing.T) {
				upstream := modelUpstream(mode.stream, shape.fields, shape.args)
				gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), mode.buffered, "token_counter")
				if res := gw.PostChat(t, chatBody(mode.stream)); res.Status != http.StatusOK {
					t.Fatalf("status %d, body %q", res.Status, res.Body)
				}
				// Estimates reconcile to provider usage: prompt + completion.
				want := 5 + 7
				if mode.stream {
					want = 5 + len(upstream.Chunks) - 2
				}
				if got := tokenTotal(t, gw); got != want {
					t.Errorf("token total %d, want %d (prompt + completion, once)", got, want)
				}
			})
		}
	}

	t.Run("non-stream estimate without usage", func(t *testing.T) {
		reasoning := strings.Repeat("r", 400)
		body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("c", 40) +
			`","reasoning_content":"` + reasoning + `"},"finish_reason":"stop"}]}`
		upstream := testutil.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: body}
		gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), false, "token_counter")
		if res := gw.PostChat(t, chatBody(false)); res.Status != http.StatusOK {
			t.Fatalf("status %d, body %q", res.Status, res.Body)
		}
		// Request "hi" estimates to 1 token; the response's 440 chars to 110.
		if got := tokenTotal(t, gw); got != 1+110 {
			t.Errorf("token total %d, want %d", got, 1+110)
		}
	})
}

func tokenTotal(t *testing.T, gw *testutil.Gateway) int {
	t.Helper()
	for _, e := range gw.Srv.CurrentState().Pipeline.Entries {
		if tc, ok := e.MW.(*tokencounter.Middleware); ok {
			total := 0
			for _, n := range tc.Totals() {
				total += n
			}
			return total
		}
	}
	t.Fatal("token_counter not in pipeline")
	return 0
}

// P0.16: buffered streaming never forwards an upstream in-stream error
// payload (unscanned upstream text); the client gets a generic error event.
func TestChatBufferedStreamHidesUpstreamErrorEvents(t *testing.T) {
	const detail = "model crashed near 10.9.8.7 while writing bob@example.com"
	upstream := modelUpstream(true, map[string]string{"content": "partial answer"}, "")
	errFrame, _ := json.Marshal(map[string]any{"error": map[string]any{"message": detail, "type": "server_error"}})
	upstream.Chunks = append(deltaFrames(upstream),
		testutil.Chunk{Data: "data: " + string(errFrame) + "\n\n"},
		testutil.Chunk{Data: "data: [DONE]\n\n"})

	for _, mw := range []string{"", "pii_redactor"} {
		t.Run("middleware="+mw, func(t *testing.T) {
			gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), true, mw)
			res := gw.PostChat(t, chatBody(true))
			if res.Status != http.StatusOK {
				t.Fatalf("status %d, body %q", res.Status, res.Body)
			}
			for _, leak := range []string{"crashed", "10.9.8.7", "bob@", "server_error", "partial answer"} {
				if strings.Contains(res.Body, leak) {
					t.Errorf("client body contains %q: %q", leak, res.Body)
				}
			}
			assertStreamEndsWithError(t, res.Body, http.StatusBadGateway)
		})
	}
}
