// Package testutil provides a scriptable fake upstream and a helper that
// runs the real gateway router against an inline YAML config, for
// end-to-end tests through httptest.
package testutil

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Chunk is one piece of a streamed response body, written and flushed after
// Delay.
type Chunk struct {
	Data  string
	Delay time.Duration
}

// Response scripts one upstream reply. The zero value is an empty 200.
type Response struct {
	Status      int // 0 means 200
	Header      http.Header
	HeaderDelay time.Duration // wait before writing headers (slow upstream)

	// Body is written BodyDelay after the headers have been flushed, so the
	// client has already received the status line when it starts reading.
	Body      string
	BodyDelay time.Duration

	// Chunks, if set, replaces Body: each chunk is written and flushed
	// separately, which controls exactly where SSE frame boundaries fall.
	Chunks []Chunk

	// Disconnect aborts the connection after Body/Chunks are written, so the
	// client sees a truncated response rather than a clean end of body.
	Disconnect bool
}

// RecordedRequest is a request the fake upstream received.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// FakeUpstream is an httptest.Server that replies from a script and records
// every request it receives.
type FakeUpstream struct {
	*httptest.Server

	mu       sync.Mutex
	script   []Response
	requests []RecordedRequest
}

// NewFakeUpstream starts a fake upstream that replies with script in order.
// The last response repeats once the script is exhausted; with an empty
// script every request gets a 500. The server is closed on test cleanup.
func NewFakeUpstream(t testing.TB, script ...Response) *FakeUpstream {
	t.Helper()
	f := &FakeUpstream{script: script}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Enqueue appends responses to the script.
func (f *FakeUpstream) Enqueue(rs ...Response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, rs...)
}

// Requests returns a copy of every request received so far.
func (f *FakeUpstream) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedRequest(nil), f.requests...)
}

// RequestCount returns how many requests have been received.
func (f *FakeUpstream) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *FakeUpstream) next(r *http.Request, body []byte) (Response, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})
	if len(f.script) == 0 {
		return Response{}, false
	}
	resp := f.script[0]
	if len(f.script) > 1 {
		f.script = f.script[1:]
	}
	return resp, true
}

func (f *FakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	resp, ok := f.next(r, body)
	if !ok {
		http.Error(w, "fake upstream: no scripted response", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	if !sleep(ctx, resp.HeaderDelay) {
		return
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	flusher := w.(http.Flusher)
	w.WriteHeader(status)
	flusher.Flush()

	if resp.Chunks != nil {
		for _, c := range resp.Chunks {
			if !sleep(ctx, c.Delay) {
				return
			}
			_, _ = io.WriteString(w, c.Data)
			flusher.Flush()
		}
	} else if resp.Body != "" {
		if !sleep(ctx, resp.BodyDelay) {
			return
		}
		_, _ = io.WriteString(w, resp.Body)
		flusher.Flush()
	}

	if resp.Disconnect {
		panic(http.ErrAbortHandler)
	}
}

// sleep waits for d or until ctx is done, reporting whether it waited the
// full duration.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// JSONResponse replies with v marshalled as JSON.
func JSONResponse(status int, v any) Response {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return Response{
		Status: status,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   string(data),
	}
}

// OpenAIChat is a successful non-streaming OpenAI chat completion.
func OpenAIChat(content string) Response {
	return JSONResponse(http.StatusOK, map[string]any{
		"id":      "chatcmpl-fake",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "fake-model",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 7, "total_tokens": 12},
	})
}

// OpenAIError is an OpenAI-shaped error envelope with the given status.
func OpenAIError(status int, message, errType string) Response {
	return JSONResponse(status, map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": nil},
	})
}

// OversizedChat is a successful chat completion whose content is n bytes.
func OversizedChat(n int) Response {
	return OpenAIChat(strings.Repeat("a", n))
}

// OpenAISSE streams deltas as one OpenAI chat.completion.chunk per Chunk,
// followed by a finish chunk carrying usage and the [DONE] sentinel. Each
// SSE frame is its own Chunk, so callers can adjust Delay or split Data to
// test frame boundaries.
func OpenAISSE(deltas ...string) Response {
	maps := make([]map[string]any, len(deltas))
	for i, d := range deltas {
		maps[i] = map[string]any{"content": d}
	}
	return OpenAISSEDeltas(maps...)
}

// OpenAISSEDeltas is OpenAISSE with arbitrary delta objects, e.g.
// {"tool_calls": [...]}.
func OpenAISSEDeltas(deltas ...map[string]any) Response {
	frame := func(v any) string {
		data, _ := json.Marshal(v)
		return "data: " + string(data) + "\n\n"
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id":      "chatcmpl-fake",
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   "fake-model",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	var chunks []Chunk
	for _, d := range deltas {
		chunks = append(chunks, Chunk{Data: frame(chunk(d, nil))})
	}
	final := chunk(map[string]any{}, "stop")
	final["usage"] = map[string]any{"prompt_tokens": 5, "completion_tokens": len(deltas), "total_tokens": 5 + len(deltas)}
	chunks = append(chunks, Chunk{Data: frame(final)}, Chunk{Data: "data: [DONE]\n\n"})
	return Response{
		Header: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks: chunks,
	}
}

// AnthropicMessage is a successful non-streaming Anthropic Messages API
// response.
func AnthropicMessage(content string) Response {
	return JSONResponse(http.StatusOK, map[string]any{
		"id":            "msg_fake",
		"type":          "message",
		"role":          "assistant",
		"model":         "fake-model",
		"content":       []any{map[string]any{"type": "text", "text": content}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 5, "output_tokens": 7},
	})
}

// AnthropicSSE streams deltas as Anthropic Messages API events, one Chunk
// per event.
func AnthropicSSE(deltas ...string) Response {
	event := func(name string, v any) Chunk {
		data, _ := json.Marshal(v)
		return Chunk{Data: "event: " + name + "\ndata: " + string(data) + "\n\n"}
	}
	chunks := []Chunk{
		event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_fake", "type": "message", "role": "assistant", "model": "fake-model",
			"content": []any{}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 0},
		}}),
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}}),
	}
	for _, d := range deltas {
		chunks = append(chunks, event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": d}}))
	}
	chunks = append(chunks,
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		event("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": len(deltas)}}),
		event("message_stop", map[string]any{"type": "message_stop"}),
	)
	return Response{
		Header: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks: chunks,
	}
}
