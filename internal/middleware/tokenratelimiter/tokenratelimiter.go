// Package tokenratelimiter enforces a sliding-window tokens-per-minute cap
// per client, weighing each request by its estimated size rather than
// simply counting requests. In-memory only for now. Fails closed by
// default.
package tokenratelimiter

import (
	"context"
	"fmt"
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
}

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

	m.mu.Lock()
	defer m.mu.Unlock()

	entries := pruneOld(m.windows[key], now)
	sum := 0
	for _, e := range entries {
		sum += e.tokens
	}
	if sum+estimated > m.tpm {
		m.windows[key] = entries
		gctx.Blocked = true
		gctx.BlockReason = "token_rate_limiter: token rate limit exceeded"
		return nil
	}
	m.windows[key] = append(entries, weightedEntry{at: now, tokens: estimated})
	return nil
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
