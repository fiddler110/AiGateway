package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
		startedAt := time.Now()
		w, rec := wrapOutcome(w)
		st := srv.CurrentState()
		requestID := newRequestID()
		w.Header().Set(requestIDHeader, requestID)

		auth, ok := st.Auth.Authenticate(r)
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
		gctx.Authenticated = auth.Authenticated
		gctx.Scratch.RequestModel = req.Model
		gctx.RequestID = requestID
		gctx.StartedAt = startedAt

		if st.Pipeline != nil {
			// Every exit from here on (served, blocked, middleware or
			// upstream failure) reaches Finish, after the response is sent.
			// WithoutCancel: a client that has gone away still gets audited.
			defer func() {
				gctx.Status = rec.outcome()
				st.Pipeline.Finish(context.WithoutCancel(r.Context()), gctx)
			}()
			if err := st.Pipeline.Run(r.Context(), &req, gctx); err != nil {
				slog.Error("request middleware failed", "request_id", requestID, "err", err)
				server.WriteError(w, http.StatusInternalServerError, withRequestID("internal middleware error", requestID))
				return
			}
			if gctx.Blocked {
				writeBlock(w, gctx)
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
			serveStream(w, r, requestID, gctx, st, candidates, &req, apiKeyFor)
			return
		}

		ctx := r.Context()
		if st.Cfg.Settings.RequestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(st.Cfg.Settings.RequestTimeout*float64(time.Second)))
			defer cancel()
		}

		res, err := st.UpstreamMgr.Send(ctx, candidates, &req, apiKeyFor)
		if err != nil {
			// err can carry upstream URLs, hosts, and IPs: log it, never send it.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				slog.Error("upstream request timed out", "request_id", requestID, "stream", false, "err", err)
				server.WriteError(w, http.StatusGatewayTimeout, withRequestID("upstream request timed out", requestID))
				return
			}
			if errors.Is(err, upstream.ErrResponseTooLarge) {
				slog.Error("upstream response too large", "request_id", requestID, "stream", false, "limit", st.Cfg.Settings.MaxResponseBytes, "err", err)
				server.WriteError(w, http.StatusBadGateway, withRequestID("upstream response too large", requestID))
				return
			}
			slog.Error("upstream request failed", "request_id", requestID, "stream", false, "err", err)
			server.WriteError(w, http.StatusServiceUnavailable, withRequestID("upstream unavailable", requestID))
			return
		}
		gctx.Upstream = res.ServedBy
		gctx.ServedBy = res.ServedBy
		if res.Status >= 400 {
			// Error bodies aren't model output, so the response pipeline
			// doesn't run over them.
			writeUpstreamError(w, res.ServedBy, res.Status, res.Header, res.Body)
			return
		}

		body, err := applyResponsePipeline(r.Context(), st.Pipeline, gctx, res.Body)
		if err != nil {
			slog.Error("response middleware failed", "request_id", requestID, "stream", false, "err", err)
			server.WriteError(w, http.StatusInternalServerError, withRequestID("internal middleware error", requestID))
			return
		}
		if gctx.Blocked {
			writeBlock(w, gctx)
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
//     collect the entire upstream stream, collapse it to one message per
//     choice, run the response pipeline over every model-generated string
//     field (content, reasoning, refusal, tool-call arguments), so it CAN
//     block, and its rewrites (redaction, pseudonym reversal) are what the
//     client receives, then re-emit synthesized chunks or an error event.
//     The raw upstream bytes are never forwarded, nor are upstream error
//     events.
//   - Passthrough mode (default): forward each line as it arrives, decoding
//     chunks to reverse pseudonymization in their string fields and holding
//     back text that may be a fake split across chunks. At EOF run the
//     response pipeline once for accounting/flagging only — it cannot block,
//     since bytes have already reached the client.
func serveStream(w http.ResponseWriter, r *http.Request, requestID string, gctx *pipeline.GatewayContext, st *server.AppState, candidates []string, req *chatmodel.ChatRequest, apiKeyFor func(string) string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		server.WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// stream_timeout bounds the whole upstream stream (P0.9). Its expiry
	// cancels the upstream request, which fails the body read in progress.
	// The response pipeline keeps r.Context(): the deadline is for upstream.
	streamCtx := r.Context()
	if st.Cfg.Settings.StreamTimeout > 0 {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithTimeout(streamCtx, time.Duration(st.Cfg.Settings.StreamTimeout*float64(time.Second)))
		defer cancel()
	}

	res, err := st.UpstreamMgr.SendStream(streamCtx, candidates, req, apiKeyFor)
	if err != nil {
		// err can carry upstream URLs, hosts, and IPs: log it, never send it.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if errors.Is(streamCtx.Err(), context.DeadlineExceeded) {
			slog.Error("upstream request timed out", "request_id", requestID, "stream", true, "err", err)
			writeSSEError(w, flusher, http.StatusGatewayTimeout, withRequestID("upstream stream timed out", requestID))
			return
		}
		slog.Error("upstream request failed", "request_id", requestID, "stream", true, "err", err)
		writeSSEError(w, flusher, http.StatusServiceUnavailable, withRequestID("upstream unavailable", requestID))
		return
	}
	gctx.Upstream = res.ServedBy
	gctx.ServedBy = res.ServedBy
	if res.Body == nil {
		// Nothing has been written yet, so a 4xx can still go out as a plain
		// JSON error with the upstream's status instead of a 200 stream.
		writeUpstreamError(w, res.ServedBy, res.Status, res.Header, res.ErrorBody)
		return
	}
	body := res.Body
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	bufferMode := middleware.SecretsScannerActive(st.Cfg) || st.Cfg.Settings.StreamBuffer
	if bufferMode {
		serveBufferedStream(w, flusher, r.Context(), streamCtx, requestID, st.Pipeline, gctx, body)
	} else {
		servePassthroughStream(w, flusher, r.Context(), streamCtx, requestID, st.Pipeline, gctx, body)
	}
}

// serveBufferedStream: any read failure (limit, timeout, drop) or oversized
// line produces only an error event; nothing partial or unscanned is sent.
func serveBufferedStream(w http.ResponseWriter, flusher http.Flusher, ctx, streamCtx context.Context, requestID string, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body io.ReadCloser) {
	raw, err := io.ReadAll(body)
	if err != nil {
		writeStreamReadError(w, flusher, streamCtx, requestID, gctx.Upstream, err)
		return
	}

	stream, err := collapseStream(raw)
	if err != nil {
		writeStreamReadError(w, flusher, streamCtx, requestID, gctx.Upstream, err)
		return
	}
	if stream.errorEvents > 0 {
		// An in-stream error means the stream failed. Its payload is unscanned
		// upstream text, so the client gets only a generic error, and the log
		// gets no payload (P0.8, P0.16).
		slog.Error("upstream sent an error event in stream", "request_id", requestID, "upstream", gctx.Upstream, "error_events", stream.errorEvents)
		writeSSEError(w, flusher, http.StatusBadGateway, withRequestID("upstream stream failed", requestID))
		return
	}
	if prompt, completion, ok := extractSSEUsage(raw); ok {
		gctx.Scratch.ActualPromptTokens = &prompt
		gctx.Scratch.ActualComplTokens = &completion
	}

	if pipe != nil {
		if err := stream.runResponsePipeline(ctx, pipe, gctx); err != nil {
			slog.Error("response middleware failed", "request_id", requestID, "stream", true, "err", err)
			writeSSEError(w, flusher, http.StatusInternalServerError, withRequestID("internal middleware error", requestID))
			return
		}
	}
	if gctx.Blocked {
		// Headers (200) are already sent, so the block's status travels only
		// as the error event's code and type; a 429 here can't carry a
		// Retry-After header. Rate-limit and budget blocks happen in the
		// request phase, before any header, so in practice this is a 400.
		writeSSEError(w, flusher, gctx.BlockHTTPStatus(), gctx.BlockReason)
		return
	}

	_, _ = w.Write(stream.sse())
	flusher.Flush()
}

// servePassthroughStream: a read failure (limit, timeout, drop) or oversized
// line ends the stream with an SSE error event instead of a silent stop
// (P0.9), unless upstream had already sent [DONE].
func servePassthroughStream(w http.ResponseWriter, flusher http.Flusher, ctx, streamCtx context.Context, requestID string, pipe *pipeline.Pipeline, gctx *pipeline.GatewayContext, body io.ReadCloser) {
	rev := newPassthroughReverser(gctx.Scratch.ReverseMap)
	var full []byte
	sawDone := false
	scanner := newSSELineScanner(body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if payload, ok := ssePayload(line); ok && string(payload) == "[DONE]" {
			sawDone = true
		}
		full = append(full, line...)
		full = append(full, '\n')
		_, _ = w.Write(rev.rewriteLine(line))
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}
	switch err := scanner.Err(); {
	case err == nil:
		if tail := rev.finish(); tail != nil {
			_, _ = w.Write([]byte("data: " + string(tail) + "\n\n"))
			flusher.Flush()
		}
	case sawDone:
		// The client already has a complete stream; don't append an error.
		slog.Warn("upstream stream failed after [DONE]", "request_id", requestID, "upstream", gctx.Upstream, "err", err)
	default:
		writeStreamReadError(w, flusher, streamCtx, requestID, gctx.Upstream, err)
	}

	if pipe == nil {
		return
	}
	if prompt, completion, ok := extractSSEUsage(full); ok {
		gctx.Scratch.ActualPromptTokens = &prompt
		gctx.Scratch.ActualComplTokens = &completion
	}
	// Accounting/flagging only — bytes are already on the wire, so a block
	// detected here cannot be enforced; it's logged via BlockMiddleware for
	// visibility instead. It sees the same fields buffered mode scans.
	var fields []pipeline.ResponseField
	if stream, err := collapseStream(full); err == nil {
		fields = stream.responseText().fields
	}
	_, _ = pipe.RunResponseAccountingOnly(ctx, fields, gctx)
}
