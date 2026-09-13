package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

const sessionRealIP = "10.0.0.5"

// sessionGateway runs context_pseudonymizer with the given settings lines
// (indented under settings:) and top-level YAML appended at the end.
func sessionGateway(t *testing.T, fake *testutil.FakeUpstream, settings string, persist bool, topLevel string) *testutil.Gateway {
	t.Helper()
	return testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
%s
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
middleware: [context_pseudonymizer]
middleware_config:
  context_pseudonymizer:
    persist_sessions_for_unauthenticated: %t
%s
`, fake.URL, settings, persist, topLevel))
}

func nonStreamBody(prompt string) string {
	content, _ := json.Marshal(prompt)
	return fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":%s}]}`, content)
}

func chatContent(t *testing.T, res testutil.Result) string {
	t.Helper()
	if res.Status != http.StatusOK {
		t.Fatalf("status %d, body %q", res.Status, res.Body)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(res.Body), &out); err != nil || len(out.Choices) == 0 {
		t.Fatalf("not a chat completion: %v; body %q", err, res.Body)
	}
	return out.Choices[0].Message.Content
}

// P0.10 end to end: in shared-auth_key and open modes, a client that sends
// another client's x-client-id must not get that client's real values
// reversed into its response.
func TestChatPseudonymSessionNotSharedBySpoofedClientID(t *testing.T) {
	const prompt = "db at " + sessionRealIP
	fake := fakeFor(t, prompt, sessionRealIP)

	cases := []struct {
		name     string
		settings string
		auth     []string
	}{
		{"shared auth_key", `  auth_key: "shared-key"`, []string{"Authorization", "Bearer shared-key"}},
		{"open auth", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := testutil.NewFakeUpstream(t, testutil.OpenAIChat("ok"), testutil.OpenAIChat("the ip is "+fake))
			gw := sessionGateway(t, upstream, tc.settings, false, "")

			chatContent(t, gw.PostChat(t, nonStreamBody(prompt), append(tc.auth, "x-client-id", "victim")...))
			got := chatContent(t, gw.PostChat(t, nonStreamBody("repeat back what I said"), append(tc.auth, "x-client-id", "victim")...))
			if strings.Contains(got, sessionRealIP) {
				t.Errorf("spoofed x-client-id received the victim's real value: %q", got)
			}
			if got != "the ip is "+fake {
				t.Errorf("content %q, want the fake left as-is", got)
			}
		})
	}
}

// P0.10: a users-table identity keeps its session across requests; other
// users, including one claiming that user's x-client-id, don't see it.
func TestChatPseudonymSessionPersistsForUsersTableIdentity(t *testing.T) {
	const prompt = "db at " + sessionRealIP
	fake := fakeFor(t, prompt, sessionRealIP)
	upstream := testutil.NewFakeUpstream(t, testutil.OpenAIChat("ok"), testutil.OpenAIChat("the ip is "+fake))
	gw := sessionGateway(t, upstream, "", false, `users:
  alice:
    gateway_key: "alice-key"
  bob:
    gateway_key: "bob-key"
`)

	chatContent(t, gw.PostChat(t, nonStreamBody(prompt), "Authorization", "Bearer alice-key"))
	if got := chatContent(t, gw.PostChat(t, nonStreamBody("repeat"), "Authorization", "Bearer alice-key")); got != "the ip is "+sessionRealIP {
		t.Errorf("alice's follow-up turn: content %q, want the real value restored", got)
	}
	if got := chatContent(t, gw.PostChat(t, nonStreamBody("repeat"), "Authorization", "Bearer bob-key", "x-client-id", "alice")); got != "the ip is "+fake {
		t.Errorf("bob received alice's real value: %q", got)
	}
}

// P0.10: the documented opt-in restores per-x-client-id persistence.
func TestChatPseudonymSessionOptInPersistsUnauthenticated(t *testing.T) {
	const prompt = "db at " + sessionRealIP
	fake := fakeFor(t, prompt, sessionRealIP)
	upstream := testutil.NewFakeUpstream(t, testutil.OpenAIChat("ok"), testutil.OpenAIChat("the ip is "+fake))
	gw := sessionGateway(t, upstream, `  auth_key: "shared-key"`, true, "")

	auth := []string{"Authorization", "Bearer shared-key", "x-client-id", "laptop"}
	chatContent(t, gw.PostChat(t, nonStreamBody(prompt), auth...))
	if got := chatContent(t, gw.PostChat(t, nonStreamBody("repeat"), auth...)); got != "the ip is "+sessionRealIP {
		t.Errorf("opted-in session not persisted: content %q", got)
	}
}
