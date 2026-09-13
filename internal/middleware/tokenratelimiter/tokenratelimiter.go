// Package tokenratelimiter enforces a sliding-window tokens-per-minute cap
// per client, weighing each request by its estimated size rather than
// simply counting requests. In-memory only for now. Fails closed by
// default.
package tokenratelimiter

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

const window = 60 * time.Second
const charsPerToken = 4

type weightedEntry struct {
	at     time.Time
	tokens int
}

type Middleware struct {
	tpm                 int
	useSourceIPFallback bool
	now                 func() time.Time

	mu      sync.Mutex
	windows map[string][]weightedEntry
	calls   int // periodic-cleanup counter
}

// cleanupEvery and staleAfter match rate_limiter's idle-key eviction.
const (
	cleanupEvery = 500
	staleAfter   = 10 * time.Minute
)

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{
		tpm:                 100000,
		useSourceIPFallback: true,
		now:                 time.Now,
		windows:             map[string][]weightedEntry{},
	}
	if v, ok := numberField(cfg, "tokens_per_minute"); ok {
		m.tpm = int(v)
	}
	if v, ok := cfg["use_source_ip_fallback"].(bool); ok {
		m.useSourceIPFallback = v
	}
	if m.tpm <= 0 {
		return nil, fmt.Errorf("token_rate_limiter: tokens_per_minute must be positive")
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

func (m *Middleware) Name() string { return "token_rate_limiter" }

func rateKey(gctx *pipeline.GatewayContext, useIPFallback bool) string {
	if gctx.ClientID == "anonymous" && useIPFallback && gctx.SourceIP != "" {
		return "ip:" + gctx.SourceIP
	}
	return gctx.ClientID
}

func estimateTokens(chars int) int {
	if chars/charsPerToken < 1 {
		return 1
	}
	return chars / charsPerToken
}

func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	chars := 0
	for _, msg := range req.Messages {
		chars += len(chatmodel.MessageScanText(msg))
	}
	estimated := estimateTokens(chars)

	key := rateKey(gctx, m.useSourceIPFallback)
	now := m.now()

	if estimated > m.tpm {
		// No amount of waiting admits this request, so a 429 with
		// Retry-After would have a client retry forever.
		gctx.Blocked = true
		gctx.BlockReason = "token_rate_limiter: request exceeds the token rate limit"
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if m.calls%cleanupEvery == 0 {
		m.cleanupLocked(now)
	}

	entries := pruneOld(m.windows[key], now)
	sum := 0
	for _, e := range entries {
		sum += e.tokens
	}
	if sum+estimated > m.tpm {
		m.windows[key] = entries
		gctx.Blocked = true
		gctx.BlockReason = "token_rate_limiter: token rate limit exceeded"
		gctx.BlockStatus = http.StatusTooManyRequests
		gctx.RetryAfter = retryAfter(entries, sum+estimated-m.tpm, now)
		return nil
	}
	m.windows[key] = append(entries, weightedEntry{at: now, tokens: estimated})
	return nil
}

// retryAfter is how long until the oldest entries (in time order) holding at
// least excess tokens leave the window.
func retryAfter(entries []weightedEntry, excess int, now time.Time) time.Duration {
	freed := 0
	for _, e := range entries {
		freed += e.tokens
		if freed >= excess {
			return e.at.Add(window).Sub(now)
		}
	}
	return window
}

// cleanupLocked removes keys whose most recent entry is older than
// staleAfter, so IP-keyed anonymous clients don't grow the map forever.
// Caller must hold m.mu.
func (m *Middleware) cleanupLocked(now time.Time) {
	staleCutoff := now.Add(-staleAfter)
	for key, entries := range m.windows {
		if len(entries) == 0 || entries[len(entries)-1].at.Before(staleCutoff) {
			delete(m.windows, key)
		}
	}
}

func pruneOld(entries []weightedEntry, now time.Time) []weightedEntry {
	cutoff := now.Add(-window)
	out := entries[:0]
	for _, e := range entries {
		if e.at.After(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, _ *pipeline.GatewayContext) (string, error) {
	return text, nil // token_rate_limiter is request-phase only
}
