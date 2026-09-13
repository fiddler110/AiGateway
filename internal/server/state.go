// Package server wires together config, the middleware pipeline, and the
// upstream manager into a running HTTP server, and owns the hot-reload
// atomic state swap.
package server

import (
	"net/http"
	"sync/atomic"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/pipeline"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

// AppState is one immutable generation of the gateway's live configuration.
// A hot reload builds an entirely new AppState and swaps it in with a single
// atomic store, so no in-flight or new request ever observes a torn
// combination of old-pipeline+new-upstream-config or vice versa.
type AppState struct {
	Cfg         *config.Config
	Pipeline    *pipeline.Pipeline
	UpstreamMgr *upstream.Manager
	Registry    provider.Registry
	// Auth holds Cfg's precomputed credential digests (NewAuthenticator).
	Auth *Authenticator
}

// Server holds the long-lived resources that outlive any single AppState
// generation (HTTP client, current-state pointer) plus everything needed to
// build a new generation on reload.
type Server struct {
	current atomic.Pointer[AppState]
	client  *http.Client
}

func New(client *http.Client) *Server {
	return &Server{client: client}
}

// CurrentState returns the active AppState. Handlers must call this exactly
// once at the start of request handling and use the returned snapshot for
// the rest of the request, so a concurrent hot reload never produces a
// mixed-generation view within one request.
func (s *Server) CurrentState() *AppState {
	return s.current.Load()
}

func (s *Server) Swap(next *AppState) {
	s.current.Store(next)
}
