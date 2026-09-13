package tokenratelimiter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func request(chars int) *chatmodel.ChatRequest {
	content, _ := json.Marshal(strings.Repeat("x", chars))
	return &chatmodel.ChatRequest{Messages: []chatmodel.ChatMessage{{Role: "user", Content: content}}}
}

func newLimiter(t *testing.T, tpm int, now *time.Time) *Middleware {
	t.Helper()
	mw, err := New(map[string]any{"tokens_per_minute": tpm})
	if err != nil {
		t.Fatal(err)
	}
	m := mw.(*Middleware)
	m.now = func() time.Time { return *now }
	return m
}

// P0.14: idle keys were never evicted, so every source IP an anonymous client
// ever used stayed in the map forever.
func TestTokenRateLimiterEvictsIdleKeys(t *testing.T) {
	now := time.Now()
	m := newLimiter(t, 1000, &now)

	idle := pipeline.NewGatewayContext("anonymous", "up", "198.51.100.7")
	if err := m.Process(context.Background(), request(40), idle); err != nil {
		t.Fatal(err)
	}
	// Literals rather than staleAfter/cleanupEvery, so this test also
	// compiles (and fails) against the pre-fix code.
	now = now.Add(11 * time.Minute)
	for i := 1; i < 500; i++ {
		gctx := pipeline.NewGatewayContext("active", "up", "")
		if err := m.Process(context.Background(), request(4), gctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Millisecond)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.windows["ip:198.51.100.7"]; ok {
		t.Error("idle key still present after cleanup")
	}
	if _, ok := m.windows["active"]; !ok {
		t.Error("active key was evicted")
	}
}

// P0.14: a rate-limit block is a 429 with a Retry-After that covers the wait
// until enough tokens leave the window; a request larger than the whole limit
// can never pass, so it is a plain 400 block.
func TestTokenRateLimiterBlockStatus(t *testing.T) {
	now := time.Now()
	m := newLimiter(t, 100, &now)

	first := pipeline.NewGatewayContext("c", "up", "")
	_ = m.Process(context.Background(), request(240), first) // 60 tokens
	now = now.Add(10 * time.Second)
	second := pipeline.NewGatewayContext("c", "up", "")
	_ = m.Process(context.Background(), request(120), second) // 30 tokens
	if first.Blocked || second.Blocked {
		t.Fatal("requests under the limit were blocked")
	}

	now = now.Add(5 * time.Second)
	over := pipeline.NewGatewayContext("c", "up", "")
	_ = m.Process(context.Background(), request(80), over) // 20 tokens: 110 > 100
	if !over.Blocked || over.BlockStatus != http.StatusTooManyRequests {
		t.Fatalf("blocked %t status %d, want 429", over.Blocked, over.BlockStatus)
	}
	// Freeing the first entry (60 tokens) is enough; it leaves at +60s,
	// 45s from now.
	if over.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", over.RetryAfter)
	}

	huge := pipeline.NewGatewayContext("other", "up", "")
	_ = m.Process(context.Background(), request(404), huge) // 101 tokens
	if !huge.Blocked || huge.BlockHTTPStatus() != http.StatusBadRequest || huge.RetryAfter != 0 {
		t.Errorf("oversized request: blocked %t status %d retry %v, want a 400 block without Retry-After",
			huge.Blocked, huge.BlockHTTPStatus(), huge.RetryAfter)
	}
}
