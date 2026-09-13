// Package piiredactor irreversibly replaces common PII (email, phone, SSN,
// credit card) with placeholder tokens, both in requests (content and
// tool-call arguments) and responses. Fails closed by default.
package piiredactor

import (
	"context"
	"fmt"
	"regexp"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type patternReplacement struct {
	re   *regexp.Regexp
	repl string
}

// builtinPatterns are applied in order; each pattern uses fixed-width digit
// groups (not unbounded quantifiers) to keep matching linear-time even
// though Go's RE2 engine is already immune to catastrophic backtracking.
var builtinPatterns = []patternReplacement{
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[EMAIL]"},
	{regexp.MustCompile(`(?:\+1[\s.-]?)?\(?\d{3}\)?[\s.-]?\d{3}[\s.-]?\d{4}\b`), "[PHONE]"},
	{regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), "[SSN]"},
	// 16-digit card: 4-4-4-4 with optional space/dash separators.
	{regexp.MustCompile(`\b\d{4}[ -]?\d{4}[ -]?\d{4}[ -]?\d{4}\b`), "[CARD]"},
	// 15-digit Amex: 4-6-5.
	{regexp.MustCompile(`\b\d{4}[ -]?\d{6}[ -]?\d{5}\b`), "[CARD]"},
}

type Middleware struct {
	patterns []patternReplacement
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{patterns: append([]patternReplacement{}, builtinPatterns...)}
	raw, ok := cfg["extra_patterns"]
	if !ok {
		return m, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("pii_redactor: extra_patterns must be a list")
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pii_redactor: extra_patterns entries must be objects with regex/replacement")
		}
		pattern, _ := entry["regex"].(string)
		replacement, _ := entry["replacement"].(string)
		if pattern == "" {
			return nil, fmt.Errorf("pii_redactor: extra_patterns entry missing regex")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("pii_redactor: invalid extra_patterns regex %q: %w", pattern, err)
		}
		m.patterns = append(m.patterns, patternReplacement{re: re, repl: replacement})
	}
	return m, nil
}

func (m *Middleware) Name() string { return "pii_redactor" }

func (m *Middleware) redact(s string) string {
	for _, p := range m.patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// Process applies redaction to each message's content AND tool-call
// arguments (via MapMessageScanText) — without the tool-call portion,
// agentic tool arguments carrying PII would bypass this middleware
// entirely. Redaction is irreversible, unlike the pseudonymizer.
func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	redactedCount := 0
	for i := range req.Messages {
		before := chatmodel.MessageScanText(req.Messages[i])
		chatmodel.MapMessageScanText(&req.Messages[i], m.redact)
		if chatmodel.MessageScanText(req.Messages[i]) != before {
			redactedCount++
		}
	}
	gctx.Scratch.PIIRedacted = redactedCount
	return nil
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	newText := m.redact(text)
	if newText != text {
		gctx.Scratch.PIIRedactedResponse = true
	}
	return newText, nil
}
