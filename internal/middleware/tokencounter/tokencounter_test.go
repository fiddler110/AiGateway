package tokencounter

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

func TestEstimateAndReconcile(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)

	req := &chatmodel.ChatRequest{Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent("twelve characters here")}}}
	gctx := pipeline.NewGatewayContext("client-a", "up", "")
	if err := m.Process(context.Background(), req, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if gctx.Scratch.EstimatedTokens <= 0 {
		t.Error("expected a positive token estimate")
	}

	actual := 50
	gctx.Scratch.ActualPromptTokens = &actual
	if _, err := m.ProcessResponse(context.Background(), "a short response", gctx); err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	if m.Totals()["client-a"] <= 0 {
		t.Error("expected positive running total after reconciliation")
	}
}

func TestIdempotentFinalization(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")
	gctx.Scratch.EstimatedTokens = 10

	_, _ = m.ProcessResponse(context.Background(), "response text", gctx)
	first := m.Totals()["client-a"]
	_, _ = m.ProcessResponse(context.Background(), "response text", gctx)
	second := m.Totals()["client-a"]
	if first != second {
		t.Error("token finalization should be idempotent per request")
	}
}
