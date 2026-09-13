package testutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/app"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/httpclient"
	"github.com/scottymacleod/aigateway/internal/server"
)

// Gateway is the real gateway router served by httptest, built from an
// inline YAML config.
type Gateway struct {
	*httptest.Server
	Srv *server.Server
}

// NewGateway parses and validates yamlConfig exactly as config.Load would,
// builds an AppState, and serves app.NewRouter. Callers interpolate fake
// upstream URLs into the YAML (e.g. with fmt.Sprintf and FakeUpstream.URL).
// The server is closed on test cleanup.
func NewGateway(t testing.TB, yamlConfig string) *Gateway {
	t.Helper()
	cfg, err := config.Parse([]byte(yamlConfig))
	if err != nil {
		t.Fatalf("parse gateway config: %v", err)
	}
	client := httpclient.New()
	srv := server.New(client)
	st, err := app.BuildState(cfg, client, app.NewRegistry())
	if err != nil {
		t.Fatalf("build gateway state: %v", err)
	}
	srv.Swap(st)

	ts := httptest.NewServer(app.NewRouter(srv))
	t.Cleanup(func() {
		ts.Close()
		client.CloseIdleConnections()
	})
	return &Gateway{Server: ts, Srv: srv}
}

// Result is a fully read gateway response.
type Result struct {
	Status int
	Header http.Header
	Body   string
}

// PostChat sends body to /v1/chat/completions with the given headers
// (alternating name, value) and reads the whole response.
func (g *Gateway) PostChat(t testing.TB, body string, headers ...string) Result {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := g.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
	return Result{Status: resp.StatusCode, Header: resp.Header, Body: string(data)}
}
