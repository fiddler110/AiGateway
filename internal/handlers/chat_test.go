package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

func gatewayFor(t *testing.T, fake *testutil.FakeUpstream) *testutil.Gateway {
	t.Helper()
	return testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
`, fake.URL))
}

func chatBody(stream bool) string {
	return fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

func decodeError(t *testing.T, body string) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("response is not a JSON error envelope: %v; body %q", err, body)
	}
	return env
}

// P0.1 end to end: a non-streaming response whose body arrives after its
// headers reaches the client intact.
func TestChatNonStreamingBodyAfterHeaders(t *testing.T) {
	resp := testutil.OpenAIChat("late body")
	resp.BodyDelay = 50 * time.Millisecond
	gw := gatewayFor(t, testutil.NewFakeUpstream(t, resp))

	res := gw.PostChat(t, chatBody(false))
	if res.Status != http.StatusOK {
		t.Fatalf("status %d, body %q", res.Status, res.Body)
	}
	if !strings.Contains(res.Body, "late body") {
		t.Errorf("body %q missing upstream content", res.Body)
	}
}

// P0.2 end to end: upstream 4xx statuses reach the client instead of 200,
// for both streaming and non-streaming requests.
func TestChatRelaysUpstreamClientErrors(t *testing.T) {
	cases := []struct {
		name       string
		upstream   testutil.Response
		wantStatus int
		wantMsg    string
		wantRetry  string
		notInBody  string
	}{
		{
			name:       "400 keeps upstream message",
			upstream:   testutil.OpenAIError(http.StatusBadRequest, "max_tokens is too large", "invalid_request_error"),
			wantStatus: http.StatusBadRequest,
			wantMsg:    "max_tokens is too large",
		},
		{
			name:       "404 with ollama error shape",
			upstream:   testutil.JSONResponse(http.StatusNotFound, map[string]any{"error": "model 'm' not found"}),
			wantStatus: http.StatusNotFound,
			wantMsg:    "model 'm' not found",
		},
		{
			name: "429 keeps Retry-After",
			upstream: func() testutil.Response {
				r := testutil.OpenAIError(http.StatusTooManyRequests, "rate limited", "rate_limit_error")
				r.Header.Set("Retry-After", "12")
				return r
			}(),
			wantStatus: http.StatusTooManyRequests,
			wantMsg:    "rate limited",
			wantRetry:  "12",
		},
		{
			name:       "non-JSON body is not reflected",
			upstream:   testutil.Response{Status: http.StatusBadRequest, Body: "<html>proxy says no</html>"},
			wantStatus: http.StatusBadRequest,
			wantMsg:    "upstream error: Bad Request",
			notInBody:  "proxy says no",
		},
		{
			name:       "401 is the gateway's credential, becomes generic 502",
			upstream:   testutil.OpenAIError(http.StatusUnauthorized, "Incorrect API key provided: sk-abc***xyz", "invalid_request_error"),
			wantStatus: http.StatusBadGateway,
			wantMsg:    "upstream rejected the gateway's credentials",
			notInBody:  "sk-abc",
		},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				fake := testutil.NewFakeUpstream(t, tc.upstream)
				gw := gatewayFor(t, fake)

				res := gw.PostChat(t, chatBody(stream))
				if res.Status != tc.wantStatus {
					t.Fatalf("status %d, want %d; body %q", res.Status, tc.wantStatus, res.Body)
				}
				if ct := res.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type %q, want application/json", ct)
				}
				env := decodeError(t, res.Body)
				if env.Error.Message != tc.wantMsg || env.Error.Code != tc.wantStatus {
					t.Errorf("error = %+v, want message %q code %d", env.Error, tc.wantMsg, tc.wantStatus)
				}
				if got := res.Header.Get("Retry-After"); got != tc.wantRetry {
					t.Errorf("Retry-After %q, want %q", got, tc.wantRetry)
				}
				if tc.notInBody != "" && strings.Contains(res.Body, tc.notInBody) {
					t.Errorf("body %q contains %q", res.Body, tc.notInBody)
				}
				if n := fake.RequestCount(); n != 1 {
					t.Errorf("upstream saw %d requests, want 1", n)
				}
			})
		}
	}
}

func TestChatStreamingSuccessStillStreams(t *testing.T) {
	gw := gatewayFor(t, testutil.NewFakeUpstream(t, testutil.OpenAISSE("hel", "lo")))

	res := gw.PostChat(t, chatBody(true))
	if res.Status != http.StatusOK {
		t.Fatalf("status %d, body %q", res.Status, res.Body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type %q, want text/event-stream", ct)
	}
	if !strings.Contains(res.Body, `"hel"`) || !strings.HasSuffix(res.Body, "data: [DONE]\n\n") {
		t.Errorf("unexpected stream body %q", res.Body)
	}
}
