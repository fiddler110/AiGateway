// Package app assembles the gateway's runtime pieces (provider registry,
// AppState, HTTP router) from a loaded config. It lives outside
// cmd/aigateway so tests can build exactly the same router the binary
// serves, instead of a hand-maintained copy.
package app

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/handlers"
	"github.com/scottymacleod/aigateway/internal/middleware"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/server"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

// NewRegistry returns the provider translators this build supports.
func NewRegistry() provider.Registry {
	return provider.Registry{
		"openai": func() provider.Translator { return openai.New() },
	}
}

// BuildState constructs one AppState generation from a validated config.
func BuildState(cfg *config.Config, client *http.Client, registry provider.Registry) (*server.AppState, error) {
	pipe, err := middleware.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("build middleware pipeline: %w", err)
	}
	return &server.AppState{
		Cfg:         cfg,
		Pipeline:    pipe,
		UpstreamMgr: upstream.NewManager(cfg, client, registry),
		Registry:    registry,
	}, nil
}

// NewRouter returns the gateway's HTTP routes bound to srv.
func NewRouter(srv *server.Server) http.Handler {
	mux := chi.NewRouter()
	mux.Post("/v1/chat/completions", handlers.Chat(srv))
	mux.Get("/v1/models", handlers.Models(srv))
	mux.Get("/health", handlers.Health(srv))
	return mux
}
