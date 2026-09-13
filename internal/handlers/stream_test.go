package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/middleware/pseudonymizer"
	"github.com/scottymacleod/aigateway/internal/pipeline"
	"github.com/scottymacleod/aigateway/internal/testutil"
)

// realSecret contains both characters that break JSON when substituted into
// wire bytes unescaped.
const realSecret = `Pa"ss\word`

var pseudonymizerConfig = map[string]any{"sensitive_strings": []any{realSecret}}

func streamGateway(t *testing.T, fake *testutil.FakeUpstream, buffered bool, middleware string) *testutil.Gateway {
	t.Helper()
	// middleware_config for a middleware not in middleware: is rejected.
	mwConfig := ""
	if strings.Contains(middleware, "context_pseudonymizer") {
		mwConfig = "middleware_config:\n  context_pseudonymizer:\n    sensitive_strings: ['Pa\"ss\\word']\n"
	}
	return testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
  stream_buffer: %t
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
middleware: [%s]
%s`, fake.URL, buffered, middleware, mwConfig))
}

// fakeFor returns the fake context_pseudonymizer assigns real in prompt.
// Fakes are deterministic, so a fresh gateway assigns the same one.
func fakeFor(t *testing.T, prompt, real string) string {
	t.Helper()
	mw, err := pseudonymizer.New(pseudonymizerConfig)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(prompt)
	req := &chatmodel.ChatRequest{Messages: []chatmodel.ChatMessage{{Role: "user", Content: content}}}
	gctx := pipeline.NewGatewayContext("", "fake", "")
	if err := mw.Process(context.Background(), req, gctx); err != nil {
		t.Fatal(err)
	}
	fake := gctx.Scratch.PseudonymMap[real]
	if fake == "" {
		t.Fatalf("%q was not pseudonymized", real)
	}
	return fake
}

func userBody(t *testing.T, prompt string) string {
	t.Helper()
	content, _ := json.Marshal(prompt)
	return fmt.Sprintf(`{"model":"m","stream":true,"messages":[{"role":"user","content":%s}]}`, content)
}

// clientStream is what an OpenAI client reconstructs from a stream.
type clientStream struct {
	content  string
	toolArgs map[int]string
	toolName map[int]string
	ids      map[string]bool
	models   map[string]bool
	finish   string
	usage    bool
}

// readStream parses a gateway SSE body, failing on any data payload that
// isn't valid JSON or a stream that doesn't end with [DONE].
func readStream(t *testing.T, body string) clientStream {
	t.Helper()
	cs := clientStream{toolArgs: map[int]string{}, toolName: map[int]string{}, ids: map[string]bool{}, models: map[string]bool{}}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream doesn't end with [DONE]: %q", body)
	}
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Error   any    `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("invalid JSON in stream: %v; payload %q", err, payload)
		}
		if chunk.Error != nil {
			t.Fatalf("stream carries an error event: %q", payload)
		}
		cs.ids[chunk.ID] = true
		cs.models[chunk.Model] = true
		cs.usage = cs.usage || chunk.Usage != nil
		for _, c := range chunk.Choices {
			cs.content += c.Delta.Content
			for _, tc := range c.Delta.ToolCalls {
				cs.toolArgs[tc.Index] += tc.Function.Arguments
				if tc.Function.Name != "" {
					cs.toolName[tc.Index] = tc.Function.Name
				}
			}
			if c.FinishReason != nil {
				cs.finish = *c.FinishReason
			}
		}
	}
	return cs
}

var streamModes = []struct {
	name     string
	buffered bool
}{{"passthrough", false}, {"buffered", true}}

// P0.4(a): a fake split across delta chunks is still reversed, at every
// split point, in both stream modes.
func TestChatStreamReversesSplitPseudonyms(t *testing.T) {
	const prompt = "connect to 10.0.0.5 for the db"
	fake := fakeFor(t, prompt, "10.0.0.5")

	type split struct {
		name   string
		deltas []string
	}
	var splits []split
	for i := 1; i < len(fake); i++ {
		splits = append(splits, split{fmt.Sprintf("at %d", i), []string{"db is at " + fake[:i], fake[i:] + ", done"}})
	}
	perByte := []string{"db is at "}
	for i := range len(fake) {
		perByte = append(perByte, fake[i:i+1])
	}
	splits = append(splits, split{"one byte per chunk", append(perByte, ", done")})

	for _, mode := range streamModes {
		for _, sp := range splits {
			t.Run(mode.name+"/"+sp.name, func(t *testing.T) {
				gw := streamGateway(t, testutil.NewFakeUpstream(t, testutil.OpenAISSE(sp.deltas...)), mode.buffered, "context_pseudonymizer")
				res := gw.PostChat(t, userBody(t, prompt))
				if res.Status != http.StatusOK {
					t.Fatalf("status %d, body %q", res.Status, res.Body)
				}
				cs := readStream(t, res.Body)
				if cs.content != "db is at 10.0.0.5, done" {
					t.Errorf("content %q", cs.content)
				}
				if strings.Contains(res.Body, fake) {
					t.Errorf("fake %q reached the client: %q", fake, res.Body)
				}
			})
		}
	}
}

// P0.4(b): restoring a real value containing `"` and `\` keeps every chunk
// valid JSON, in content and in tool-call arguments (split mid-fake, so the
// escaped form is held across chunks too).
func TestChatStreamReversesIntoValidJSON(t *testing.T) {
	prompt := "the password is " + realSecret
	fake := fakeFor(t, prompt, realSecret)
	argsJSON := `{"password":"` + fake + `"}`
	cut := strings.Index(argsJSON, fake) + 3

	upstream := testutil.OpenAISSEDeltas(
		map[string]any{"role": "assistant", "content": "use "},
		map[string]any{"content": fake + " now"},
		map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "login", "arguments": argsJSON[:cut]},
		}}},
		map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": argsJSON[cut:]},
		}}},
	)

	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), mode.buffered, "context_pseudonymizer")
			res := gw.PostChat(t, userBody(t, prompt))
			if res.Status != http.StatusOK {
				t.Fatalf("status %d, body %q", res.Status, res.Body)
			}
			cs := readStream(t, res.Body)
			if cs.content != "use "+realSecret+" now" {
				t.Errorf("content %q", cs.content)
			}
			var args struct{ Password string }
			if err := json.Unmarshal([]byte(cs.toolArgs[0]), &args); err != nil {
				t.Fatalf("tool arguments %q aren't valid JSON: %v", cs.toolArgs[0], err)
			}
			if args.Password != realSecret || cs.toolName[0] != "login" {
				t.Errorf("tool call name %q password %q", cs.toolName[0], args.Password)
			}
		})
	}
}

// P0.3: buffered streaming sends the response pipeline's rewritten content,
// not the raw upstream bytes, and keeps the stream's metadata.
func TestChatBufferedStreamAppliesResponseRedaction(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("mail bob@exa", "mple.com please"))
	gw := streamGateway(t, fake, true, "pii_redactor")

	res := gw.PostChat(t, chatBody(true))
	if res.Status != http.StatusOK {
		t.Fatalf("status %d, body %q", res.Status, res.Body)
	}
	cs := readStream(t, res.Body)
	if cs.content != "mail [EMAIL] please" {
		t.Errorf("content %q", cs.content)
	}
	if strings.Contains(res.Body, "bob@") || strings.Contains(res.Body, "mple.com") {
		t.Errorf("unredacted email reached the client: %q", res.Body)
	}
	if len(cs.ids) != 1 || !cs.ids["chatcmpl-fake"] || len(cs.models) != 1 || !cs.models["fake-model"] {
		t.Errorf("ids %v models %v, want only chatcmpl-fake / fake-model", cs.ids, cs.models)
	}
	if cs.finish != "stop" || !cs.usage {
		t.Errorf("finish %q usage %t", cs.finish, cs.usage)
	}
}

// Passthrough without pseudonyms to reverse forwards upstream bytes as-is.
// token_counter only measures the response, so it doesn't force buffering.
func TestChatPassthroughStreamUnchangedWithoutPseudonyms(t *testing.T) {
	upstream := testutil.OpenAISSE("hel", "lo")
	var want strings.Builder
	for _, c := range upstream.Chunks {
		want.WriteString(c.Data)
	}
	gw := streamGateway(t, testutil.NewFakeUpstream(t, upstream), false, "token_counter")

	res := gw.PostChat(t, chatBody(true))
	if res.Body != want.String() {
		t.Errorf("body %q, want upstream bytes %q", res.Body, want.String())
	}
}

// P0.17: pii_redactor and content_policy force buffered mode even with
// stream_buffer: false, so model output is redacted or blocked before any of
// it reaches the client.
func TestChatResponseDLPForcesBufferedStream(t *testing.T) {
	t.Run("pii_redactor", func(t *testing.T) {
		fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("mail bob@exa", "mple.com please"))
		gw := streamGateway(t, fake, false, "pii_redactor")
		res := gw.PostChat(t, chatBody(true))
		if res.Status != http.StatusOK {
			t.Fatalf("status %d, body %q", res.Status, res.Body)
		}
		if strings.Contains(res.Body, "bob@") || strings.Contains(res.Body, "mple.com") {
			t.Fatalf("unredacted email reached the client: %q", res.Body)
		}
		if cs := readStream(t, res.Body); cs.content != "mail [EMAIL] please" {
			t.Errorf("content %q", cs.content)
		}
	})
	t.Run("content_policy", func(t *testing.T) {
		fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("this is forbid", "den text"))
		gw := testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
  stream_buffer: false
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
middleware: [content_policy]
middleware_config:
  content_policy:
    deny_patterns: ['forbidden']
`, fake.URL))
		res := gw.PostChat(t, chatBody(true))
		if strings.Contains(res.Body, "forbid") || strings.Contains(res.Body, "den text") {
			t.Errorf("model output reached the client before content_policy ran: %q", res.Body)
		}
		if !strings.Contains(res.Body, `"content_policy:`) {
			t.Errorf("no content_policy block event in %q", res.Body)
		}
	})
}
