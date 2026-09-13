package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ErrBodyTooLarge is returned when the request body exceeds max_bytes.
type ErrBodyTooLarge struct{ MaxBytes int64 }

func (e *ErrBodyTooLarge) Error() string {
	return fmt.Sprintf("request body exceeds %d bytes", e.MaxBytes)
}

// ReadJSONBody defends against oversized/malformed bodies without ever
// buffering more than maxBytes: it streams the body via an io.LimitedReader
// sized to maxBytes+1, so a body that is too large is detected as soon as
// the limit is crossed — regardless of whether Content-Length was accurate,
// present, or a lie (e.g. chunked transfer encoding). maxBytes <= 0 means
// unlimited.
func ReadJSONBody(r *http.Request, maxBytes int64, out any) error {
	if maxBytes <= 0 {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	}

	limited := &io.LimitedReader{R: r.Body, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return &ErrBodyTooLarge{MaxBytes: maxBytes}
	}
	return json.Unmarshal(data, out)
}
