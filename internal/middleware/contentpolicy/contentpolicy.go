// Package contentpolicy blocks requests/responses matching a configured
// regex deny-list. Fails closed by default: a bug here must hard-fail the
// request rather than silently skip the policy check.
package contentpolicy

import (
	"context"
	"fmt"
	"regexp"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type Middleware struct {
	patterns []*regexp.Regexp
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{}
	raw, ok := cfg["deny_patterns"]
	if !ok {
		return m, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("content_policy: deny_patterns must be a list of strings")
	}
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("content_policy: deny_patterns entries must be strings")
		}
		re, err := regexp.Compile("(?i)" + s)
		if err != nil {
			return nil, fmt.Errorf("content_policy: invalid deny_pattern %q: %w", s, err)
		}
		m.patterns = append(m.patterns, re)
	}
	return m, nil
}

func (m *Middleware) Name() string { return "content_policy" }

// Process scans each message's plain text (not tool-call args, matching the
// reference — content_policy is aimed at conversational content) for any
// deny pattern. On a match, the request is blocked with a GENERIC reason;
// the actual matched pattern is stashed separately so it never leaks into
// the block reason surfaced to the client or into logs.
func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	for _, msg := range req.Messages {
		text := chatmodel.MessageText(msg.Content)
		for _, re := range m.patterns {
			if re.MatchString(text) {
				gctx.Blocked = true
				gctx.BlockReason = "content_policy: request blocked by policy"
				gctx.Scratch.ContentPolicyMatch = re.String()
				return nil
			}
		}
	}
	return nil
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	for _, re := range m.patterns {
		if re.MatchString(text) {
			gctx.Blocked = true
			gctx.BlockReason = "content_policy: response blocked by policy"
			gctx.Scratch.ContentPolicyMatch = re.String()
			return text, nil
		}
	}
	return text, nil
}
