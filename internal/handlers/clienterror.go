package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/scottymacleod/aigateway/internal/server"
)

// requestIDHeader carries the per-request ID back to the client so a
// generic error message can be matched to its server log line.
const requestIDHeader = "x-request-id"

// newRequestID returns 16 random hex characters. Deliberately minimal: P1.8
// owns full request IDs (middleware, access log, audit rows).
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// withRequestID appends the request ID to a generic client message. Client
// messages for upstream and internal failures never include err.Error(),
// which can carry upstream URLs, hostnames, and IPs (P0.8); the detail goes
// to the server log under the same ID.
func withRequestID(msg, requestID string) string {
	return msg + " (request_id: " + requestID + ")"
}

// writeSSEError writes one SSE error event whose data payload is the same
// ErrorEnvelope the non-streaming path returns, JSON-encoded so any message
// text is escaped correctly (P0.7), followed by [DONE] so clients stop
// reading. The caller must already have sent the event-stream headers.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, status int, message string) {
	// An interface, not *outcomeRecorder: the flushing variant embeds it.
	if rec, ok := w.(interface{ streamError(int) }); ok {
		rec.streamError(status)
	}
	data, err := marshalJSON(server.NewErrorEnvelope(status, message))
	if err != nil {
		return // unreachable: the envelope is strings and an int
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	flusher.Flush()
}
