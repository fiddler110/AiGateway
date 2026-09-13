package handlers_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/testutil"
)

// P0.14: /health stays unauthenticated but discloses nothing; circuit detail
// is only on /health/detail, behind the same auth as /v1.
func TestHealthEndpoints(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAIChat("hi"))
	gw := testutil.NewGateway(t, fmt.Sprintf(`
upstreams:
  secret-upstream-name:
    base_url: %q
    auth_type: none
settings:
  auth_key: "health-test-key"
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
`, fake.URL))

	get := func(path, key string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, gw.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, body := get("/health", ""); status != http.StatusOK || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("/health = %d %q, want 200 {\"status\":\"ok\"}", status, body)
	}
	if status, body := get("/health/detail", ""); status != http.StatusUnauthorized || strings.Contains(body, "secret-upstream-name") {
		t.Errorf("/health/detail without key = %d %q, want 401 without upstream names", status, body)
	}
	if status, _ := get("/health/detail", "wrong-key"); status != http.StatusUnauthorized {
		t.Errorf("/health/detail with wrong key = %d, want 401", status)
	}
	status, body := get("/health/detail", "health-test-key")
	if status != http.StatusOK || !strings.Contains(body, "secret-upstream-name") || !strings.Contains(body, `"circuit`) {
		t.Errorf("/health/detail with key = %d %q, want 200 with upstream circuit state", status, body)
	}
}
