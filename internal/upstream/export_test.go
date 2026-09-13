package upstream

import (
	"context"
	"io"
	"time"

	"github.com/scottymacleod/aigateway/internal/provider"
)

// NewTranslatedStreamForTest exposes the stream translation adapter.
func NewTranslatedStreamForTest(src io.ReadCloser, t provider.StreamTranslator) io.ReadCloser {
	return newTranslatedStream(src, t, "test")
}

// SetBackoffHooks replaces the Manager's backoff sleeper and jitter source
// so tests can observe delays without really sleeping. A nil hook is left
// unchanged.
func SetBackoffHooks(m *Manager, sleep func(context.Context, time.Duration) error, randFloat func() float64) {
	if sleep != nil {
		m.sleep = sleep
	}
	if randFloat != nil {
		m.randFloat = randFloat
	}
}
