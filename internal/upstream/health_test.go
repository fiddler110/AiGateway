package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
)

// healthUpstream is a fake upstream that answers every request with a
// settable status, after an optional delay, and records each probe.
type healthUpstream struct {
	*httptest.Server
	status atomic.Int32
	probes atomic.Int32

	mu                             sync.Mutex
	lastMethod, lastPath, lastAuth string
}

func newHealthUpstream(t *testing.T, status int, delay time.Duration) *healthUpstream {
	t.Helper()
	u := &healthUpstream{}
	u.status.Store(int32(status))
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.lastMethod, u.lastPath, u.lastAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		u.mu.Unlock()
		u.probes.Add(1)
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		w.WriteHeader(int(u.status.Load()))
	}))
	t.Cleanup(u.Close)
	return u
}

var fastHealthChecks = config.HealthCheckConfig{Enabled: true, IntervalSeconds: 0.005, TimeoutSeconds: 1}

// newHealthTestManager returns a Manager for ups, closed on cleanup (before
// the fake upstreams it probes, which were registered earlier).
func newHealthTestManager(t *testing.T, hc config.HealthCheckConfig, ups map[string]config.UpstreamConfig) *Manager {
	t.Helper()
	cfg := config.Defaults()
	cfg.Upstreams = ups
	cfg.Resilience.HealthCheck = hc
	m := NewManager(&cfg, &http.Client{}, provider.Registry{})
	t.Cleanup(m.Close)
	return m
}

func waitHealth(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// P0.5: failing probes of health_path make an upstream unavailable after
// three in a row, and one good probe makes it available again. Probes are
// GETs with the upstream's normal auth header.
func TestHealthChecksMarkUnhealthyAndRecover(t *testing.T) {
	t.Setenv("AIGW_HEALTH_TEST_KEY", "health-key")
	up := newHealthUpstream(t, http.StatusServiceUnavailable, 0)
	m := newHealthTestManager(t, fastHealthChecks, map[string]config.UpstreamConfig{
		"a": {BaseURL: up.URL + "/v1/", HealthPath: "/health", APIKeyEnv: "AIGW_HEALTH_TEST_KEY"},
	})
	e := m.entries["a"]
	m.StartHealthChecks()

	waitHealth(t, "upstream marked unhealthy", func() bool { return !e.health.IsHealthy() })
	if n := up.probes.Load(); n < 3 {
		t.Errorf("unhealthy after %d probes, want at least 3", n)
	}
	if available, _ := m.isAvailable(e, time.Now()); available {
		t.Error("unhealthy upstream is still available")
	}
	up.mu.Lock()
	method, path, auth := up.lastMethod, up.lastPath, up.lastAuth
	up.mu.Unlock()
	if method != http.MethodGet || path != "/v1/health" || auth != "Bearer health-key" {
		t.Errorf("probe was %s %s with Authorization %q, want GET /v1/health with the upstream's bearer key", method, path, auth)
	}

	up.status.Store(http.StatusOK)
	waitHealth(t, "upstream healthy again", e.health.IsHealthy)
	if available, _ := m.isAvailable(e, time.Now()); !available {
		t.Error("recovered upstream is unavailable")
	}
}

// Any response below 500 is a good probe (a 401 means reachable), and a
// probe slower than timeout_seconds is a failed one.
func TestHealthCheckVerdicts(t *testing.T) {
	t.Run("4xx is healthy", func(t *testing.T) {
		up := newHealthUpstream(t, http.StatusUnauthorized, 0)
		m := newHealthTestManager(t, fastHealthChecks, map[string]config.UpstreamConfig{"a": {BaseURL: up.URL, HealthPath: "/health"}})
		m.StartHealthChecks()
		waitHealth(t, "five probes", func() bool { return up.probes.Load() >= 5 })
		if !m.entries["a"].health.IsHealthy() {
			t.Error("upstream answering 401 marked unhealthy")
		}
	})
	t.Run("timeout is a failure", func(t *testing.T) {
		up := newHealthUpstream(t, http.StatusOK, 5*time.Second)
		hc := fastHealthChecks
		hc.TimeoutSeconds = 0.02
		m := newHealthTestManager(t, hc, map[string]config.UpstreamConfig{"a": {BaseURL: up.URL, HealthPath: "/health"}})
		m.StartHealthChecks()
		waitHealth(t, "slow upstream marked unhealthy", func() bool { return !m.entries["a"].health.IsHealthy() })
	})
}

// Upstreams without health_path are never probed, and nothing is probed
// while health checks are disabled.
func TestHealthChecksSkipped(t *testing.T) {
	up := newHealthUpstream(t, http.StatusServiceUnavailable, 0)
	disabled := fastHealthChecks
	disabled.Enabled = false
	managers := map[string]*Manager{
		"no health_path": newHealthTestManager(t, fastHealthChecks, map[string]config.UpstreamConfig{"a": {BaseURL: up.URL}}),
		"disabled":       newHealthTestManager(t, disabled, map[string]config.UpstreamConfig{"a": {BaseURL: up.URL, HealthPath: "/health"}}),
	}
	for _, m := range managers {
		m.StartHealthChecks()
	}
	time.Sleep(50 * time.Millisecond)
	if n := up.probes.Load(); n != 0 {
		t.Errorf("%d probes, want 0", n)
	}
	for name, m := range managers {
		if !m.entries["a"].health.IsHealthy() {
			t.Errorf("%s: unprobed upstream marked unhealthy", name)
		}
	}
}

// P0.5: Close stops the checker, so a retired generation doesn't leak one.
// Close is safe to repeat, and a closed Manager doesn't restart.
func TestHealthChecksStopOnClose(t *testing.T) {
	up := newHealthUpstream(t, http.StatusOK, 0)
	m := newHealthTestManager(t, fastHealthChecks, map[string]config.UpstreamConfig{"a": {BaseURL: up.URL, HealthPath: "/health"}})
	m.StartHealthChecks()
	waitHealth(t, "three probes", func() bool { return up.probes.Load() >= 3 })

	m.Close()
	m.Close()
	m.StartHealthChecks()
	time.Sleep(20 * time.Millisecond) // let a request already on the wire land
	n := up.probes.Load()
	time.Sleep(50 * time.Millisecond)
	if got := up.probes.Load(); got != n {
		t.Errorf("%d probes after Close, want 0", got-n)
	}
}
