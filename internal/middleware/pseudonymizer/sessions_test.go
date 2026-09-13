package pseudonymizer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/scottymacleod/aigateway/internal/pipeline"
)

func authed(clientID string) *pipeline.GatewayContext {
	gctx := pipeline.NewGatewayContext(clientID, "up", "")
	gctx.Authenticated = true
	return gctx
}

func unauthed(clientID string) *pipeline.GatewayContext {
	return pipeline.NewGatewayContext(clientID, "up", "")
}

// issue runs Process for gctx over prompt and returns the fake given to real.
func issue(t *testing.T, m *Middleware, gctx *pipeline.GatewayContext, prompt, real string) string {
	t.Helper()
	if err := m.Process(context.Background(), newReq(prompt), gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	fake := gctx.Scratch.PseudonymMap[real]
	if fake == "" {
		t.Fatalf("%q was not pseudonymized", real)
	}
	return fake
}

// reverseIn runs a request with no sensitive values for gctx, then reverses
// text through it, as a follow-up turn would.
func reverseIn(t *testing.T, m *Middleware, gctx *pipeline.GatewayContext, text string) string {
	t.Helper()
	if err := m.Process(context.Background(), newReq("repeat back what I said"), gctx); err != nil {
		t.Fatalf("Process: %v", err)
	}
	got, err := m.ProcessResponse(context.Background(), text, gctx)
	if err != nil {
		t.Fatalf("ProcessResponse: %v", err)
	}
	return got
}

func newMiddleware(t *testing.T, cfg map[string]any) *Middleware {
	t.Helper()
	mw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mw.(*Middleware)
}

// P0.10: without a users-table identity, a second request that claims the
// same client ID must not get the first request's fakes reversed into real
// values, whether ProcessResponse uses its own request map or falls back to
// the session store.
func TestUnauthenticatedSessionsArePerRequest(t *testing.T) {
	m := newMiddleware(t, nil)
	fake := issue(t, m, unauthed("victim"), "db at 10.0.0.5", "10.0.0.5")

	if got := reverseIn(t, m, unauthed("victim"), "ip "+fake); got != "ip "+fake {
		t.Errorf("spoofed client ID got another request's real value: %q", got)
	}
	got, err := m.ProcessResponse(context.Background(), "ip "+fake, unauthed("victim"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "ip "+fake {
		t.Errorf("session-store fallback restored another request's real value: %q", got)
	}
}

// P0.10: users-table identities keep their session across requests, and it
// is visible only to that identity, never to another user or to an
// unauthenticated client presenting the same client ID.
func TestAuthenticatedSessionsPersistPerIdentity(t *testing.T) {
	m := newMiddleware(t, nil)
	fake := issue(t, m, authed("alice"), "db at 10.0.0.5", "10.0.0.5")

	if got := reverseIn(t, m, authed("alice"), "ip "+fake); got != "ip 10.0.0.5" {
		t.Errorf("same user's follow-up turn not reversed: %q", got)
	}
	if got := reverseIn(t, m, authed("bob"), "ip "+fake); got != "ip "+fake {
		t.Errorf("another user got alice's real value: %q", got)
	}
	if got := reverseIn(t, m, unauthed("alice"), "ip "+fake); got != "ip "+fake {
		t.Errorf("unauthenticated client ID %q got alice's real value: %q", "alice", got)
	}
}

// P0.10: the operator opt-in persists unauthenticated sessions per client ID.
func TestPersistSessionsForUnauthenticatedOptIn(t *testing.T) {
	m := newMiddleware(t, map[string]any{"persist_sessions_for_unauthenticated": true})
	fake := issue(t, m, unauthed("shared"), "db at 10.0.0.5", "10.0.0.5")

	if got := reverseIn(t, m, unauthed("shared"), "ip "+fake); got != "ip 10.0.0.5" {
		t.Errorf("opted-in session not persisted: %q", got)
	}
	if got := reverseIn(t, m, unauthed("other"), "ip "+fake); got != "ip "+fake {
		t.Errorf("different client ID got the session's real value: %q", got)
	}
}

func TestNewRejectsNonBoolPersistSessions(t *testing.T) {
	if _, err := New(map[string]any{"persist_sessions_for_unauthenticated": "yes"}); err == nil {
		t.Error("New accepted a non-boolean persist_sessions_for_unauthenticated")
	}
}

// P0.11: concurrent requests for one identity, each with a distinct IP, must
// leave a complete, bijective session map: no request's mapping is lost, and
// no fake is issued to two real values. Meaningful without -race: a lost
// update shows up as an unreversed fake, a collision as a duplicate fake or a
// wrong reversal. Repeated over several rounds to make the interleaving likely.
func TestConcurrentRequestsAssignBijectiveMap(t *testing.T) {
	const rounds, n = 20, 64
	for round := range rounds {
		m := newMiddleware(t, nil)
		ips := make([]string, n)
		for i := range ips {
			ips[i] = fmt.Sprintf("10.%d.%d.%d", round+1, i/200, i%200+1)
		}
		fakes := make([]string, n)
		errs := make([]error, n)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				gctx := authed("alice")
				errs[i] = m.Process(context.Background(), newReq("host "+ips[i]+" is down"), gctx)
				fakes[i] = gctx.Scratch.PseudonymMap[ips[i]]
			}()
		}
		close(start)
		wg.Wait()

		owner := map[string]string{}
		for i, fake := range fakes {
			if errs[i] != nil {
				t.Fatalf("round %d: Process: %v", round, errs[i])
			}
			if fake == "" {
				t.Fatalf("round %d: %s was not pseudonymized", round, ips[i])
			}
			if prev, dup := owner[fake]; dup {
				t.Errorf("round %d: fake %s issued to both %s and %s", round, fake, prev, ips[i])
			}
			owner[fake] = ips[i]
		}

		// A later request reverses through the stored session only.
		got, err := m.ProcessResponse(context.Background(), strings.Join(fakes, "\n"), authed("alice"))
		if err != nil {
			t.Fatal(err)
		}
		gotLines := strings.Split(got, "\n")
		for i, line := range gotLines {
			if line != ips[i] {
				t.Errorf("round %d: fake %s reversed to %q, want %s (mapping lost or collided)", round, fakes[i], line, ips[i])
			}
		}
	}
}
