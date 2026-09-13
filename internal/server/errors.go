package server

import (
	"encoding/json"
	"net/http"
)

var errorTypes = map[int]string{
	http.StatusBadRequest:          "invalid_request_error",
	http.StatusUnauthorized:        "authentication_error",
	http.StatusForbidden:           "permission_error",
	http.StatusNotFound:            "not_found_error",
	http.StatusRequestEntityTooLarge: "invalid_request_error",
	http.StatusTooManyRequests:     "rate_limit_error",
}

// WriteError writes an OpenAI-compatible error envelope
// {"error":{"message","type","code"}} matching what OpenAI-format clients
// (Claude Code, VS Code Copilot, opencode, etc.) already expect to parse.
func WriteError(w http.ResponseWriter, status int, message string) {
	errType, ok := errorTypes[status]
	if !ok {
		errType = "api_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}
