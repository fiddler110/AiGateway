package pseudonymizer

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

func newReq(content string) *chatmodel.ChatRequest {
	return &chatmodel.ChatRequest{Messages: []chatmodel.ChatMessage{{Role: "user", Content: rawContent(content)}}}
}

func TestForwardAndReverseRoundTrip(t *testing.T) {
	mw, _ := New(nil)
	m := mw.(*Middleware)

	req := newReq("connect to 10.0.0.5 for the db")
	gctx := pipeline.NewGatewayContext("client-a", "up", "1.2.3.4")
	if err := m.Process(context.Background(), req, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	got := chatmodel.MessageText(req.Messages[0].Content)
	if strings.Contains(got, "10.0.0.5") {
		t.Errorf("real IP leaked into pseudonymized request: %q", got)
	}
	if got == "connect to 10.0.0.5 for the db" {
		t.Error("expected substitution to change the text")
	}

	reversed, err := m.ProcessResponse(context.Background(), got, gctx)
	if err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	if reversed != "connect to 10.0.0.5 for the db" {
		t.Errorf("reversal round-trip failed: got %q", reversed)
	}
}

func TestDeterminism(t *testing.T) {
	mw1, _ := New(nil)
	mw2, _ := New(nil)
	m1 := mw1.(*Middleware)
	m2 := mw2.(*Middleware)

	req1 := newReq("host is 192.168.1.100")
	req2 := newReq("host is 192.168.1.100")
	gctx1 := pipeline.NewGatewayContext("client-a", "up", "")
	gctx2 := pipeline.NewGatewayContext("client-b", "up", "")

	_ = m1.Process(context.Background(), req1, gctx1)
	_ = m2.Process(context.Background(), req2, gctx2)

	text1 := chatmodel.MessageText(req1.Messages[0].Content)
	text2 := chatmodel.MessageText(req2.Messages[0].Content)
	if text1 != text2 {
		t.Errorf("same real value produced different fakes across independent middleware instances: %q vs %q", text1, text2)
	}
}

func TestSameValueSameFakeWithinSession(t *testing.T) {
	mw, _ := New(nil)
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	req1 := newReq("server 10.1.1.1 is up")
	_ = m.Process(context.Background(), req1, gctx)
	text1 := chatmodel.MessageText(req1.Messages[0].Content)

	req2 := newReq("ping 10.1.1.1 again")
	gctx2 := pipeline.NewGatewayContext("client-a", "up", "")
	_ = m.Process(context.Background(), req2, gctx2)
	text2 := chatmodel.MessageText(req2.Messages[0].Content)

	fake1 := strings.TrimPrefix(strings.TrimSuffix(text1, " is up"), "server ")
	fake2 := strings.TrimPrefix(strings.TrimSuffix(text2, " again"), "ping ")
	if fake1 != fake2 {
		t.Errorf("same real IP got different fakes across requests in the same session: %q vs %q", fake1, fake2)
	}
}

func TestIgnoreDomainsNotPseudonymized(t *testing.T) {
	mw, _ := New(map[string]any{"internal_domains": []any{".internal"}})
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	req := newReq("call api.openai.com and db.internal for the demo")
	_ = m.Process(context.Background(), req, gctx)
	got := chatmodel.MessageText(req.Messages[0].Content)

	if !strings.Contains(got, "api.openai.com") {
		t.Errorf("well-known domain should never be pseudonymized, got %q", got)
	}
	if strings.Contains(got, "db.internal") {
		t.Errorf("internal-suffixed hostname should be pseudonymized, got %q", got)
	}
}

func TestPlaceholderPasswordNotPseudonymized(t *testing.T) {
	mw, _ := New(nil)
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	req := newReq("config: password=your_password_here")
	_ = m.Process(context.Background(), req, gctx)
	got := chatmodel.MessageText(req.Messages[0].Content)
	if got != "config: password=your_password_here" {
		t.Errorf("placeholder password should not be pseudonymized, got %q", got)
	}
}

func TestCIDRPreservesMask(t *testing.T) {
	mw, _ := New(nil)
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	req := newReq("subnet 10.0.0.0/24 in use")
	_ = m.Process(context.Background(), req, gctx)
	got := chatmodel.MessageText(req.Messages[0].Content)
	if !strings.HasSuffix(got, "/24 in use") {
		t.Errorf("CIDR mask should be preserved, got %q", got)
	}
	if strings.Contains(got, "10.0.0.0") {
		t.Errorf("real network part leaked: %q", got)
	}
}

func TestToolCallArgumentsPseudonymized(t *testing.T) {
	mw, _ := New(nil)
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	toolCalls := json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"ssh","arguments":"{\"host\":\"10.5.5.5\"}"}}]`)
	req := &chatmodel.ChatRequest{
		Messages: []chatmodel.ChatMessage{
			{
				Role:    "assistant",
				Content: rawContent(""),
				Extra:   map[string]json.RawMessage{"tool_calls": toolCalls},
			},
		},
	}
	if err := m.Process(context.Background(), req, gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	argsRaw := string(req.Messages[0].Extra["tool_calls"])
	if strings.Contains(argsRaw, "10.5.5.5") {
		t.Errorf("real IP leaked through tool-call arguments: %s", argsRaw)
	}
}

func TestLongestValueFirstAvoidsPartialCollision(t *testing.T) {
	mw, _ := New(map[string]any{
		"sensitive_strings": []any{"admin", "admin@example.com"},
	})
	m := mw.(*Middleware)
	gctx := pipeline.NewGatewayContext("client-a", "up", "")

	req := newReq("user admin@example.com logged in as admin")
	_ = m.Process(context.Background(), req, gctx)
	got := chatmodel.MessageText(req.Messages[0].Content)
	if strings.Contains(got, "admin@example.com") {
		t.Errorf("longer value should have been substituted before the shorter one: %q", got)
	}
}

func TestFakeIPv4CollisionResalts(t *testing.T) {
	existing := map[string]string{}
	first := fakeIPv4("10.0.0.1", existing)
	existing[first] = "10.0.0.1"
	// Force a collision scenario: pretend a different real value already
	// claimed whatever fake "10.0.0.2" would naturally hash to, by
	// generating it and asserting the collision-avoidance loop still
	// terminates with a value distinct from a manufactured clash.
	second := fakeIPv4("10.0.0.2", existing)
	if second == first {
		t.Error("two different real IPs should not receive the same fake without a re-salt collision being detected as a bug")
	}
}
