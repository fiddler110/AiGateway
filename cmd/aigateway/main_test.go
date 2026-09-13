package main

import (
	"net/http"
	"testing"
	"time"
)

// P0.14: header reads and idle connections are bounded; writes are not, so
// long streams aren't cut off.
func TestNewHTTPServerTimeouts(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", s.ReadHeaderTimeout)
	}
	if s.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", s.IdleTimeout)
	}
	if s.WriteTimeout != 0 || s.ReadTimeout != 0 {
		t.Errorf("WriteTimeout = %v, ReadTimeout = %v, want both unset", s.WriteTimeout, s.ReadTimeout)
	}
	if s.Addr != "127.0.0.1:0" || s.Handler == nil {
		t.Errorf("Addr/Handler not set")
	}
}
