package costtracker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func rawContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestBudgetEnforced(t *testing.T) {
	mw, err := New(map[string]any{
		"pricing": map[string]any{
			"gpt-4": map[string]any{"prompt_per_1k": 1000.0, "completion_per_1k": 1000.0},
		},
		"budgets": map[string]any{"default": 0.01},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)

	req := &chatmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent("this is a moderately long message to rack up estimated cost")}},
	}
	gctx := pipeline.NewGatewayContext("client-a", "up", "")
	if err := m.Process(context.Background(), req, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gctx.Blocked {
		t.Fatalf("first request should not be blocked, cost=%v", gctx.Scratch.EstimatedCost)
	}

	// Second request should now exceed the tiny budget given the expensive
	// per-1k rate configured above.
	gctx2 := pipeline.NewGatewayContext("client-a", "up", "")
	if err := m.Process(context.Background(), req, gctx2); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !gctx2.Blocked {
		t.Error("expected budget to be exceeded on the second request")
	}
}

func TestExactModelMatchBeatsSubstring(t *testing.T) {
	mw, err := New(map[string]any{
		"pricing": map[string]any{
			"gpt-4":  map[string]any{"prompt_per_1k": 1.0},
			"gpt-4o": map[string]any{"prompt_per_1k": 2.0},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	r := m.rateFor("gpt-4o")
	if r.promptPer1K != 2.0 {
		t.Errorf("expected exact match for gpt-4o (rate 2.0), got %v", r.promptPer1K)
	}
}

func TestSubstringMatchPrefersLongestKey(t *testing.T) {
	mw, err := New(map[string]any{
		"pricing": map[string]any{
			"gpt-4":  map[string]any{"prompt_per_1k": 1.0},
			"gpt-4o": map[string]any{"prompt_per_1k": 2.0},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	// "gpt-4o-mini" contains both "gpt-4" and "gpt-4o" as substrings; the
	// longer, more specific key should win rather than depending on map
	// iteration order (a deliberate fix over the Python reference).
	r := m.rateFor("gpt-4o-mini")
	if r.promptPer1K != 2.0 {
		t.Errorf("expected longest-substring match gpt-4o (rate 2.0), got %v", r.promptPer1K)
	}
}

func TestResponseReconciliation(t *testing.T) {
	mw, err := New(map[string]any{
		"pricing": map[string]any{
			"gpt-4": map[string]any{"prompt_per_1k": 10.0, "completion_per_1k": 20.0},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)

	req := &chatmodel.ChatRequest{Model: "gpt-4", Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent("hi")}}}
	gctx := pipeline.NewGatewayContext("client-a", "up", "")
	_ = m.Process(context.Background(), req, gctx)

	actualPrompt := 100
	gctx.Scratch.ActualPromptTokens = &actualPrompt
	if _, err := m.ProcessResponse(context.Background(), "a response with some words in it", gctx); err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	if gctx.Scratch.TotalSpend <= 0 {
		t.Error("expected non-zero total spend after reconciliation")
	}

	// Idempotency: a second call must not double-charge.
	spendAfterFirst := gctx.Scratch.TotalSpend
	if _, err := m.ProcessResponse(context.Background(), "a response with some words in it", gctx); err != nil {
		t.Fatalf("ProcessResponse (2nd): %v", err)
	}
	if gctx.Scratch.TotalSpend != spendAfterFirst {
		t.Error("cost finalization should be idempotent per request")
	}
}
