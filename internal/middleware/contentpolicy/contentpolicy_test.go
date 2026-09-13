package contentpolicy

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

func TestProcess(t *testing.T) {
	cases := []struct {
		name        string
		patterns    []any
		content     string
		wantBlocked bool
	}{
		{"no patterns configured", nil, "anything goes", false},
		{"no match", []any{"forbidden"}, "hello world", false},
		{"case-insensitive match", []any{"forbidden"}, "this is FORBIDDEN content", true},
		{"regex match", []any{`\bsecret\b`}, "the secret plan", true},
		{"regex no partial-word match", []any{`\bsecret\b`}, "secretary", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw, err := New(map[string]any{"deny_patterns": tc.patterns})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := &chatmodel.ChatRequest{
				Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent(tc.content)}},
			}
			gctx := pipeline.NewGatewayContext("client", "up", "1.2.3.4")
			if err := mw.Process(context.Background(), req, gctx); err != nil {
				t.Fatalf("Process: %v", err)
			}
			if gctx.Blocked != tc.wantBlocked {
				t.Errorf("Blocked = %v, want %v (reason=%q)", gctx.Blocked, tc.wantBlocked, gctx.BlockReason)
			}
			if tc.wantBlocked {
				if gctx.BlockReason == "" {
					t.Error("expected a block reason")
				}
				// The matched pattern must never leak into BlockReason.
				for _, p := range tc.patterns {
					if s, ok := p.(string); ok && gctx.BlockReason == s {
						t.Errorf("block reason leaked the matched pattern: %q", s)
					}
				}
			}
		})
	}
}

func TestProcessResponse(t *testing.T) {
	mw, err := New(map[string]any{"deny_patterns": []any{"blocked-word"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gctx := pipeline.NewGatewayContext("client", "up", "1.2.3.4")
	text, err := mw.ProcessResponse(context.Background(), "this has a blocked-word in it", gctx)
	if err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	if !gctx.Blocked {
		t.Error("expected response to be blocked")
	}
	if gctx.BlockReason != "content_policy: response blocked by policy" {
		t.Errorf("unexpected block reason: %q", gctx.BlockReason)
	}
	if text == "" {
		t.Error("ProcessResponse should still return the text")
	}
}
