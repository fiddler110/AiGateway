package handlers_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/app"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/handlers"
	"github.com/scottymacleod/aigateway/internal/httpclient"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/server"
	"github.com/scottymacleod/aigateway/internal/testutil"
)

// renameTranslator is a test-only stream translator that rewrites the
// delta text "hel" to "translated".
type renameTranslator struct{ *openai.Translator }

func (renameTranslator) NewStreamTranslator() provider.StreamTranslator { return renameStream{} }

type renameStream struct{}

func (renameStream) Feed(line []byte) ([][]byte, error) {
	return [][]byte{bytes.ReplaceAll(line, []byte(`"hel"`), []byte(`"translated"`))}, nil
}

func (renameStream) Done() ([][]byte, provider.Usage, error) { return nil, provider.Usage{}, nil }

func gatewayYAML(fakeURL string, buffered bool) string {
	return fmt.Sprintf(`
upstreams:
  fake:
    base_url: %q
    auth_type: none
settings:
  default_upstream: fake
  stream_buffer: %t
resilience:
  retry_attempts: 0
  health_check:
    enabled: false
`, fakeURL, buffered)
}

// P0.13: the stream translator registered for the upstream's api_format
// shapes what the client receives, in both passthrough and buffered mode.
func TestChatStreamUsesProviderStreamTranslator(t *testing.T) {
	registry := provider.Registry{"openai": func() provider.Translator { return renameTranslator{openai.New()} }}
	for _, buffered := range []bool{false, true} {
		t.Run(fmt.Sprintf("buffered=%t", buffered), func(t *testing.T) {
			fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("hel", "lo"))
			gw := testutil.NewGatewayWithRegistry(t, gatewayYAML(fake.URL, buffered), registry)

			res := gw.PostChat(t, chatBody(true))
			if res.Status != http.StatusOK {
				t.Fatalf("status %d body %q", res.Status, res.Body)
			}
			if cs := readStream(t, res.Body); cs.content != "translatedlo" {
				t.Errorf("client content %q, want %q (body %q)", cs.content, "translatedlo", res.Body)
			}
		})
	}
}

// plainWriter is a ResponseWriter that cannot flush.
type plainWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (p *plainWriter) Header() http.Header         { return p.header }
func (p *plainWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (p *plainWriter) WriteHeader(code int) {
	if p.code == 0 {
		p.code = code
	}
}

// The chat handler wraps its ResponseWriter; the wrapper must implement
// http.Flusher only when the real writer does, or serveStream's "streaming
// not supported" check passes for a writer that cannot stream.
func TestChatStreamRequiresFlusher(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.OpenAISSE("hel", "lo"))
	cfg, err := config.Parse([]byte(gatewayYAML(fake.URL, false)))
	if err != nil {
		t.Fatal(err)
	}
	client := httpclient.New()
	srv := server.New(client)
	st, err := app.BuildState(cfg, client, app.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Pipeline.Close() })
	srv.Swap(st)

	w := &plainWriter{header: http.Header{}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(true)))
	r.Header.Set("Content-Type", "application/json")
	handlers.Chat(srv)(w, r)

	if w.code != http.StatusInternalServerError || !strings.Contains(w.body.String(), "streaming not supported") {
		t.Errorf("status %d body %q, want 500 streaming not supported", w.code, w.body.String())
	}
	if n := fake.RequestCount(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}

	// A flushing writer still streams through the wrapper.
	rec := httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(true)))
	handlers.Chat(srv)(rec, r)
	if rec.Code != http.StatusOK || !rec.Flushed {
		t.Errorf("flushing writer: status %d flushed %t", rec.Code, rec.Flushed)
	}
}
