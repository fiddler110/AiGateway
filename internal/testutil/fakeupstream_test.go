package testutil

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	return http.Post(url, "application/json", strings.NewReader(`{"model":"m"}`))
}

func TestFakeUpstreamScriptOrderAndRecording(t *testing.T) {
	f := NewFakeUpstream(t, OpenAIError(http.StatusTooManyRequests, "slow down", "rate_limit_error"), OpenAIChat("hi"))

	want := []int{http.StatusTooManyRequests, http.StatusOK, http.StatusOK} // last response repeats
	for i, status := range want {
		resp, err := post(t, f.URL+"/v1/chat/completions")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("request %d: status %d, want %d", i, resp.StatusCode, status)
		}
	}

	reqs := f.Requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(reqs))
	}
	if reqs[0].Path != "/v1/chat/completions" || string(reqs[0].Body) != `{"model":"m"}` {
		t.Errorf("recorded request = %s %q", reqs[0].Path, reqs[0].Body)
	}
}

func TestFakeUpstreamEmptyScriptReturns500(t *testing.T) {
	f := NewFakeUpstream(t)
	resp, err := post(t, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
}

func TestFakeUpstreamDelays(t *testing.T) {
	const delay = 50 * time.Millisecond
	f := NewFakeUpstream(t, Response{HeaderDelay: delay, Body: "x", BodyDelay: delay})

	start := time.Now()
	resp, err := post(t, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := time.Since(start); got < delay {
		t.Errorf("headers after %v, want >= %v", got, delay)
	}
	start = time.Now()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "x" {
		t.Fatalf("body %q, err %v", body, err)
	}
	if got := time.Since(start); got < delay {
		t.Errorf("body after %v, want >= %v", got, delay)
	}
}

func TestFakeUpstreamDisconnectTruncatesStream(t *testing.T) {
	sse := OpenAISSE("hello", " world")
	sse.Chunks = sse.Chunks[:1]
	sse.Disconnect = true
	f := NewFakeUpstream(t, sse)

	resp, err := post(t, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("read succeeded with %q, want an error from the aborted connection", body)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("chunks before the disconnect should arrive, got %q", body)
	}
}
