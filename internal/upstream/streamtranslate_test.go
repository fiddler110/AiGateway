package upstream_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/testutil"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

// shoutTranslator is a test-only non-identity translator: it upper-cases
// every "data:" line, swallows blank lines and "event:" lines, and appends a
// marker line at Done. It embeds openai for the request side.
type shoutTranslator struct{ *openai.Translator }

func (shoutTranslator) NewStreamTranslator() provider.StreamTranslator { return &shoutStream{} }

type shoutStream struct{ fed []string }

func (s *shoutStream) Feed(line []byte) ([][]byte, error) {
	s.fed = append(s.fed, string(line))
	if bytes.HasPrefix(line, []byte("boom")) {
		return nil, errors.New("bad native event")
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, nil
	}
	return [][]byte{bytes.ToUpper(line), {}}, nil
}

func (s *shoutStream) Done() ([][]byte, provider.Usage, error) {
	return [][]byte{[]byte("data: END"), {}}, provider.Usage{}, nil
}

func shoutRegistry() provider.Registry {
	return provider.Registry{"openai": func() provider.Translator { return shoutTranslator{openai.New()} }}
}

func managerWith(t *testing.T, up config.UpstreamConfig, registry provider.Registry, maxBytes int64) *upstream.Manager {
	t.Helper()
	cfg, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resilience.RetryAttempts = 0
	cfg.Resilience.HealthCheck.Enabled = false
	if maxBytes > 0 {
		cfg.Settings.MaxResponseBytes = maxBytes
	}
	cfg.Upstreams = map[string]config.UpstreamConfig{"up": up}
	return upstream.NewManager(cfg, &http.Client{}, registry)
}

// P0.13: the translator registered for the upstream's api_format is applied
// to the stream body SendStream returns.
func TestSendStreamAppliesStreamTranslator(t *testing.T) {
	fake := testutil.NewFakeUpstream(t, testutil.Response{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/event-stream"}},
		Chunks: []testutil.Chunk{{Data: "event: x\r\ndata: {\"a\":1}\r\n\r\n"}, {Data: "data: [done]\n\n"}},
	})
	m := managerWith(t, config.UpstreamConfig{BaseURL: fake.URL, AuthType: "none"}, shoutRegistry(), 0)

	res, err := m.SendStream(context.Background(), []string{"up"}, chatRequest(), noKey)
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "DATA: {\"A\":1}\n\nDATA: [DONE]\n\ndata: END\n\n"
	if string(got) != want {
		t.Errorf("stream %q, want %q", got, want)
	}
}

// P0.13: an api_format with no registered translator is an error, never a
// silent fallback to openai, and nothing is sent upstream.
func TestUnknownAPIFormatIsError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		fake := testutil.NewFakeUpstream(t, testutil.OpenAIChat("hi"))
		m := managerWith(t, config.UpstreamConfig{BaseURL: fake.URL, AuthType: "none", APIFormat: "bogus"},
			provider.Registry{"openai": func() provider.Translator { return openai.New() }}, 0)
		var err error
		if stream {
			var res upstream.StreamResult
			res, err = m.SendStream(context.Background(), []string{"up"}, chatRequest(), noKey)
			if res.Body != nil {
				res.Body.Close()
			}
		} else {
			_, err = m.Send(context.Background(), []string{"up"}, chatRequest(), noKey)
		}
		if err == nil || !strings.Contains(err.Error(), `api_format "bogus"`) {
			t.Errorf("stream=%t: err = %v, want an unknown api_format error", stream, err)
		}
		if n := fake.RequestCount(); n != 0 {
			t.Errorf("stream=%t: upstream received %d requests, want 0", stream, n)
		}
	}
}

// pipeBody is an upstream body the test writes to line by line.
type pipeBody struct {
	*io.PipeReader
}

// P0.13: translation happens per line as it arrives. A complete line is
// readable before the next line exists, so passthrough latency is unchanged.
func TestTranslatedStreamIsLineByLine(t *testing.T) {
	pr, pw := io.Pipe()
	body := upstream.NewTranslatedStreamForTest(pipeBody{pr}, shoutStreamFor())
	defer body.Close()
	defer pw.Close()

	go func() { _, _ = pw.Write([]byte("data: one\ndata: tw")) }()
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(body)
		if sc.Scan() {
			lines <- sc.Text()
		}
	}()
	select {
	case got := <-lines:
		if got != "DATA: ONE" {
			t.Errorf("first line %q, want DATA: ONE", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first line not delivered before the rest of the stream arrived")
	}
}

func shoutStreamFor() provider.StreamTranslator {
	return shoutTranslator{openai.New()}.NewStreamTranslator()
}

// P0.13: a failed body read keeps its error (so the P0.9 limit is still
// recognised), delivers lines completed before it, never feeds the partial
// line, and skips Done. Translation errors and oversized lines are errors.
func TestTranslatedStreamErrors(t *testing.T) {
	t.Run("response limit", func(t *testing.T) {
		fake := testutil.NewFakeUpstream(t, testutil.Response{
			Status: http.StatusOK,
			Chunks: []testutil.Chunk{{Data: "data: aaaa\ndata: " + strings.Repeat("b", 100) + "\n"}},
		})
		m := managerWith(t, config.UpstreamConfig{BaseURL: fake.URL, AuthType: "none"}, shoutRegistry(), 20)
		res, err := m.SendStream(context.Background(), []string{"up"}, chatRequest(), noKey)
		if err != nil {
			t.Fatalf("SendStream: %v", err)
		}
		defer res.Body.Close()
		got, err := io.ReadAll(res.Body)
		if !errors.Is(err, upstream.ErrResponseTooLarge) {
			t.Errorf("err = %v, want ErrResponseTooLarge", err)
		}
		if string(got) != "DATA: AAAA\n\n" {
			t.Errorf("delivered %q, want only the complete first line", got)
		}
	})
	t.Run("translate error", func(t *testing.T) {
		body := upstream.NewTranslatedStreamForTest(io.NopCloser(strings.NewReader("boom\n")), shoutStreamFor())
		if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "translate stream") {
			t.Errorf("err = %v, want a translate error", err)
		}
	})
	t.Run("line too long", func(t *testing.T) {
		body := upstream.NewTranslatedStreamForTest(io.NopCloser(strings.NewReader(strings.Repeat("x", 5*1024*1024))), shoutStreamFor())
		if _, err := io.ReadAll(body); !errors.Is(err, bufio.ErrTooLong) {
			t.Errorf("err = %v, want bufio.ErrTooLong", err)
		}
	})
	t.Run("unterminated final line on clean EOF", func(t *testing.T) {
		body := upstream.NewTranslatedStreamForTest(io.NopCloser(strings.NewReader("data: x")), shoutStreamFor())
		got, err := io.ReadAll(body)
		if err != nil || string(got) != "DATA: X\n\ndata: END\n\n" {
			t.Errorf("got %q, %v", got, err)
		}
	})
}
