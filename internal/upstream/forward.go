package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scottymacleod/aigateway/internal/config"
)

// buildHeaders sets auth and content-type headers for a request to the given
// upstream. Anthropic-format upstreams always get X-Api-Key +
// anthropic-version regardless of the configured auth_type, matching that
// API's hard requirement.
func buildHeaders(req *http.Request, up config.UpstreamConfig, apiKey string) {
	if up.APIFormat == "anthropic" {
		if apiKey != "" {
			req.Header.Set("X-Api-Key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		switch up.AuthType {
		case "x-api-key":
			if apiKey != "" {
				req.Header.Set("X-Api-Key", apiKey)
			}
		case "none":
			// no auth header
		default: // "bearer" or unset
			if apiKey != "" {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}
		}
	}
	req.Header.Set("Content-Type", "application/json")
}

// ResolveAPIKey reads the actual key material from the process environment
// by name, fresh, per call. Key material is never stored in config, on the
// GatewayContext, or passed to logging.
func ResolveAPIKey(envVar string) string {
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

// chatPath returns up.ChatPath if the caller overrode it, else
// defaultPath (which the active Translator supplies, e.g. "/messages" for
// Anthropic instead of the generic "/chat/completions").
func chatPath(up config.UpstreamConfig, defaultPath string) string {
	if up.ChatPath != "" {
		return up.ChatPath
	}
	return defaultPath
}

// UpstreamRequest is the input to a single forward attempt.
type UpstreamRequest struct {
	Upstream config.UpstreamConfig
	APIKey   string
	Body     []byte
	Path     string // resolved path (already applied chatPath/defaultPath logic)
}

// forwardResult is a fully read non-streaming upstream response.
type forwardResult struct {
	status int
	header http.Header
	body   []byte
}

// forward makes one non-streaming POST to the upstream and reads the whole
// body before returning. The body must be read here, inside the per-upstream
// timeout context: cancelling that context (the deferred cancel) aborts any
// body read still in progress, so returning the *http.Response and reading
// it in the caller fails whenever the body arrives after the headers (P0.1).
func forward(ctx context.Context, client *http.Client, r UpstreamRequest) (forwardResult, error) {
	url := strings.TrimRight(r.Upstream.BaseURL, "/") + r.Path
	timeout := time.Duration(r.Upstream.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(r.Body))
	if err != nil {
		return forwardResult{}, fmt.Errorf("build request: %w", err)
	}
	buildHeaders(httpReq, r.Upstream, r.APIKey)
	resp, err := client.Do(httpReq)
	if err != nil {
		return forwardResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return forwardResult{}, fmt.Errorf("read response body: %w", err)
	}
	return forwardResult{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// forwardStream makes one streaming POST; the read timeout is intentionally
// unbounded (the caller's context still carries cancellation from client
// disconnect) since chat streams can legitimately run long.
func forwardStream(ctx context.Context, client *http.Client, r UpstreamRequest) (*http.Response, error) {
	url := strings.TrimRight(r.Upstream.BaseURL, "/") + r.Path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	buildHeaders(httpReq, r.Upstream, r.APIKey)
	return client.Do(httpReq)
}
