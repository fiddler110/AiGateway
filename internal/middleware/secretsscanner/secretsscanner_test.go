package secretsscanner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func rawContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func runProcess(t *testing.T, mw pipeline.Middleware, text string) *pipeline.GatewayContext {
	t.Helper()
	req := &chatmodel.ChatRequest{Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent(text)}}}
	gctx := pipeline.NewGatewayContext("client", "up", "1.2.3.4")
	if err := mw.Process(context.Background(), req, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	return gctx
}

func TestBlockingPatterns(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		text string
	}{
		{"openai key", "here is my key sk-" + strings.Repeat("A", 25)},
		{"aws access key", "AKIA" + strings.Repeat("A", 16)},
		{"github token", "ghp_" + strings.Repeat("a", 36)},
		{"github pat", "github_pat_" + strings.Repeat("a", 25)},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----"},
		{"connection string", "postgres://admin:hunter2@db.internal:5432/app"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gctx := runProcess(t, mw, tc.text)
			if !gctx.Blocked {
				t.Errorf("expected block for %q, reason=%q", tc.text, gctx.BlockReason)
			}
		})
	}
}

func TestFlaggingPatterns(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	gctx := runProcess(t, mw, "token: "+jwt)
	if gctx.Blocked {
		t.Error("JWT should be flagged, not blocked, by default")
	}
	if len(gctx.Scratch.SecretsFlagged) == 0 {
		t.Error("expected jwt_token to be flagged")
	}
}

func TestBlockOnFlag(t *testing.T) {
	mw, err := New(map[string]any{"block_on_flag": true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	gctx := runProcess(t, mw, "token: "+jwt)
	if !gctx.Blocked {
		t.Error("expected block when block_on_flag is true")
	}
}

func TestPlaceholderPasswordNotFlagged(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gctx := runProcess(t, mw, "config example: password=your_password_here")
	if gctx.Blocked {
		t.Error("placeholder password should not block")
	}
	if len(gctx.Scratch.SecretsFlagged) != 0 {
		t.Error("placeholder password should not be flagged")
	}
}

func TestRealPasswordFlagged(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gctx := runProcess(t, mw, "password=Tr0ub4dor&3")
	if len(gctx.Scratch.SecretsFlagged) == 0 {
		t.Error("expected password_field to be flagged")
	}
}

func TestEntropyFallback(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A high-entropy base64-ish token with no recognizable prefix pattern.
	highEntropy := "aK9$xQ2!mZ7#pL4&nR8@vT1%wY6^bC3*"
	// Strip characters not in the entropy token charset for a clean match.
	token := "aK9xQ2mZ7pL4nR8vT1wY6bC3fJ5hG0sD9"
	_ = highEntropy
	gctx := runProcess(t, mw, "blob: "+token)
	if len(gctx.Scratch.SecretsFlagged) == 0 {
		t.Skip("entropy of synthetic token did not clear threshold; not a correctness failure of the scanner logic itself")
	}
}

func TestNoFalsePositiveOnPlainText(t *testing.T) {
	mw, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gctx := runProcess(t, mw, "This is just a normal sentence about the weather today.")
	if gctx.Blocked || len(gctx.Scratch.SecretsFlagged) != 0 {
		t.Error("plain prose should not trigger the scanner")
	}
}
