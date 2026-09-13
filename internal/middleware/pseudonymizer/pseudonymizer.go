// Package pseudonymizer implements AiGateway's flagship reversible-DLP
// feature: sensitive infrastructure values (private IPs, hostnames,
// passwords, credential paths) are replaced with structurally similar fakes
// before a request leaves the gateway, then reversed on the response so the
// caller sees the real values — the model reasons over consistent fake
// data without ever seeing the real secrets. Fails closed by default.
package pseudonymizer

import (
	"context"
	"errors"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
	"github.com/scottymacleod/aigateway/internal/pseudomap"
)

type Middleware struct {
	internalDomains  []string
	ignoreDomains    []string
	sensitiveStrings []string
	usernames        []string
	sessions         *sessionStore

	// persistUnauthenticated opts in to keeping sessions across requests
	// for unauthenticated (shared auth_key / open) clients, keyed by the
	// spoofable x-client-id. Off by default; see P0.10.
	persistUnauthenticated bool
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{sessions: newSessionStore()}
	m.internalDomains = stringList(cfg, "internal_domains")
	m.ignoreDomains = stringList(cfg, "ignore_domains")
	m.sensitiveStrings = stringList(cfg, "sensitive_strings")
	m.usernames = stringList(cfg, "usernames")
	if raw, ok := cfg["persist_sessions_for_unauthenticated"]; ok {
		b, isBool := raw.(bool)
		if !isBool {
			return nil, errors.New("persist_sessions_for_unauthenticated must be a boolean")
		}
		m.persistUnauthenticated = b
	}
	return m, nil
}

// sessionFor returns the persistent session for gctx's identity, or nil when
// the request has no persistent session: either the identity isn't
// authenticated and persistence isn't opted in, or create is false and none
// exists yet. Keys are namespaced so an x-client-id can never name a
// users-table identity's session.
func (m *Middleware) sessionFor(gctx *pipeline.GatewayContext, create bool) *session {
	switch {
	case gctx.Authenticated:
		return m.sessions.get("user:"+gctx.ClientID, create)
	case m.persistUnauthenticated:
		return m.sessions.get("client:"+gctx.ClientID, create)
	default:
		return nil
	}
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

// fakeFor generates the deterministic fake candidate for one finding at the
// given salt. session.assign re-salts on collision, for every kind.
func fakeFor(f finding, salt int) string {
	switch f.kind {
	case kindIPv4:
		return fakeIPv4Salted(f.value, salt)
	case kindIPv6:
		return fakeIPv6(f.value, salt)
	case kindCIDR:
		return fakeCIDR(f.value, f.extra, salt)
	case kindPassword:
		return fakePassword(f.value, salt)
	case kindConnString:
		if f.extra == "user" {
			return fakeUsername(f.value, salt)
		}
		return fakePassword(f.value, salt)
	case kindUnixPath, kindWindowsPath:
		return fakeUsername(f.value, salt)
	case kindHostname:
		return fakeHostname(f.value, salt)
	case kindUsername:
		return fakeUsername(f.value, salt)
	case kindSensitiveString:
		return fakeSensitiveString(f.value, salt)
	default:
		return f.value
	}
}

// buildSubstituter returns a function that replaces every key in mapping
// within a string in one leftmost-longest pass (pseudomap.Matcher): the
// longest key at a position wins, so a shorter value is never replaced as a
// partial substring of a longer one (e.g. "admin" vs "admin@example.com"),
// and a value just written in is never rescanned, so a fake containing a
// real value, or a restored real value containing a fake, isn't substituted
// a second time (P0.14). Entries with an empty value are skipped.
func buildSubstituter(mapping map[string]string) func(string) string {
	nonEmpty := make(map[string]string, len(mapping))
	for k, v := range mapping {
		if v != "" {
			nonEmpty[k] = v
		}
	}
	return pseudomap.New(nonEmpty).ReplaceAll
}

// Process finds sensitive values across all messages and assigns each a
// deterministic fake in the request's session (reusing assignments from
// prior turns), then substitutes content + tool-call arguments using the
// session's full map. The FULL map (not just this request's findings) is
// what makes values pseudonymized in earlier turns still reverse correctly
// even if they don't reappear in this request.
//
// Only authenticated identities get a persistent session (or unauthenticated
// clients, when the operator opts in); otherwise the session lives for this
// request only, so a spoofed x-client-id can't reverse another client's
// fakes into real values (P0.10).
func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	sess := m.sessionFor(gctx, true)
	if sess == nil {
		sess = newSession()
	}

	for i := range req.Messages {
		text := chatmodel.MessageScanText(req.Messages[i])
		for _, f := range detect(text, m.internalDomains, m.ignoreDomains, m.sensitiveStrings, m.usernames) {
			if _, err := sess.assign(f.value, func(salt int) string { return fakeFor(f, salt) }); err != nil {
				return err
			}
		}
	}

	forward, reverse := sess.snapshot()
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
// (or loaded from the identity's persistent session on a path where Process
// didn't run for this exact request). Only fakes issued to this request's
// own session are restored: without a persistent session there is nothing
// to load.
func (m *Middleware) ProcessResponse(_ context.Context, text string, gctx *pipeline.GatewayContext) (string, error) {
	reverse := gctx.Scratch.ReverseMap
	if reverse == nil {
		if sess := m.sessionFor(gctx, false); sess != nil {
			_, reverse = sess.snapshot()
		}
	}
	if len(reverse) == 0 {
		return text, nil
	}
	restore := buildSubstituter(reverse)
	return restore(text), nil
}
