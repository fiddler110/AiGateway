// Package upstream implements outbound request forwarding with per-upstream
// circuit breaking, retry/fallback across a priority-ordered upstream list,
// and background health checks.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
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
	// maxResponseBytes bounds every upstream response body (P0.9).
	maxResponseBytes int64
	// sleep and randFloat are the backoff's clock and jitter source,
	// swappable so tests neither really sleep nor depend on randomness.
	sleep     func(ctx context.Context, d time.Duration) error
	randFloat func() float64 // uniform in [0, 1)
}

// sleepCtx waits for d or until ctx is done, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func NewManager(cfg *config.Config, client *http.Client, registry provider.Registry) *Manager {
	m := &Manager{
		entries:          map[string]*entry{},
		resilience:       cfg.Resilience,
		client:           client,
		registry:         registry,
		maxResponseBytes: cfg.Settings.MaxResponseBytes,
		sleep:            sleepCtx,
		randFloat:        rand.Float64,
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
		entries:          map[string]*entry{},
		resilience:       cfg.Resilience,
		client:           client,
		registry:         registry,
		maxResponseBytes: cfg.Settings.MaxResponseBytes,
		sleep:            m.sleep,
		randFloat:        m.randFloat,
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
	Healthy  bool   `json:"healthy"`
	Circuit  string `json:"circuit"`
	Failures int    `json:"failures"`
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

// maxErrorBodyBytes bounds how much of an upstream error body is read. Error
// bodies are only mined for a short message, never relayed whole.
const maxErrorBodyBytes = 64 * 1024

// isUpstreamFailure reports whether a response status counts against the
// upstream's circuit breaker and triggers retry/fallback. 4xx responses are
// the upstream correctly rejecting this request (bad input, rate limit,
// unknown model), so they are relayed to the client instead: retrying them
// elsewhere would hide the real error. Anything that is neither 2xx nor 4xx
// (5xx, or a 1xx/3xx the client didn't resolve) is a failure.
func isUpstreamFailure(status int) bool {
	return status < 200 || (status >= 300 && status < 400) || status >= 500
}

// Result is a completed non-streaming upstream call. Status is always 2xx or
// 4xx: failures are retried or reported as an error instead.
type Result struct {
	Status int
	Header http.Header
	// Body is the canonical OpenAI-shaped response for a 2xx, or the
	// upstream's untranslated error body for a 4xx.
	Body     []byte
	Usage    provider.Usage
	ServedBy string
}

// translatorFor resolves the Translator for an upstream's configured
// api_format; unset means openai (identity). A format with no registered
// translator is an error: silently speaking OpenAI to, say, an Anthropic
// endpoint would send the wrong wire format and misparse the reply (P0.13).
// config.Validate rejects such formats at load, so this is a backstop.
func (m *Manager) translatorFor(cfg config.UpstreamConfig) (provider.Translator, error) {
	format := cfg.APIFormat
	if format == "" {
		format = "openai"
	}
	ctor, ok := m.registry[format]
	if !ok {
		return nil, fmt.Errorf("no translator registered for api_format %q", format)
	}
	return ctor(), nil
}

// Send performs a non-streaming chat call, trying candidates in fallback
// order. Only failure statuses (see isUpstreamFailure) and network errors
// count against the circuit breaker; a 4xx is returned to the caller with
// its status so the client sees the upstream's real answer (P0.2).
func (m *Manager) Send(ctx context.Context, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(upstreamName string) string) (Result, error) {
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

			translator, terr := m.translatorFor(e.cfg)
			if terr != nil {
				if halfOpen {
					e.circuit.ReleaseHalfOpenProbe()
				}
				return Result{}, fmt.Errorf("upstream %s: %w", name, terr)
			}
			nativeBody, defaultPath, terr := translator.ToUpstream(req)
			if terr != nil {
				if halfOpen {
					e.circuit.ReleaseHalfOpenProbe()
				}
				return Result{}, fmt.Errorf("translate request for %s: %w", name, terr)
			}

			res, ferr := forward(ctx, m.client, UpstreamRequest{
				Upstream: e.cfg,
				APIKey:   apiKeyFor(name),
				Body:     nativeBody,
				Path:     chatPath(e.cfg, defaultPath),
				MaxBytes: m.maxResponseBytes,
			})
			if errors.Is(ferr, ErrResponseTooLarge) && !isUpstreamFailure(res.status) {
				// The upstream is reachable and answered; it just answered
				// too much. Retrying or falling back would repeat a runaway
				// generation, so this is terminal and not a circuit failure.
				e.circuit.RecordSuccess()
				return Result{}, fmt.Errorf("upstream %s: %w", name, ferr)
			}
			if ferr == nil && isUpstreamFailure(res.status) {
				ferr = fmt.Errorf("upstream %s returned status %d", name, res.status)
			}
			if ferr != nil {
				e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
				if halfOpen {
					e.circuit.ReleaseHalfOpenProbe()
				}
				lastErr = ferr
				if attempt < m.resilience.RetryAttempts {
					// Never after the final attempt: the next step is a
					// different upstream (or giving up), not this one again.
					if err := m.sleep(ctx, m.backoffDelay(attempt)); err != nil {
						return Result{}, fmt.Errorf("retry backoff for upstream %s: %w (last error: %v)", name, err, lastErr)
					}
				}
				continue
			}

			e.circuit.RecordSuccess()
			if res.status >= 400 {
				return Result{Status: res.status, Header: res.header, Body: res.body, ServedBy: name}, nil
			}
			openaiBody, u, terr := translator.FromUpstream(res.body)
			if terr != nil {
				return Result{}, fmt.Errorf("translate response from %s: %w", name, terr)
			}
			return Result{Status: res.status, Header: res.header, Body: openaiBody, Usage: u, ServedBy: name}, nil
		}
	}
	if lastErr != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrAllUpstreamsUnavailable, lastErr)
	}
	return Result{}, ErrAllUpstreamsUnavailable
}

// StreamResult is a streaming upstream call whose headers have arrived.
// Exactly one of Body (2xx) or ErrorBody (4xx) is set.
type StreamResult struct {
	Status int
	Header http.Header
	// Body is the live upstream stream; the caller must close it. Reading
	// more than settings.max_response_bytes fails with ErrResponseTooLarge.
	Body io.ReadCloser
	// ErrorBody is the upstream's error body, read to at most
	// maxErrorBodyBytes, with the connection already closed.
	ErrorBody []byte
	ServedBy  string
}

// SendStream performs a streaming chat call, trying candidates in fallback
// order with no retry-within-upstream (a partially-streamed response can't
// be safely retried once bytes may have reached the client), so it never
// backs off: retry_attempts and retry_delay_seconds apply to Send only. Success is
// recorded once response headers/status are received (before any body is
// read) — a deliberate deviation from the Python reference, which recorded
// success per received chunk and could record success just before an
// eventual mid-stream failure. A network error while reading the body AFTER
// successful headers is surfaced to the caller as a truncated stream, not
// fed back into the circuit breaker, since retrying isn't safe at that point
// and it's a distinct failure mode (dropped connection) from "is this
// upstream reachable."
func (m *Manager) SendStream(ctx context.Context, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(upstreamName string) string) (StreamResult, error) {
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

		translator, terr := m.translatorFor(e.cfg)
		if terr != nil {
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			return StreamResult{}, fmt.Errorf("upstream %s: %w", name, terr)
		}
		nativeBody, defaultPath, terr := translator.ToUpstream(req)
		if terr != nil {
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			return StreamResult{}, fmt.Errorf("translate request for %s: %w", name, terr)
		}

		resp, ferr := forwardStream(ctx, m.client, UpstreamRequest{
			Upstream: e.cfg,
			APIKey:   apiKeyFor(name),
			Body:     nativeBody,
			Path:     chatPath(e.cfg, defaultPath),
		})
		if ferr == nil && isUpstreamFailure(resp.StatusCode) {
			// The body is drained (bounded) for connection reuse but never
			// put in the error: it can carry internal detail (P0.8).
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			ferr = fmt.Errorf("upstream %s returned status %d", name, resp.StatusCode)
		}
		if ferr != nil {
			e.circuit.RecordFailure(m.resilience.CircuitBreaker.FailureThreshold, cooldown(m.resilience), now)
			if halfOpen {
				e.circuit.ReleaseHalfOpenProbe()
			}
			lastErr = ferr
			continue
		}

		// Headers received and not a failure status: record success now,
		// before any body byte is read (see doc comment above for why).
		e.circuit.RecordSuccess()
		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
			return StreamResult{Status: resp.StatusCode, Header: resp.Header, ErrorBody: errBody, ServedBy: name}, nil
		}
		// Reads past maxResponseBytes fail with ErrResponseTooLarge (P0.9).
		// The limit bounds native bytes read off the network; the stream
		// translator then turns them into canonical OpenAI SSE (P0.13), so
		// the handler never sees the provider's wire format.
		native := limitedBody{Reader: LimitReader(resp.Body, m.maxResponseBytes), Closer: resp.Body}
		body := newTranslatedStream(native, translator.NewStreamTranslator(), name)
		return StreamResult{Status: resp.StatusCode, Header: resp.Header, Body: body, ServedBy: name}, nil
	}
	if lastErr != nil {
		return StreamResult{}, fmt.Errorf("%w: %v", ErrAllUpstreamsUnavailable, lastErr)
	}
	return StreamResult{}, ErrAllUpstreamsUnavailable
}

func cooldown(r config.ResilienceConfig) time.Duration {
	return time.Duration(r.CircuitBreaker.CooldownSeconds * float64(time.Second))
}

// jitterFraction spreads each backoff delay uniformly over ±20% so clients
// that failed together don't retry in lockstep.
const jitterFraction = 0.2

// backoffDelay is the wait before retry attempt+1 on the SAME upstream:
// linear, retry_delay_seconds × (attempt+1) as in the Python reference, then
// jittered by ±jitterFraction. It is never used between different upstreams
// in the fallback list or after the final attempt.
func (m *Manager) backoffDelay(attempt int) time.Duration {
	base := m.resilience.RetryDelaySeconds * float64(attempt+1)
	jitter := 1 + jitterFraction*(2*m.randFloat()-1)
	d := base * jitter * float64(time.Second)
	if !(d > 0) { // zero, negative, or NaN
		return 0
	}
	if d >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(d)
}
