// Package upstream implements outbound request forwarding with per-upstream
// circuit breaking, retry/fallback across a priority-ordered upstream list,
// and background health checks.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
)

type entry struct {
	name    string
	cfg     config.UpstreamConfig
	circuit *CircuitState
	health  *HealthState
}

// Manager resolves model-route fallback lists into actual HTTP calls,
// applying circuit-breaker/health availability checks, linear-backoff
// retries, and fallback to the next upstream on exhaustion.
type Manager struct {
	entries    map[string]*entry
	resilience config.ResilienceConfig
	client     *http.Client
	registry   provider.Registry
}

func NewManager(cfg *config.Config, client *http.Client, registry provider.Registry) *Manager {
	m := &Manager{
		entries:    map[string]*entry{},
		resilience: cfg.Resilience,
		client:     client,
		registry:   registry,
	}
	for name, up := range cfg.Upstreams {
		m.entries[name] = &entry{name: name, cfg: up, circuit: &CircuitState{}, health: NewHealthState()}
	}
	return m
}

// WithMergedConfig returns a new Manager for a reloaded config, copying
// forward circuit/health state for upstreams that already existed so a hot
// reload doesn't reset breaker/health history for still-known upstreams.
func (m *Manager) WithMergedConfig(cfg *config.Config, client *http.Client, registry provider.Registry) *Manager {
	next := &Manager{
		entries:    map[string]*entry{},
		resilience: cfg.Resilience,
		client:     client,
		registry:   registry,
	}
	for name, up := range cfg.Upstreams {
		if old, ok := m.entries[name]; ok {
			next.entries[name] = &entry{name: name, cfg: up, circuit: old.circuit, health: old.health}
		} else {
			next.entries[name] = &entry{name: name, cfg: up, circuit: &CircuitState{}, health: NewHealthState()}
		}
	}
	return next
}

// Status reports {healthy, circuit, failures} per upstream for /health and
// /metrics.
type UpstreamStatus struct {
	Healthy bool   `json:"healthy"`
	Circuit string `json:"circuit"`
	Failures int   `json:"failures"`
}

func (m *Manager) Status() map[string]UpstreamStatus {
	now := time.Now()
	out := make(map[string]UpstreamStatus, len(m.entries))
	for name, e := range m.entries {
		var circuitName string
		switch e.circuit.Status(now) {
		case StatusOpen:
			circuitName = "open"
		case StatusHalfOpen:
			circuitName = "half_open"
		default:
			circuitName = "closed"
		}
		e.circuit.mu.Lock()
		failures := e.circuit.failures
		e.circuit.mu.Unlock()
		out[name] = UpstreamStatus{Healthy: e.health.IsHealthy(), Circuit: circuitName, Failures: failures}
	}
	return out
}

// isAvailable combines circuit and health state: an OPEN circuit, or a
// HALF_OPEN circuit whose single probe slot is already taken, makes the
// upstream unavailable; a CLOSED circuit whose health check reports
// unhealthy (and health checks are enabled) is also unavailable.
func (m *Manager) isAvailable(e *entry, now time.Time) (available bool, halfOpenProbe bool) {
	switch e.circuit.Status(now) {
	case StatusOpen:
		return false, false
	case StatusHalfOpen:
		if e.circuit.TryAcquireHalfOpenProbe(now) {
			return true, true
		}
		return false, false
	default: // StatusClosed
		if m.resilience.HealthCheck.Enabled && !e.health.IsHealthy() {
			return false, false
		}
		return true, false
	}
}

var ErrAllUpstreamsUnavailable = errors.New("all upstreams unavailable")

// translatorFor resolves the Translator for an upstream's configured
// api_format, defaulting to openai (identity) if unset/unknown.
func (m *Manager) translatorFor(cfg config.UpstreamConfig) provider.Translator {
	format := cfg.APIFormat
	if format == "" {
		format = "openai"
	}
	if ctor, ok := m.registry[format]; ok {
		return ctor()
	}
	return m.registry["openai"]()
}

// Send performs a non-streaming chat call, trying candidates in fallback
// order. A response status < 500 counts as success for circuit-breaker
// purposes (2xx and 4xx client errors like 400/401/429 are NOT upstream
// failures — only 5xx and network errors are). Returns the canonical
// OpenAI-shaped response body, usage, and the name of the upstream that
// actually served the request.
func (m *Manager) Send(ctx context.Context, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(upstreamName string) string) (body []byte, usage provider.Usage, servedBy string, err error) {
	var lastErr error
	for _, name := range candidates {
		e, ok := m.entries[name]
		if !ok {
			continue
		}
		for attempt := 0; attempt <= m.resilience.RetryAttempts; attempt++ {
			now := time.Now()
			available, halfOpen := m.isAvailable(e, now)
			if !available {
				lastErr = fmt.Errorf("upstream %s unavailable", name)
				break // try next upstream, not another attempt on this one
			}

			translator := m.translatorFor(e.cfg)
			nativeBody, defaultPath, terr := translator.ToUpstream(req)
			if terr != nil {
				if halfOpen {
					e.circuit.ReleaseHalfOpenProbe()
				}
				return nil, provider.Usage{}, "", fmt.Errorf("translate request for %s: %w", name, terr)
			}

			resp, ferr := forward(ctx, m.client, UpstreamRequest{
				Upstream: e.cfg,
				APIKey:   apiKeyFor(name),
				Body:     nativeBody,
				Path:     chatPath(e.cfg, defaultPath),
			})
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}

			if ferr != nil {
				e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
				lastErr = ferr
				m.backoff(ctx, attempt)
				continue
			}

			respBody, rerr := readAndClose(resp)
			if rerr != nil {
				e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
				lastErr = rerr
				m.backoff(ctx, attempt)
				continue
			}

			if resp.StatusCode >= 500 {
				e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
				lastErr = fmt.Errorf("upstream %s returned status %d", name, resp.StatusCode)
				m.backoff(ctx, attempt)
				continue
			}

			e.circuit.RecordSuccess()
			openaiBody, u, terr := translator.FromUpstream(respBody)
			if terr != nil {
				return nil, provider.Usage{}, "", fmt.Errorf("translate response from %s: %w", name, terr)
			}
			return openaiBody, u, name, nil
		}
	}
	if lastErr != nil {
		return nil, provider.Usage{}, "", fmt.Errorf("%w: %v", ErrAllUpstreamsUnavailable, lastErr)
	}
	return nil, provider.Usage{}, "", ErrAllUpstreamsUnavailable
}

// SendStream performs a streaming chat call, trying candidates in fallback
// order with no retry-within-upstream (a partially-streamed response can't
// be safely retried once bytes may have reached the client). Success is
// recorded once response headers/status are received (before any body is
// read) — a deliberate deviation from the Python reference, which recorded
// success per received chunk and could record success just before an
// eventual mid-stream failure. A network error while reading the body AFTER
// successful headers is surfaced to the caller as a truncated stream, not
// fed back into the circuit breaker, since retrying isn't safe at that point
// and it's a distinct failure mode (dropped connection) from "is this
// upstream reachable."
func (m *Manager) SendStream(ctx context.Context, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(upstreamName string) string) (io.ReadCloser, string, error) {
	var lastErr error
	for _, name := range candidates {
		e, ok := m.entries[name]
		if !ok {
			continue
		}
		now := time.Now()
		available, halfOpen := m.isAvailable(e, now)
		if !available {
			lastErr = fmt.Errorf("upstream %s unavailable", name)
			continue
		}

		translator := m.translatorFor(e.cfg)
		nativeBody, defaultPath, terr := translator.ToUpstream(req)
		if terr != nil {
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			return nil, "", fmt.Errorf("translate request for %s: %w", name, terr)
		}

		resp, ferr := forwardStream(ctx, m.client, UpstreamRequest{
			Upstream: e.cfg,
			APIKey:   apiKeyFor(name),
			Body:     nativeBody,
			Path:     chatPath(e.cfg, defaultPath),
		})
		if ferr != nil {
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
			lastErr = ferr
			continue
		}

		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
			lastErr = fmt.Errorf("upstream %s returned status %d: %s", name, resp.StatusCode, string(body))
			continue
		}

		// Headers received and status < 500: record success now, before any
		// body byte is read (see doc comment above for why).
		e.circuit.RecordSuccess()
		if halfOpen {
			e.circuit.ReleaseHalfOpenProbe()
		}
		return resp.Body, name, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrAllUpstreamsUnavailable, lastErr)
	}
	return nil, "", ErrAllUpstreamsUnavailable
}

func cooldown(r config.ResilienceConfig) time.Duration {
	return time.Duration(r.CircuitBreaker.CooldownSeconds * float64(time.Second))
}

// backoff sleeps a linear delay between retry attempts on the SAME
// upstream (not between different upstreams in the fallback list), honoring
// context cancellation.
func (m *Manager) backoff(ctx context.Context, attempt int) {
	delay := time.Duration(float64(attempt+1)) * time.Second
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
}

func readAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
