package ratelimiter

import (
	"context"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func TestRateLimiterBlocksOverThreshold(t *testing.T) {
	mw, err := New(map[string]any{"requests_per_minute": 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	now := time.Now()
	m.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		gctx := pipeline.NewGatewayContext("client-a", "up", "1.2.3.4")
		if err := m.Process(context.Background(), nil, gctx); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if gctx.Blocked {
			t.Fatalf("request %d should not be blocked yet", i)
		}
	}

	gctx := pipeline.NewGatewayContext("client-a", "up", "1.2.3.4")
	if err := m.Process(context.Background(), nil, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !gctx.Blocked {
		t.Error("3rd request within the window should be blocked")
	}
}

func TestRateLimiterWindowExpires(t *testing.T) {
	mw, err := New(map[string]any{"requests_per_minute": 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	now := time.Now()
	m.now = func() time.Time { return now }

	gctx1 := pipeline.NewGatewayContext("client-a", "up", "1.2.3.4")
	_ = m.Process(context.Background(), nil, gctx1)

	now = now.Add(61 * time.Second)
	gctx2 := pipeline.NewGatewayContext("client-a", "up", "1.2.3.4")
	if err := m.Process(context.Background(), nil, gctx2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gctx2.Blocked {
		t.Error("request after the window elapsed should not be blocked")
	}
}

func TestAnonymousUsesSourceIPKey(t *testing.T) {
	mw, err := New(map[string]any{"requests_per_minute": 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	now := time.Now()
	m.now = func() time.Time { return now }

	gctx1 := pipeline.NewGatewayContext("anonymous", "up", "9.9.9.9")
	_ = m.Process(context.Background(), nil, gctx1)

	// Same IP, still anonymous: should be limited together (2nd request
	// blocked), proving the advisory client-id can't be rotated to bypass
	// the limit while the source IP stays constant.
	gctx2 := pipeline.NewGatewayContext("anonymous", "up", "9.9.9.9")
	if err := m.Process(context.Background(), nil, gctx2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !gctx2.Blocked {
		t.Error("second anonymous request from the same source IP should be blocked")
	}
}
