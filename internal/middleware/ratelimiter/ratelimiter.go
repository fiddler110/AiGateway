// Package ratelimiter enforces a sliding-window requests-per-minute cap per
// client. In-memory only for now (Redis-backed sharing across replicas is
// added in a later phase). Fails closed by default.
package ratelimiter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

const window = 60 * time.Second

type Middleware struct {
	rpm                 int
	useSourceIPFallback bool
	now                 func() time.Time // injectable for tests

	mu       sync.Mutex
	requests map[string][]time.Time
	calls    int // periodic-cleanup counter
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{
		rpm:                 60,
		useSourceIPFallback: true,
		now:                 time.Now,
		requests:            map[string][]time.Time{},
	}
	if v, ok := numberField(cfg, "requests_per_minute"); ok {
		m.rpm = int(v)
	}
	if v, ok := cfg["use_source_ip_fallback"].(bool); ok {
		m.useSourceIPFallback = v
	}
	if m.rpm <= 0 {
		return nil, fmt.Errorf("rate_limiter: requests_per_minute must be positive")
	}
	return m, nil
}

func numberField(cfg map[string]any, key string) (float64, bool) {
	v, ok := cfg[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func (m *Middleware) Name() string { return "rate_limiter" }

// rateKey applies the anonymous-client IP fallback: an anonymous client
// with a known source IP is keyed by IP, so rotating the advisory
// x-client-id header can't be used to bypass the limit.
func rateKey(gctx *pipeline.GatewayContext, useIPFallback bool) string {
	if gctx.ClientID == "anonymous" && useIPFallback && gctx.SourceIP != "" {
		return "ip:" + gctx.SourceIP
	}
	return gctx.ClientID
}

func (m *Middleware) Process(_ context.Context, _ *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	key := rateKey(gctx, m.useSourceIPFallback)
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if m.calls%500 == 0 {
		m.cleanupLocked(now)
	}

	times := pruneOld(m.requests[key], now)
	if len(times) >= m.rpm {
		m.requests[key] = times
		gctx.Blocked = true
		gctx.BlockReason = "rate_limiter: request rate limit exceeded"
		return nil
	}
	m.requests[key] = append(times, now)
	return nil
}

func pruneOld(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-window)
	out := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// cleanupLocked removes keys whose most recent request is stale (>10min),
// bounding memory growth from clients that stop sending requests. Caller
// must hold m.mu.
func (m *Middleware) cleanupLocked(now time.Time) {
	staleCutoff := now.Add(-10 * time.Minute)
	for key, times := range m.requests {
		if len(times) == 0 || times[len(times)-1].Before(staleCutoff) {
			delete(m.requests, key)
		}
	}
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, _ *pipeline.GatewayContext) (string, error) {
	return text, nil // rate_limiter is request-phase only
}
