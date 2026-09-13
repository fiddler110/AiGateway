// Package tokencounter estimates and reconciles per-client token usage.
// Fails open by default: a bug here shouldn't block traffic.
package tokencounter

import (
	"context"
	"sync"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// charsPerToken is the rough estimator used before the provider reports
// actual usage (chars/4 is the same heuristic the Python reference uses).
const charsPerToken = 4

func estimateTokens(chars int) int {
	if chars/charsPerToken < 1 {
		return 1
	}
	return chars / charsPerToken
}

type Middleware struct {
	mu     sync.Mutex
	totals map[string]int // client_id -> total tokens, in-memory fallback for /metrics
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	return &Middleware{totals: map[string]int{}}, nil
}

func (m *Middleware) Name() string { return "token_counter" }

func (m *Middleware) add(clientID string, delta int) {
	m.mu.Lock()
	m.totals[clientID] += delta
	m.mu.Unlock()
}

// Totals returns a snapshot of per-client token totals, used by /metrics
// when no DB-backed totals are available.
func (m *Middleware) Totals() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.totals))
	for k, v := range m.totals {
		out[k] = v
	}
	return out
}

func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	chars := 0
	for _, msg := range req.Messages {
		chars += len(chatmodel.MessageScanText(msg))
	}
	estimated := estimateTokens(chars)
	gctx.Scratch.EstimatedTokens = estimated
	m.add(gctx.ClientID, estimated)
	return nil
}

// ProcessResponse reconciles the estimate against actual provider-reported
// usage exactly once per request (idempotency guard: the response pipeline
// may run once per choice in a multi-choice response).
func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	if gctx.Scratch.TokensFinalized {
		return text, nil
	}
	gctx.Scratch.TokensFinalized = true

	adjustment := 0
	if gctx.Scratch.ActualPromptTokens != nil {
		adjustment = *gctx.Scratch.ActualPromptTokens - gctx.Scratch.EstimatedTokens
	}
	completionTokens := estimateTokens(len(text))
	if gctx.Scratch.ActualComplTokens != nil {
		completionTokens = *gctx.Scratch.ActualComplTokens
	}
	m.add(gctx.ClientID, adjustment+completionTokens)
	return text, nil
}
