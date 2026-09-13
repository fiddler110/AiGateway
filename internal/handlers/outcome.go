package handlers

import (
	"math"
	"net/http"
	"strconv"

	"github.com/scottymacleod/aigateway/internal/pipeline"
	"github.com/scottymacleod/aigateway/internal/server"
)

// outcomeRecorder wraps the chat handler's ResponseWriter to learn the status
// actually sent, for gctx.Status and audit_log. A stream commits to 200
// before it can fail, so writeSSEError also reports its event's code here,
// and that code is the outcome.
type outcomeRecorder struct {
	http.ResponseWriter
	status    int
	sseStatus int
}

func (o *outcomeRecorder) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *outcomeRecorder) Write(b []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the wrapped writer.
func (o *outcomeRecorder) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// flushingOutcomeRecorder is an outcomeRecorder over a writer that can
// flush. The two types exist so that the wrapper implements http.Flusher
// exactly when the writer it wraps does: a Flush method that silently did
// nothing would make serveStream's "streaming not supported" check pass for
// a writer that cannot stream, and every event would sit in a buffer.
type flushingOutcomeRecorder struct {
	*outcomeRecorder
	flusher http.Flusher
}

func (f flushingOutcomeRecorder) Flush() {
	if f.status == 0 {
		f.status = http.StatusOK // flushing commits the headers
	}
	f.flusher.Flush()
}

// wrapOutcome wraps w in an outcomeRecorder, keeping http.Flusher only when
// w implements it.
func wrapOutcome(w http.ResponseWriter) (http.ResponseWriter, *outcomeRecorder) {
	rec := &outcomeRecorder{ResponseWriter: w}
	if f, ok := w.(http.Flusher); ok {
		return flushingOutcomeRecorder{outcomeRecorder: rec, flusher: f}, rec
	}
	return rec, rec
}

func (o *outcomeRecorder) streamError(code int) {
	if o.sseStatus == 0 {
		o.sseStatus = code
	}
}

// outcome is the status to record: the SSE error code if a stream failed,
// else the status written (200 if the handler wrote nothing).
func (o *outcomeRecorder) outcome() int {
	switch {
	case o.sseStatus != 0:
		return o.sseStatus
	case o.status != 0:
		return o.status
	default:
		return http.StatusOK
	}
}

// writeBlock sends a block before any response bytes: its status (400 for
// content and DLP blocks, 429 for rate-limit and budget blocks) and, for a
// 429, Retry-After in whole seconds, rounded up and at least 1.
func writeBlock(w http.ResponseWriter, gctx *pipeline.GatewayContext) {
	status := gctx.BlockHTTPStatus()
	if status == http.StatusTooManyRequests {
		secs := max(int64(math.Ceil(gctx.RetryAfter.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	server.WriteError(w, status, gctx.BlockReason)
}
