package handlers

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/middleware"
	"github.com/scottymacleod/aigateway/internal/pipeline"
	"github.com/scottymacleod/aigateway/internal/server"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

// Chat handles POST /v1/chat/completions: auth, model routing, the
// middleware pipeline (populated starting in Phase 2 — an empty pipeline is
// a no-op here), and upstream forwarding with fallback/retry, for both
// streaming and non-streaming requests.
func Chat(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := srv.CurrentState()

		auth, ok := server.Authenticate(st.Cfg, r)
		if !ok {
			server.WriteError(w, http.StatusUnauthorized, "invalid or missing credentials")
			return
		}

		var req chatmodel.ChatRequest
		if err := server.ReadJSONBody(r, st.Cfg.Settings.MaxRequestBytes, &req); err != nil {
			var tooLarge *server.ErrBodyTooLarge
			if errors.As(err, &tooLarge) {
				server.WriteError(w, http.StatusRequestEntityTooLarge, tooLarge.Error())
				return
			}
			server.WriteError(w, http.StatusBadRequest, "invalid JSON request body: "+err.Error())
			return
		}
		if req.Model == "" || len(req.Messages) == 0 {
			server.WriteError(w, http.StatusBadRequest, "request must include a non-empty model and at least one message")
			return
		}

		candidates, err := config.ResolveModelRoute(st.Cfg, req.Model, auth.UpstreamOverride)
		if err != nil {
			server.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}

		gctx := pipeline.NewGatewayContext(auth.ClientID, candidates[0], server.SourceIP(st.Cfg, r))
		gctx.Scratch.RequestModel = req.Model

		if st.Pipeline != nil {
			if err := st.Pipeline.Run(r.Context(), &req, gctx); err != nil {
				server.WriteError(w, http.StatusInternalServerError, "internal middleware error")
				return
			}
			if gctx.Blocked {
				server.WriteError(w, http.StatusBadRequest, gctx.BlockReason)
				return
			}
		}

		apiKeyFor := func(upstreamName string) string {
			up := st.Cfg.Upstreams[upstreamName]
			envVar := up.APIKeyEnv
			if auth.UpstreamKeyEnvOverride != "" {
				envVar = auth.UpstreamKeyEnvOverride
			}
			return upstream.ResolveAPIKey(envVar)
		}

		if req.Stream {
			serveStream(w, r, gctx, st, candidates, &req, apiKeyFor)
			return
		}

		ctx := r.Context()
		if st.Cfg.Settings.RequestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(st.Cfg.Settings.RequestTimeout*float64(time.Second)))
			defer cancel()
		}

		body, _, servedBy, err := st.UpstreamMgr.Send(ctx, candidates, &req, apiKeyFor)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				server.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
				return
			}
			server.WriteError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		gctx.Upstream = servedBy

		body, err = applyResponsePipeline(r.Context(), st.Pipeline, gctx, body)
		if err != nil {
			server.WriteError(w, http.StatusInternalServerError, "internal middleware error")
			return
		}
		if gctx.Blocked {
			server.WriteError(w, http.StatusBadRequest, gctx.BlockReason)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// serveStream forwards a streaming chat request. Two modes:
//
//   - Buffer mode, forced when secrets_scanner is active (flag-after-leak
//     is unacceptable for credentials) or settings.stream_buffer is true:
//     collect the entire upstream stream, run the response pipeline ONCE
//     over the reconstructed full content (so it CAN block), then emit
//     either the real reverse-pseudonymized stream or a synthetic blocked
//     event.
//   - Passthrough mode (default): forward each complete line immediately
//     after reverse-pseudonymizing it, then at EOF run the response
//     pipeline once for accounting/flagging only — it cannot block, since
//     bytes have already reached the client.
func serveStream(w http.ResponseWriter, r *http.Request, gctx *pipeline.GatewayContext, st *server.AppState, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(string) string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		server.WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	body, servedBy, err := st.UpstreamMgr.SendStream(r.Context(), candidates, req, apiKeyFor)
	if err != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"status\":503,\"detail\":\"" + err.Error() + "\"}}\n\n"))
		flusher.Flush()
		return
	}
	gctx.Upstream = servedBy
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	bufferMode := middleware.SecretsScannerActive(st.Cfg) || st.Cfg.Settings.StreamBuffer
	if bufferMode {
		serveBufferedStream(w, flusher, r.Context(), st.Pipeline, gctx, body)
	} else {
		servePassthroughStream(w, flusher, r.Context(), st.Pipeline, gctx, body)
	}
}

func serveBufferedStream(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body io.ReadCloser) {
	raw, err := io.ReadAll(body)
	if err != nil {
		_, _ = w.Write([]byte("data: {\"error\":{\"status\":503,\"detail\":\"upstream stream read failed\"}}\n\n"))
		flusher.Flush()
		return
	}

	content := extractSSEContent(raw)
	if prompt, completion, ok := extractSSEUsage(raw); ok {
		gctx.Scratch.ActualPromptTokens = &prompt
		gctx.Scratch.ActualComplTokens = &completion
	}

	if pipe != nil {
		if _, err := pipe.RunResponse(ctx, content, gctx); err != nil {
			_, _ = w.Write([]byte("data: {\"error\":{\"status\":500,\"detail\":\"internal middleware error\"}}\n\n"))
			flusher.Flush()
			return
		}
	}
	if gctx.Blocked {
		_, _ = w.Write([]byte("data: {\"error\":{\"status\":400,\"detail\":\"" + gctx.BlockReason + "\"}}\n\ndata: [DONE]\n\n"))
		flusher.Flush()
		return
	}

	_, _ = w.Write([]byte(gctx.ReverseSubstitute(string(raw))))
	flusher.Flush()
}

func servePassthroughStream(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body io.ReadCloser) {
	var full []byte
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		full = append(full, line...)
		full = append(full, '\n')
		reversed := gctx.ReverseSubstitute(string(line))
		_, _ = w.Write([]byte(reversed))
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}

	if pipe == nil {
		return
	}
	content := extractSSEContent(full)
	if prompt, completion, ok := extractSSEUsage(full); ok {
		gctx.Scratch.ActualPromptTokens = &prompt
		gctx.Scratch.ActualComplTokens = &completion
	}
	// Accounting/flagging only — bytes are already on the wire, so a block
	// detected here cannot be enforced; it's logged via BlockMiddleware for
	// visibility instead.
	_, _ = pipe.RunResponseAccountingOnly(ctx, content, gctx)
}
