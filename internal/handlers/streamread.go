package handlers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/scottymacleod/aigateway/internal/upstream"
)

// maxSSELineBytes caps one upstream SSE line. A longer line is an error
// (bufio.ErrTooLong), never a silent end of stream.
const maxSSELineBytes = 4 * 1024 * 1024

// readErrRecorder remembers the first non-EOF error its reader returns.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (e *readErrRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// newSSELineScanner scans body line by line. Unlike a plain bufio.Scanner,
// when the read fails (dropped connection, stream timeout, response limit)
// it never yields the unterminated partial line in front of the failure:
// scanning stops with that error instead, so a truncated line is never
// forwarded as if it were complete.
func newSSELineScanner(body io.Reader) *bufio.Scanner {
	src := &readErrRecorder{r: body}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && src.err != nil && bytes.IndexByte(data, '\n') < 0 {
			return 0, nil, src.err
		}
		return bufio.ScanLines(data, atEOF)
	})
	return scanner
}

// writeStreamReadError reports a failed upstream stream read to the client
// as an SSE error event with a generic message; the error detail goes only
// to the server log. streamCtx is the context bounded by stream_timeout.
func writeStreamReadError(w http.ResponseWriter, flusher http.Flusher, streamCtx context.Context, requestID, upstreamName string, err error) {
	switch {
	case errors.Is(streamCtx.Err(), context.DeadlineExceeded):
		slog.Error("upstream stream timed out", "request_id", requestID, "upstream", upstreamName, "err", err)
		writeSSEError(w, flusher, http.StatusGatewayTimeout, withRequestID("upstream stream timed out", requestID))
	case errors.Is(streamCtx.Err(), context.Canceled):
		// The client went away; there is nobody to tell.
		slog.Debug("client disconnected during stream", "request_id", requestID, "upstream", upstreamName)
	case errors.Is(err, upstream.ErrResponseTooLarge), errors.Is(err, bufio.ErrTooLong):
		slog.Error("upstream response too large", "request_id", requestID, "upstream", upstreamName, "stream", true, "err", err)
		writeSSEError(w, flusher, http.StatusBadGateway, withRequestID("upstream response too large", requestID))
	default:
		slog.Error("upstream stream read failed", "request_id", requestID, "upstream", upstreamName, "err", err)
		writeSSEError(w, flusher, http.StatusServiceUnavailable, withRequestID("upstream stream read failed", requestID))
	}
}
