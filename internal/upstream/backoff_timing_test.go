package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/testutil"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

// newTestManagerWithDelay builds a Manager whose upstreams are named in
// order: "up" for the first URL, then "up2", "up3", ...
func newTestManagerWithDelay(t *testing.T, baseURL string, retries int, delaySeconds float64, moreURLs ...string) *upstream.Manager {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstreams = map[string]config.UpstreamConfig{"up": {BaseURL: baseURL, AuthType: "none"}}
	for i, u := range moreURLs {
		cfg.Upstreams["up"+string(rune('2'+i))] = config.UpstreamConfig{BaseURL: u, AuthType: "none"}
	}
	cfg.Resilience.RetryAttempts = retries
	cfg.Resilience.RetryDelaySeconds = delaySeconds
	cfg.Resilience.HealthCheck.Enabled = false
	registry := provider.Registry{"openai": func() provider.Translator { return openai.New() }}
	return upstream.NewManager(&cfg, &http.Client{}, registry)
}

// P0.6 (black-box, real clock): retry_delay_seconds used to be ignored in
// favour of a hardcoded 1s, 2s, ... backoff that also ran after the final
// attempt, so one retry against a failing upstream took about 3s.
func TestSendRetryUsesConfiguredDelay(t *testing.T) {
	fake := testutil.NewFakeUpstream(t) // empty script: every request is a 500
	m := newTestManagerWithDelay(t, fake.URL, 1, 0.01)

	start := time.Now()
	_, err := m.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send succeeded against a failing upstream")
	}
	if n := fake.RequestCount(); n != 2 {
		t.Errorf("upstream saw %d requests, want 2", n)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Send took %v with retry_delay_seconds 0.01, want well under 1s", elapsed)
	}
}

// P0.6 (black-box, real clock): cancelling the request during backoff must
// end Send with the context error, without more attempts that would count
// the client's disconnect against the upstream's circuit.
func TestSendBackoffAbortsOnCancel(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	m := newTestManagerWithDelay(t, fake.URL, 3, 30)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := m.Send(ctx, []string{"up"}, chatRequest(), noKey)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send took %v after cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := fake.RequestCount(); n != 1 {
		t.Errorf("upstream saw %d requests, want 1 (no attempts after cancel)", n)
	}
	if n := failures(m); n != 1 {
		t.Errorf("circuit failures = %d, want 1", n)
	}
}
