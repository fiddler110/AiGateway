package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

// limitGateway is a gateway with one extra settings block (indented YAML
// lines), for P0.9's response size and duration bounds.
func limitGateway(t *testing.T, fake *testutil.FakeUpstream, buffered bool, settings string) *testutil.Gateway {
	t.Helper()
	return testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
  stream_buffer: %t
%s
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
`, fake.URL, buffered, settings))
}

// assertStreamEndsWithError checks that body is a well-formed SSE stream
// (every data payload valid JSON, so no truncated line was forwarded) whose
// last event before [DONE] is an error envelope with code.
func assertStreamEndsWithError(t *testing.T, body string, code int) {
	t.Helper()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream doesn't end with [DONE]: %.300q", body)
	}
	var last string
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("invalid JSON forwarded in stream: %.300q", payload)
		}
		last = payload
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(last), &env); err != nil || env.Error.Code != code {
		t.Fatalf("last event %.300q, want an error envelope with code %d", last, code)
	}
	if !strings.Contains(env.Error.Message, "request_id") {
		t.Errorf("error message %q lacks a request_id", env.Error.Message)
	}
}

// deltaFrames returns the upstream frames of resp without the final
// usage/[DONE] frames, i.e. only its delta chunks.
func deltaFrames(resp testutil.Response) []testutil.Chunk {
	return append([]testutil.Chunk(nil), resp.Chunks[:len(resp.Chunks)-2]...)
}

// P0.9: a non-streaming upstream body over max_response_bytes is a generic
// 502, not a truncated or unbounded read, and isn't retried on a fallback.
func TestChatNonStreamResponseOverLimit(t *testing.T) {
	t.Run("over limit", func(t *testing.T) {
		primary := testutil.NewFakeUpstream(t, testutil.OversizedChat(4096))
		fallback := testutil.NewFakeUpstream(t, testutil.OpenAIChat("from fallback"))
		gw := testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  primary:
    base_url: %q
    auth_type: none
  fallback:
    base_url: %q
    auth_type: none
model_routes:
  - pattern: "*"
    upstreams: [primary, fallback]
settings:
  max_response_bytes: 1024
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
`, primary.URL, fallback.URL))

		res := gw.PostChat(t, chatBody(false))
		if res.Status != http.StatusBadGateway {
			t.Fatalf("status %d, want 502; body %.300q", res.Status, res.Body)
		}
		env := decodeError(t, res.Body)
		if !strings.Contains(env.Error.Message, "too large") || !strings.Contains(env.Error.Message, "request_id") {
			t.Errorf("message %q", env.Error.Message)
		}
		if strings.Contains(res.Body, "aaaa") {
			t.Errorf("upstream content reached the client: %.300q", res.Body)
		}
		if fallback.RequestCount() != 0 {
			t.Errorf("fallback received %d requests, want 0", fallback.RequestCount())
		}
	})
	t.Run("under limit", func(t *testing.T) {
		gw := limitGateway(t, testutil.NewFakeUpstream(t, testutil.OversizedChat(100)), false, "  max_response_bytes: 1024")
		if res := gw.PostChat(t, chatBody(false)); res.Status != http.StatusOK {
			t.Fatalf("status %d, body %.300q", res.Status, res.Body)
		}
	})
}

// P0.9: a stream over max_response_bytes ends in an error event. Passthrough
// keeps what it already forwarded; buffered sends nothing but the error.
func TestChatStreamResponseOverLimit(t *testing.T) {
	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("hello ", strings.Repeat("a", 4096)))
			gw := limitGateway(t, fake, mode.buffered, "  max_response_bytes: 1024")

			res := gw.PostChat(t, chatBody(true))
			assertStreamEndsWithError(t, res.Body, http.StatusBadGateway)
			if strings.Contains(res.Body, "aaaa") {
				t.Errorf("over-limit content reached the client: %.300q", res.Body)
			}
			if mode.buffered && strings.Contains(res.Body, "hello") {
				t.Errorf("buffered mode sent partial content: %.300q", res.Body)
			}
			if !mode.buffered && !strings.Contains(res.Body, "hello ") {
				t.Errorf("passthrough lost content forwarded before the limit: %.300q", res.Body)
			}
		})
	}
}

// P0.9: one SSE line over the 4 MiB scanner cap is an error event, not a
// silent end of stream (passthrough) or a silently truncated re-emit
// (buffered).
func TestChatStreamOversizedLine(t *testing.T) {
	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("hello ", strings.Repeat("a", 5<<20)))
			gw := limitGateway(t, fake, mode.buffered, "")

			res := gw.PostChat(t, chatBody(true))
			assertStreamEndsWithError(t, res.Body, http.StatusBadGateway)
			if mode.buffered && strings.Contains(res.Body, "hello") {
				t.Errorf("buffered mode sent partial content: %.300q", res.Body)
			}
		})
	}
}

// P0.9: a stream that never finishes is cut off at stream_timeout, the
// upstream read is cancelled, and the client gets a 504 error event.
func TestChatStreamTimeout(t *testing.T) {
	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			frames := deltaFrames(testutil.OpenAISSE("hello ", "never sent"))
			frames[1].Delay = time.Minute // the upstream hangs mid-stream
			fake := testutil.NewFakeUpstream(t, testutil.Response{
				Header: http.Header{"Content-Type": {"text/event-stream"}},
				Chunks: frames,
			})
			gw := limitGateway(t, fake, mode.buffered, "  stream_timeout: 0.2")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(chatBody(true)))
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := gw.Client().Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("stream did not end within the client's 5s bound (after %v): %v; got %.300q", elapsed, err, data)
			}
			if elapsed > 3*time.Second {
				t.Errorf("stream took %v to end with a 0.2s stream_timeout", elapsed)
			}
			assertStreamEndsWithError(t, string(data), http.StatusGatewayTimeout)
			if strings.Contains(string(data), "never sent") {
				t.Errorf("content after the hang reached the client")
			}
		})
	}
}

// P0.9: an upstream that drops mid-stream ends the client's stream with an
// error event instead of a silent, apparently complete end.
func TestChatStreamUpstreamDrop(t *testing.T) {
	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			fake := testutil.NewFakeUpstream(t, testutil.Response{
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Chunks:     deltaFrames(testutil.OpenAISSE("hel", "lo")),
				Disconnect: true,
			})
			gw := limitGateway(t, fake, mode.buffered, "")

			res := gw.PostChat(t, chatBody(true))
			assertStreamEndsWithError(t, res.Body, http.StatusServiceUnavailable)
			if mode.buffered && strings.Contains(res.Body, "hel") {
				t.Errorf("buffered mode sent partial content: %.300q", res.Body)
			}
			if !mode.buffered && !strings.Contains(res.Body, `"hel"`) {
				t.Errorf("passthrough lost content forwarded before the drop: %.300q", res.Body)
			}
		})
	}
}

// P0.9: an upstream that drops mid-line never has the truncated line
// forwarded as if complete.
func TestChatPassthroughStreamDropMidLine(t *testing.T) {
	frames := deltaFrames(testutil.OpenAISSE("hel", "lo"))
	cut := len(frames[1].Data) / 2
	frames[1].Data = frames[1].Data[:cut]
	fake := testutil.NewFakeUpstream(t, testutil.Response{
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Chunks:     frames,
		Disconnect: true,
	})
	gw := limitGateway(t, fake, false, "")

	res := gw.PostChat(t, chatBody(true))
	assertStreamEndsWithError(t, res.Body, http.StatusServiceUnavailable)
}

// P0.14 (decision 2026-09-13): when stream_timeout expires before the
// upstream sends headers, nothing has reached the client, so it gets a real
// 504 JSON error rather than a 200 stream carrying an error event.
func TestChatStreamTimeoutBeforeHeaders(t *testing.T) {
	for _, mode := range streamModes {
		t.Run(mode.name, func(t *testing.T) {
			resp := testutil.OpenAISSE("never sent")
			resp.HeaderDelay = time.Minute // the upstream never answers
			fake := testutil.NewFakeUpstream(t, resp)
			gw := limitGateway(t, fake, mode.buffered, "  stream_timeout: 0.2")

			res := gw.PostChat(t, chatBody(true))
			if res.Status != http.StatusGatewayTimeout {
				t.Fatalf("status %d, want 504; body %.300q", res.Status, res.Body)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q, want application/json", ct)
			}
			env := decodeError(t, res.Body)
			if env.Error.Code != http.StatusGatewayTimeout || !strings.Contains(env.Error.Message, "request_id") {
				t.Errorf("error = %+v, want code 504 with a request_id", env.Error)
			}
		})
	}
}
