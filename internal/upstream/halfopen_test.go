package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
)

// P0.14: while a half-open probe fails, only that probe reaches the upstream.
// The failure must be recorded (re-opening the circuit) BEFORE the probe
// slot is released; released first, there is a window in which the circuit
// is still half-open with a free slot and a second probe gets through.
//
// Rather than hoping a race lands in that window (which needs -race or luck),
// the circuit's onRelease hook fires a burst of requests at exactly the
// moment the slot frees, so the old order fails deterministically.
func TestHalfOpenFailingProbeAdmitsOneRequest(t *testing.T) {
	type sendFunc func(m *Manager) error
	sends := map[string]sendFunc{
		"Send": func(m *Manager) error {
			_, err := m.Send(context.Background(), []string{"up"}, probeRequest(), func(string) string { return "" })
			return err
		},
		"SendStream": func(m *Manager) error {
			res, err := m.SendStream(context.Background(), []string{"up"}, probeRequest(), func(string) string { return "" })
			if res.Body != nil {
				res.Body.Close()
			}
			return err
		},
	}
	const burst = 8

	for name, send := range sends {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			probeArrived := make(chan struct{})
			finishProbe := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					close(probeArrived)
					<-finishProbe
				}
				http.Error(w, "down", http.StatusInternalServerError)
			}))
			defer srv.Close()

			cfg := config.Defaults()
			cfg.Upstreams = map[string]config.UpstreamConfig{"up": {BaseURL: srv.URL, AuthType: "none"}}
			cfg.Resilience.RetryAttempts = 0
			cfg.Resilience.HealthCheck.Enabled = false
			cfg.Resilience.CircuitBreaker.FailureThreshold = 1
			cfg.Resilience.CircuitBreaker.CooldownSeconds = 60
			registry := provider.Registry{"openai": func() provider.Translator { return openai.New() }}
			m := NewManager(&cfg, srv.Client(), registry)

			// Tripped long enough ago that the cooldown has elapsed: half-open.
			c := m.entries["up"].circuit
			c.failures = 1
			c.openUntil = time.Now().Add(-time.Second)

			burstErrs := func() []error {
				errs := make([]error, burst)
				var wg sync.WaitGroup
				for i := range errs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						errs[i] = send(m)
					}()
				}
				wg.Wait()
				return errs
			}

			var fired atomic.Bool
			var atRelease []error
			c.onRelease = func() {
				// CompareAndSwap, not sync.Once: with the old order a request
				// from the burst becomes a probe and releases in turn.
				if fired.CompareAndSwap(false, true) {
					atRelease = burstErrs()
				}
			}

			probeDone := make(chan error, 1)
			go func() { probeDone <- send(m) }()
			<-probeArrived

			for i, err := range burstErrs() {
				if !errors.Is(err, ErrAllUpstreamsUnavailable) {
					t.Errorf("request %d during the probe: err = %v, want ErrAllUpstreamsUnavailable", i, err)
				}
			}
			close(finishProbe)
			if err := <-probeDone; !errors.Is(err, ErrAllUpstreamsUnavailable) {
				t.Errorf("probe err = %v, want ErrAllUpstreamsUnavailable", err)
			}

			if !fired.Load() {
				t.Fatal("probe slot was never released")
			}
			for i, err := range atRelease {
				if !errors.Is(err, ErrAllUpstreamsUnavailable) {
					t.Errorf("request %d as the probe slot freed: err = %v, want ErrAllUpstreamsUnavailable", i, err)
				}
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("upstream saw %d requests, want 1 (only the probe)", n)
			}
			if s := c.Status(time.Now()); s != StatusOpen {
				t.Errorf("circuit status = %v, want open after the failed probe", s)
			}
		})
	}
}

func probeRequest() *chatmodel.ChatRequest {
	return &chatmodel.ChatRequest{Model: "m", Messages: []chatmodel.ChatMessage{{Role: "user"}}}
}
