package upstream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StartHealthChecks starts this Manager's background health checker (P0.5)
// when resilience.health_check is enabled. It probes the health_path of
// every upstream that has one, once at start and then every
// interval_seconds, and records each result in that upstream's HealthState.
// Upstreams without a health_path are never probed and stay healthy, unlike
// the reference, which guesses /health, /v1/models, then the base URL: a
// guessed path that 404s would count as healthy anyway.
//
// The checker belongs to this Manager generation. Call Close when retiring
// it (P1.3 hot reload), or every reload leaks a checker. Calling
// StartHealthChecks again, or after Close, does nothing.
func (m *Manager) StartHealthChecks() {
	hc := m.resilience.HealthCheck
	if !hc.Enabled {
		return
	}
	var probed []*entry
	for _, e := range m.entries {
		if e.cfg.HealthPath != "" {
			probed = append(probed, e)
		}
	}
	if len(probed) == 0 {
		return
	}

	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.healthClosed || m.stopHealth != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.stopHealth = cancel
	interval := time.Duration(hc.IntervalSeconds * float64(time.Second))
	timeout := time.Duration(hc.TimeoutSeconds * float64(time.Second))

	m.healthDone.Add(1)
	go func() {
		defer m.healthDone.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			m.checkAll(ctx, probed, timeout)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Close stops the health checker, if one is running, and waits for its
// in-flight probes to finish. It is safe to call more than once.
func (m *Manager) Close() {
	m.healthMu.Lock()
	m.healthClosed = true
	stop := m.stopHealth
	m.healthMu.Unlock()
	if stop != nil {
		stop()
	}
	m.healthDone.Wait()
}

// checkAll probes every entry concurrently, so one slow upstream doesn't
// delay the others' checks, and returns when all probes are recorded.
func (m *Manager) checkAll(ctx context.Context, entries []*entry, timeout time.Duration) {
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			ok := m.probe(ctx, e, timeout)
			if ctx.Err() != nil {
				return // stopped mid-probe: says nothing about the upstream
			}
			if e.health.RecordCheck(ok, time.Now()) {
				if ok {
					slog.Info("upstream healthy again", "upstream", e.name)
				} else {
					slog.Warn("upstream marked unhealthy after consecutive failed health checks", "upstream", e.name)
				}
			}
		}(e)
	}
	wg.Wait()
}

// probe GETs e's health_path with the upstream's normal auth headers. Like
// the reference, any response below 500 within timeout is healthy: a 401 or
// 404 still means the upstream is reachable and serving, and request-level
// failures are the circuit breaker's job. A 5xx, timeout, or network error
// is a failed probe.
func (m *Manager) probe(ctx context.Context, e *entry, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := strings.TrimRight(e.cfg.BaseURL, "/") + e.cfg.HealthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("health probe failed", "upstream", e.name, "err", err)
		return false
	}
	buildHeaders(req, e.cfg, ResolveAPIKey(e.cfg.APIKeyEnv))
	req.Header.Del("Content-Type")
	resp, err := m.client.Do(req)
	if err != nil {
		slog.Debug("health probe failed", "upstream", e.name, "err", err)
		return false
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
	return resp.StatusCode < 500
}
