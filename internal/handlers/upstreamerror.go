package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/scottymacleod/aigateway/internal/server"
)

// maxUpstreamErrorMessage caps the upstream error message relayed to a
// client.
const maxUpstreamErrorMessage = 1024

// writeUpstreamError relays an upstream 4xx to the client with the
// upstream's status in the gateway's OpenAI error envelope. Only a short
// message string is taken from the upstream body; the body itself is never
// relayed, so non-JSON bodies (HTML error pages from proxies) aren't
// reflected.
//
// 401, 403 and 407 are the exception: the credential the upstream rejected
// is the gateway's own (api_key_env), not the one the client presented.
// Relaying 401 would tell the client its gateway key is wrong, and upstream
// messages for these statuses can quote part of the rejected key, so the
// client gets a generic 502 and the detail stays in the server log.
func writeUpstreamError(w http.ResponseWriter, upstreamName string, status int, header http.Header, body []byte) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		slog.Warn("upstream rejected gateway credentials", "upstream", upstreamName, "status", status)
		server.WriteError(w, http.StatusBadGateway, "upstream rejected the gateway's credentials")
		return
	}
	if status == http.StatusTooManyRequests {
		if ra := header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
	}
	msg := upstreamErrorMessage(body)
	if msg == "" {
		msg = "upstream error: " + http.StatusText(status)
	}
	server.WriteError(w, status, msg)
}

// upstreamErrorMessage extracts a message from the error body shapes
// upstreams use: OpenAI {"error":{"message":...}}, Ollama/llama.cpp
// {"error":"..."}, and bare {"message":...}. Returns "" if none match.
func upstreamErrorMessage(body []byte) string {
	var parsed struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	msg := parsed.Message
	var nested struct {
		Message string `json:"message"`
	}
	var flat string
	switch {
	case json.Unmarshal(parsed.Error, &nested) == nil && nested.Message != "":
		msg = nested.Message
	case json.Unmarshal(parsed.Error, &flat) == nil && flat != "":
		msg = flat
	}
	return truncateUTF8(msg, maxUpstreamErrorMessage)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
