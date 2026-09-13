package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
)

type upperMW struct{ calls int }

func (m *upperMW) Name() string                                                           { return "upper" }
func (m *upperMW) Process(context.Context, *chatmodel.ChatRequest, *GatewayContext) error { return nil }
func (m *upperMW) ProcessResponse(_ context.Context, text string, _ *GatewayContext) (string, error) {
	m.calls++
	return strings.ToUpper(text), nil
}

type accountingMW struct{ texts []string }

func (m *accountingMW) Name() string           { return "acct" }
func (m *accountingMW) AccountsWholeResponse() {}
func (m *accountingMW) Process(context.Context, *chatmodel.ChatRequest, *GatewayContext) error {
	return nil
}
func (m *accountingMW) ProcessResponse(_ context.Context, text string, _ *GatewayContext) (string, error) {
	m.texts = append(m.texts, text)
	return "ignored", nil
}

// failSecondMW rewrites the first field, then errors on the second.
type failSecondMW struct{ calls int }

func (m *failSecondMW) Name() string { return "fail" }
func (m *failSecondMW) Process(context.Context, *chatmodel.ChatRequest, *GatewayContext) error {
	return nil
}
func (m *failSecondMW) ProcessResponse(_ context.Context, text string, _ *GatewayContext) (string, error) {
	m.calls++
	if m.calls == 2 {
		return "", errors.New("boom")
	}
	return "rewritten", nil
}

// P0.16: rewriting middleware run once per field; accounting middleware run
// once per response over non-derived text; a fail-open middleware that
// errors partway applies none of its rewrites.
func TestRunResponseFields(t *testing.T) {
	upper, acct, fail := &upperMW{}, &accountingMW{}, &failSecondMW{}
	p := &Pipeline{Entries: []Entry{{MW: fail, FailOpen: true}, {MW: upper}, {MW: acct, FailOpen: true}}}
	in := []ResponseField{{Text: "ab"}, {Text: `{"k":"cd"}`}, {Text: "cd", Derived: true}}

	out, err := p.RunResponse(context.Background(), in, NewGatewayContext("", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{out[0].Text, out[1].Text, out[2].Text}; strings.Join(got, "|") != `AB|{"K":"CD"}|CD` {
		t.Errorf("fields %q", got)
	}
	if in[0].Text != "ab" {
		t.Errorf("input modified: %q", in[0].Text)
	}
	if upper.calls != 3 {
		t.Errorf("rewriting middleware called %d times, want 3", upper.calls)
	}
	if len(acct.texts) != 1 || acct.texts[0] != `AB{"K":"CD"}` {
		t.Errorf("accounting middleware saw %q, want one call over non-derived text", acct.texts)
	}
}
