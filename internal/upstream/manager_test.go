package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/testutil"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

func newTestManager(t *testing.T, baseURL string, retries int) *upstream.Manager {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstreams = map[string]config.UpstreamConfig{"up": {BaseURL: baseURL, AuthType: "none"}}
	cfg.Resilience.RetryAttempts = retries
	cfg.Resilience.HealthCheck.Enabled = false
	registry := provider.Registry{"openai": func() provider.Translator { return openai.New() }}
	return upstream.NewManager(&cfg, &http.Client{}, registry)
}

func chatRequest() *chatmodel.ChatRequest {
	return &chatmodel.ChatRequest{Model: "m", Messages: []chatmodel.ChatMessage{{Role: "user"}}}
}

func noKey(string) string { return "" }

func failures(m *upstream.Manager) int { return m.Status()["up"].Failures }

// P0.1: the body used to be read after forward's timeout context was
// cancelled, so a body arriving after the headers failed with "context
// canceled" and counted against the circuit breaker.
func TestSendReadsBodyThatArrivesAfterHeaders(t *testing.T) {
	resp := testutil.OpenAIChat("late body")
	resp.BodyDelay = 50 * time.Millisecond
	fake := testutil.NewFakeUpstream(t, resp)
	m := newTestManager(t, fake.URL, 0)

	res, err := m.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != http.StatusOK || !strings.Contains(string(res.Body), "late body") {
		t.Errorf("got status %d body %q", res.Status, res.Body)
	}
	if !res.Usage.Actual || res.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v, want provider-reported total 12", res.Usage)
	}
	if n := failures(m); n != 0 {
		t.Errorf("circuit failures = %d, want 0", n)
	}
}

// P0.2: 4xx responses are returned with their status, not retried, and not
// counted as upstream failures.
func TestSendReturnsClientErrorStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := testutil.OpenAIError(status, "nope", "invalid_request_error")
			up.Header.Set("Retry-After", "7")
			fake := testutil.NewFakeUpstream(t, up)
			m := newTestManager(t, fake.URL, 2)

			res, err := m.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if res.Status != status {
				t.Errorf("status = %d, want %d", res.Status, status)
			}
			if !strings.Contains(string(res.Body), "nope") {
				t.Errorf("body = %q, want the upstream error body", res.Body)
			}
			if got := res.Header.Get("Retry-After"); got != "7" {
				t.Errorf("Retry-After = %q, want 7", got)
			}
			if n := fake.RequestCount(); n != 1 {
				t.Errorf("upstream saw %d requests, want 1 (4xx must not be retried)", n)
			}
			if n := failures(m); n != 0 {
				t.Errorf("circuit failures = %d, want 0", n)
			}
		})
	}
}

func TestSendServerErrorIsFailure(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAIError(http.StatusBadGateway, "down", "api_error"))
	m := newTestManager(t, fake.URL, 0)

	_, err := m.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
	if !errors.Is(err, upstream.ErrAllUpstreamsUnavailable) {
		t.Fatalf("err = %v, want ErrAllUpstreamsUnavailable", err)
	}
	if n := failures(m); n != 1 {
		t.Errorf("circuit failures = %d, want 1", n)
	}
}

func TestSendStreamReturnsClientErrorStatus(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAIError(http.StatusNotFound, "no such model", "invalid_request_error"))
	m := newTestManager(t, fake.URL, 0)

	res, err := m.SendStream(context.Background(), []string{"up"}, chatRequest(), noKey)
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if res.Body != nil {
		res.Body.Close()
		t.Fatal("Body set for a 4xx, want only ErrorBody")
	}
	if res.Status != http.StatusNotFound || !strings.Contains(string(res.ErrorBody), "no such model") {
		t.Errorf("status %d error body %q", res.Status, res.ErrorBody)
	}
	if n := failures(m); n != 0 {
		t.Errorf("circuit failures = %d, want 0", n)
	}
}

func TestSendStreamServerErrorIsFailure(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAIError(http.StatusServiceUnavailable, "internal detail", "api_error"))
	m := newTestManager(t, fake.URL, 0)

	_, err := m.SendStream(context.Background(), []string{"up"}, chatRequest(), noKey)
	if !errors.Is(err, upstream.ErrAllUpstreamsUnavailable) {
		t.Fatalf("err = %v, want ErrAllUpstreamsUnavailable", err)
	}
	if strings.Contains(err.Error(), "internal detail") {
		t.Errorf("error %q embeds the upstream body", err)
	}
	if n := failures(m); n != 1 {
		t.Errorf("circuit failures = %d, want 1", n)
	}
}
