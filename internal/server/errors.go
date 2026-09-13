package server

import (
	"encoding/json"
	"net/http"
)

var errorTypes = map[int]string{
	http.StatusBadRequest:            "invalid_request_error",
	http.StatusUnauthorized:          "authentication_error",
	http.StatusForbidden:             "permission_error",
	http.StatusNotFound:              "not_found_error",
	http.StatusRequestEntityTooLarge: "invalid_request_error",
	http.StatusTooManyRequests:       "rate_limit_error",
}

// ErrorEnvelope is the OpenAI-compatible error body
// {"error":{"message","type","code"}} that OpenAI-format clients (Claude
// Code, VS Code Copilot, opencode, etc.) already expect to parse. Streaming
// error events carry the same envelope as their data payload.
type ErrorEnvelope struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner object of ErrorEnvelope.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    int    `json:"code"`
}

// NewErrorEnvelope builds the envelope for status, mapping it to an OpenAI
// error type ("api_error" for anything unmapped).
func NewErrorEnvelope(status int, message string) ErrorEnvelope {
	errType, ok := errorTypes[status]
	if !ok {
		errType = "api_error"
	}
	return ErrorEnvelope{Error: ErrorDetail{Message: message, Type: errType, Code: status}}
}

// WriteError writes status with an ErrorEnvelope body.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(NewErrorEnvelope(status, message))
}
