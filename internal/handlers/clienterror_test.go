package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scottymacleod/aigateway/internal/server"
)

// P0.7: SSE error events are JSON-encoded, so quotes and backslashes in a
// message survive, and the payload matches the non-streaming envelope.
func TestWriteSSEErrorEscapesMessage(t *testing.T) {
	for _, msg := range []string{`plain`, `has "quotes"`, `back\slash`, "new\nline", `</script>&`} {
		rec := httptest.NewRecorder()
		writeSSEError(rec, rec, http.StatusBadRequest, msg)

		body := rec.Body.String()
		payload, ok := strings.CutPrefix(body, "data: ")
		if !ok || !strings.HasSuffix(body, "\n\ndata: [DONE]\n\n") {
			t.Fatalf("unexpected framing %q", body)
		}
		payload = strings.TrimSuffix(payload, "\n\ndata: [DONE]\n\n")
		var got server.ErrorEnvelope
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("message %q: invalid JSON payload %q: %v", msg, payload, err)
		}
		if want := server.NewErrorEnvelope(http.StatusBadRequest, msg); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
}
