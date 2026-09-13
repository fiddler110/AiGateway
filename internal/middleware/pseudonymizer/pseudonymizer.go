// Package pseudonymizer implements AiGateway's flagship reversible-DLP
// feature: sensitive infrastructure values (private IPs, hostnames,
// passwords, credential paths) are replaced with structurally similar fakes
// before a request leaves the gateway, then reversed on the response so the
// caller sees the real values — the model reasons over consistent fake
// data without ever seeing the real secrets. Fails closed by default.
package pseudonymizer

import (
	"context"
	"sort"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type Middleware struct {
	internalDomains  []string
	ignoreDomains    []string
	sensitiveStrings []string
	usernames        []string
	sessions         *sessionStore
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{sessions: newSessionStore()}
	m.internalDomains = stringList(cfg, "internal_domains")
	m.ignoreDomains = stringList(cfg, "ignore_domains")
	m.sensitiveStrings = stringList(cfg, "sensitive_strings")
	m.usernames = stringList(cfg, "usernames")
	return m, nil
}

func stringList(cfg map[string]any, key string) []string {
	raw, ok := cfg[key]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (m *Middleware) Name() string { return "context_pseudonymizer" }

// fakeFor generates the deterministic fake replacement for one finding,
// given the client's existing fake assignments (for IPv4/CIDR collision
// avoidance).
func fakeFor(f finding, existingFakes map[string]string) string {
	switch f.kind {
	case kindIPv4:
		return fakeIPv4(f.value, existingFakes)
	case kindIPv6:
		return fakeIPv6(f.value)
	case kindCIDR:
		return fakeCIDR(f.value, f.extra, existingFakes)
	case kindPassword:
		return fakePassword(f.value)
	case kindConnString:
		if f.extra == "user" {
			return fakeUsername(f.value)
		}
		return fakePassword(f.value)
	case kindUnixPath, kindWindowsPath:
		return fakeUsername(f.value)
	case kindHostname:
		return fakeHostname(f.value)
	case kindUsername:
		return fakeUsername(f.value)
	case kindSensitiveString:
		return fakeSensitiveString(f.value)
	default:
		return f.value
	}
}

// buildSubstituter returns a function that replaces every key in mapping
// within a string, longest key first, so a shorter value is never replaced
// as a partial substring of a longer one (e.g. "admin" vs
// "admin@example.com").
func buildSubstituter(mapping map[string]string) func(string) string {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	return func(s string) string {
		for _, k := range keys {
			if v := mapping[k]; v != "" {
				s = strings.ReplaceAll(s, k, v)
			}
		}
		return s
	}
}

// Process finds new sensitive values across all messages, assigns them
// deterministic fakes (reusing the client's existing session assignments
// for values seen in prior turns), merges everything into the client's full
// session map, and substitutes content + tool-call arguments accordingly.
// The FULL merged map (not just this request's new findings) is what makes
// values pseudonymized in earlier turns still reverse correctly even if
// they don't reappear in this request.
func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	forward, reverse := m.sessions.getOrCreate(gctx.ClientID)

	newCount := 0
	for i := range req.Messages {
		text := chatmodel.MessageScanText(req.Messages[i])
		for _, f := range detect(text, m.internalDomains, m.ignoreDomains, m.sensitiveStrings, m.usernames) {
			if _, exists := forward[f.value]; exists {
				continue
			}
			fake := fakeFor(f, reverse)
			forward[f.value] = fake
			reverse[fake] = f.value
			newCount++
		}
	}

	if newCount > 0 {
		m.sessions.merge(gctx.ClientID, forward, reverse)
	}

	substitute := buildSubstituter(forward)
	for i := range req.Messages {
		chatmodel.MapMessageScanText(&req.Messages[i], substitute)
	}

	gctx.Scratch.PseudonymMap = forward
	gctx.Scratch.ReverseMap = reverse
	gctx.Scratch.PseudonymizedCount = len(forward)
	return nil
}

// ProcessResponse reverses fake->real using the map built during Process
// (or loaded fresh from the session store on a cache-hit path where Process
// didn't run for this exact request but the client has a prior session).
func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	reverse := gctx.Scratch.ReverseMap
	if reverse == nil {
		_, reverse = m.sessions.getOrCreate(gctx.ClientID)
	}
	if len(reverse) == 0 {
		return text, nil
	}
	restore := buildSubstituter(reverse)
	return restore(text), nil
}
