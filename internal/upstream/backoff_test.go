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

// recordingSleeper records requested delays and returns immediately.
type recordingSleeper struct {
	delays []time.Duration
	onCall func(call int)
}

func (s *recordingSleeper) sleep(ctx context.Context, d time.Duration) error {
	s.delays = append(s.delays, d)
	if s.onCall != nil {
		s.onCall(len(s.delays))
	}
	return nil
}

// P0.6: delays are linear in retry_delay_seconds, jittered within ±20%, and
// there is no sleep after the final attempt.
func TestSendBackoffDelays(t *testing.T) {
	const delay = 0.5 // seconds
	cases := []struct {
		name   string
		rand   float64
		factor float64 // expected jitter multiplier
	}{
		{"no jitter", 0.5, 1.0},
		{"minimum jitter", 0, 0.8},
		{"maximum jitter", 0.999999, 1.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := testutil.NewFakeUpstream(t)
			m := newTestManagerWithDelay(t, fake.URL, 3, delay)
			s := &recordingSleeper{}
			upstream.SetBackoffHooks(m, s.sleep, func() float64 { return tc.rand })

			if _, err := m.Send(context.Background(), []string{"up"}, chatRequest(), noKey); err == nil {
				t.Fatal("Send succeeded against a failing upstream")
			}
			if n := fake.RequestCount(); n != 4 {
				t.Errorf("upstream saw %d requests, want 4", n)
			}
			if len(s.delays) != 3 {
				t.Fatalf("slept %d times (%v), want 3: once between each of 4 attempts, none after the last", len(s.delays), s.delays)
			}
			for i, got := range s.delays {
				base := delay * float64(i+1) * float64(time.Second)
				want := time.Duration(base * tc.factor)
				if diff := got - want; diff < -time.Millisecond || diff > time.Millisecond {
					t.Errorf("delay %d = %v, want %v", i, got, want)
				}
				if float64(got) < 0.8*base || float64(got) > 1.2*base {
					t.Errorf("delay %d = %v outside ±20%% of %v", i, got, time.Duration(base))
				}
			}
		})
	}
}

// P0.6: moving to the next fallback upstream, or giving up, never waits.
func TestSendNoSleepAfterFinalAttemptBeforeFallback(t *testing.T) {
	first := testutil.NewFakeUpstream(t)
	second := testutil.NewFakeUpstream(t)
	m := newTestManagerWithDelay(t, first.URL, 1, 1, second.URL)
	s := &recordingSleeper{}
	var countsAtSleep [][2]int
	s.onCall = func(int) {
		countsAtSleep = append(countsAtSleep, [2]int{first.RequestCount(), second.RequestCount()})
	}
	upstream.SetBackoffHooks(m, s.sleep, func() float64 { return 0.5 })

	if _, err := m.Send(context.Background(), []string{"up", "up2"}, chatRequest(), noKey); !errors.Is(err, upstream.ErrAllUpstreamsUnavailable) {
		t.Fatalf("err = %v, want ErrAllUpstreamsUnavailable", err)
	}
	// One sleep per upstream, each between its two attempts.
	want := [][2]int{{1, 0}, {2, 1}}
	if len(countsAtSleep) != len(want) {
		t.Fatalf("sleeps happened at request counts %v, want %v", countsAtSleep, want)
	}
	for i := range want {
		if countsAtSleep[i] != want[i] {
			t.Errorf("sleeps happened at request counts %v, want %v", countsAtSleep, want)
			break
		}
	}
}

// P0.6: a sleeper error (context done) ends Send at once: no further
// attempts on this upstream and no fallback.
func TestSendBackoffSleepErrorStops(t *testing.T) {
	first := testutil.NewFakeUpstream(t)
	second := testutil.NewFakeUpstream(t, testutil.OpenAIChat("should not be reached"))
	m := newTestManagerWithDelay(t, first.URL, 2, 1, second.URL)
	upstream.SetBackoffHooks(m, func(ctx context.Context, d time.Duration) error {
		return context.DeadlineExceeded
	}, nil)

	_, err := m.Send(context.Background(), []string{"up", "up2"}, chatRequest(), noKey)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if a, b := first.RequestCount(), second.RequestCount(); a != 1 || b != 0 {
		t.Errorf("request counts = %d, %d, want 1, 0", a, b)
	}
}

// SendStream does not retry within an upstream, so it must never back off,
// even with retries configured.
func TestSendStreamNeverBacksOff(t *testing.T) {
	first := testutil.NewFakeUpstream(t)
	second := testutil.NewFakeUpstream(t)
	m := newTestManagerWithDelay(t, first.URL, 2, 1, second.URL)
	upstream.SetBackoffHooks(m, func(ctx context.Context, d time.Duration) error {
		t.Errorf("SendStream slept %v", d)
		return nil
	}, nil)

	if _, err := m.SendStream(context.Background(), []string{"up", "up2"}, chatRequest(), noKey); !errors.Is(err, upstream.ErrAllUpstreamsUnavailable) {
		t.Fatalf("err = %v, want ErrAllUpstreamsUnavailable", err)
	}
	if a, b := first.RequestCount(), second.RequestCount(); a != 1 || b != 1 {
		t.Errorf("request counts = %d, %d, want 1, 1", a, b)
	}
}

// Hot reload keeps the Manager's backoff hooks.
func TestWithMergedConfigKeepsBackoffHooks(t *testing.T) {
	fake := testutil.NewFakeUpstream(t)
	m := newTestManagerWithDelay(t, fake.URL, 1, 1)
	s := &recordingSleeper{}
	upstream.SetBackoffHooks(m, s.sleep, func() float64 { return 0.5 })

	cfg := config.Defaults()
	cfg.Upstreams = map[string]config.UpstreamConfig{"up": {BaseURL: fake.URL, AuthType: "none"}}
	cfg.Resilience.RetryAttempts = 1
	cfg.Resilience.RetryDelaySeconds = 2
	cfg.Resilience.HealthCheck.Enabled = false
	next := m.WithMergedConfig(&cfg, &http.Client{}, provider.Registry{"openai": func() provider.Translator { return openai.New() }})

	_, _ = next.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
	if len(s.delays) != 1 || s.delays[0] != 2*time.Second {
		t.Errorf("delays = %v, want [2s] from the reloaded config", s.delays)
	}
}
