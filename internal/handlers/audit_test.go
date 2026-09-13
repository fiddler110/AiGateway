package handlers_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

type auditLine struct {
	RequestID        string `json:"request_id"`
	ClientID         string `json:"client_id"`
	Model            string `json:"model"`
	MessageCount     *int   `json:"message_count"`
	Stream           *bool  `json:"stream"`
	Upstream         string `json:"upstream"`
	Status           int    `json:"status"`
	LatencyMS        *int64 `json:"latency_ms"`
	PromptTokens     *int   `json:"prompt_tokens"`
	CompletionTokens *int   `json:"completion_tokens"`
	Blocked          bool   `json:"blocked"`
	BlockMiddleware  string `json:"block_middleware"`
	BlockDirection   string `json:"block_direction"`
}

// P0.14: audit_log used to write in the request phase, so it logged the
// first route candidate instead of the upstream that served the request,
// and never recorded status, latency, tokens, or block outcome. It now
// writes after the response, including for blocked requests and upstream
// failures, and never writes prompt or response text.
func TestChatAuditRecordsOutcome(t *testing.T) {
	const prompt = "PROMPT-TEXT-NEVER-LOGGED"
	const answer = "ANSWER-TEXT-NEVER-LOGGED"
	down := testutil.NewFakeUpstream(t)
	down.Close() // connection refused
	up := testutil.NewFakeUpstream(t, testutil.OpenAIChat(answer), testutil.OpenAISSE(answer))
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")

	gw := testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  down:
    base_url: %q
    auth_type: none
  up:
    base_url: %q
    auth_type: none
model_routes:
  - pattern: "m"
    upstreams: [down, up]
  - pattern: "dead"
    upstreams: [down]
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
middleware: [audit_log, rate_limiter]
middleware_config:
  audit_log:
    log_file: %q
  rate_limiter:
    requests_per_minute: 4
`, down.URL, up.URL, logPath))

	post := func(model string, stream bool) testutil.Result {
		body := fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, model, stream, prompt)
		return gw.PostChat(t, body)
	}
	results := []testutil.Result{
		post("m", false),    // served by the fallback upstream
		post("m", true),     // streamed by the fallback upstream
		post("dead", false), // no upstream answers: 503
		post("dead", true),  // stream fails before any byte: a real 503 too
		post("m", false),    // fifth request in the minute: rate limited
	}
	wantHTTP := []int{200, 200, 503, 503, 429}
	for i, res := range results {
		if res.Status != wantHTTP[i] {
			t.Fatalf("request %d: status %d, want %d; body %q", i, res.Status, wantHTTP[i], res.Body)
		}
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{prompt, answer} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("audit log contains %q", leak)
		}
	}
	var lines []auditLine
	for sc := bufio.NewScanner(strings.NewReader(string(raw))); sc.Scan(); {
		var l auditLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("invalid audit line %q: %v", sc.Text(), err)
		}
		lines = append(lines, l)
	}
	if len(lines) != len(results) {
		t.Fatalf("got %d audit lines, want %d:\n%s", len(lines), len(results), raw)
	}

	want := []struct {
		upstream string
		status   int
		stream   bool
		tokens   bool
		blocked  string
	}{
		{upstream: "up", status: 200, tokens: true},
		{upstream: "up", status: 200, stream: true},
		{status: 503},
		{status: 503, stream: true},
		{status: 429, blocked: "rate_limiter"},
	}
	for i, l := range lines {
		w := want[i]
		if l.RequestID != results[i].Header.Get("x-request-id") || l.RequestID == "" {
			t.Errorf("line %d: request_id %q, want response header %q", i, l.RequestID, results[i].Header.Get("x-request-id"))
		}
		if l.Upstream != w.upstream || l.Status != w.status {
			t.Errorf("line %d: upstream %q status %d, want %q %d", i, l.Upstream, l.Status, w.upstream, w.status)
		}
		if l.LatencyMS == nil || *l.LatencyMS < 0 || l.ClientID != "anonymous" {
			t.Errorf("line %d: latency_ms %v client_id %q", i, l.LatencyMS, l.ClientID)
		}
		if l.MessageCount == nil || *l.MessageCount != 1 || l.Stream == nil || *l.Stream != w.stream {
			t.Errorf("line %d: message_count %v stream %v, want 1 and %t", i, l.MessageCount, l.Stream, w.stream)
		}
		if w.tokens && (l.PromptTokens == nil || *l.PromptTokens != 5 || l.CompletionTokens == nil || *l.CompletionTokens != 7) {
			t.Errorf("line %d: tokens %v/%v, want provider-reported 5/7", i, l.PromptTokens, l.CompletionTokens)
		}
		wantBlocked := w.blocked != ""
		if l.Blocked != wantBlocked || l.BlockMiddleware != w.blocked {
			t.Errorf("line %d: blocked %t by %q, want %t by %q", i, l.Blocked, l.BlockMiddleware, wantBlocked, w.blocked)
		}
		if wantBlocked && l.BlockDirection != "request" {
			t.Errorf("line %d: block_direction %q, want request", i, l.BlockDirection)
		}
	}
	if n := up.RequestCount(); n != 2 {
		t.Errorf("serving upstream saw %d requests, want 2", n)
	}
}
