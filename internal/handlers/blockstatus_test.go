package handlers_test

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

// P0.14: blocks used to be 400 whatever blocked them. Rate-limit and budget
// blocks are 429 with Retry-After; content and DLP blocks stay 400 without
// it. Request-phase blocks happen before any response byte, so a streaming
// request gets the same real status and header.
func TestChatBlockStatus(t *testing.T) {
	cases := []struct {
		name       string
		middleware string
		config     string
		warmUp     bool // one allowed request first, to exhaust the limit
		prompt     string
		wantStatus int
		wantType   string
		maxRetry   int // upper bound on Retry-After seconds; 0 means no header
	}{
		{
			name: "rate_limiter", middleware: "rate_limiter",
			config: "  rate_limiter:\n    requests_per_minute: 1\n",
			warmUp: true, prompt: "hi",
			wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", maxRetry: 60,
		},
		{
			name: "token_rate_limiter", middleware: "token_rate_limiter",
			config: "  token_rate_limiter:\n    tokens_per_minute: 1\n",
			warmUp: true, prompt: "hi",
			wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", maxRetry: 60,
		},
		{
			// The warm-up's 7 reported completion tokens cost $7 against a
			// $0.001 budget; spend resets at the next UTC month.
			name: "cost_tracker budget", middleware: "cost_tracker",
			config: "  cost_tracker:\n    pricing:\n      m:\n        completion_per_1k: 1000\n    budgets:\n      default: 0.001\n",
			warmUp: true, prompt: "hi",
			wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error", maxRetry: 32 * 24 * 3600,
		},
		{
			name: "content_policy", middleware: "content_policy",
			config:     "  content_policy:\n    deny_patterns: ['forbidden']\n",
			prompt:     "something forbidden",
			wantStatus: http.StatusBadRequest, wantType: "invalid_request_error",
		},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				fake := testutil.NewFakeUpstream(t, testutil.OpenAIChat("ok"))
				gw := testutil.NewGateway(t, fmt.Sprintf(`
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
middleware: [%s]
middleware_config:
%s`, fake.URL, tc.middleware, tc.config))

				if tc.warmUp {
					if res := gw.PostChat(t, chatBody(false)); res.Status != http.StatusOK {
						t.Fatalf("warm-up status %d, body %q", res.Status, res.Body)
					}
				}
				body := fmt.Sprintf(`{"model":"m","stream":%t,"messages":[{"role":"user","content":%q}]}`, stream, tc.prompt)
				res := gw.PostChat(t, body)
				if res.Status != tc.wantStatus {
					t.Fatalf("status %d, want %d; body %q", res.Status, tc.wantStatus, res.Body)
				}
				env := decodeError(t, res.Body)
				if env.Error.Code != tc.wantStatus || env.Error.Type != tc.wantType || !strings.HasPrefix(env.Error.Message, tc.middleware+":") {
					t.Errorf("error = %+v, want code %d type %s from %s", env.Error, tc.wantStatus, tc.wantType, tc.middleware)
				}
				retry := res.Header.Get("Retry-After")
				if tc.maxRetry == 0 {
					if retry != "" {
						t.Errorf("Retry-After %q on a %d block", retry, tc.wantStatus)
					}
				} else if secs, err := strconv.Atoi(retry); err != nil || secs < 1 || secs > tc.maxRetry {
					t.Errorf("Retry-After %q, want whole seconds in [1, %d]", retry, tc.maxRetry)
				}
				wantRequests := 0
				if tc.warmUp {
					wantRequests = 1
				}
				if n := fake.RequestCount(); n != wantRequests {
					t.Errorf("upstream saw %d requests, want %d", n, wantRequests)
				}
			})
		}
	}
}
