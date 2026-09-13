// Command aigateway runs the AI gateway HTTP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/handlers"
	"github.com/scottymacleod/aigateway/internal/httpclient"
	"github.com/scottymacleod/aigateway/internal/middleware"
	"github.com/scottymacleod/aigateway/internal/provider"
	"github.com/scottymacleod/aigateway/internal/provider/openai"
	"github.com/scottymacleod/aigateway/internal/server"
	"github.com/scottymacleod/aigateway/internal/upstream"
)

func newRegistry() provider.Registry {
	return provider.Registry{
		"openai": func() provider.Translator { return openai.New() },
	}
}

func buildAppState(cfg *config.Config, client *http.Client, registry provider.Registry) (*server.AppState, error) {
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

func main() {
	configPath := flag.String("config", "config.yaml", "path to gateway config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	server.SetupLogging(cfg.Settings.LogLevel)

	client := httpclient.New()
	registry := newRegistry()

	srv := server.New(client)
	initialState, err := buildAppState(cfg, client, registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build initial state: %v\n", err)
		os.Exit(1)
	}
	srv.Swap(initialState)

	mux := chi.NewRouter()
	mux.Post("/v1/chat/completions", handlers.Chat(srv))
	mux.Get("/v1/models", handlers.Models(srv))
	mux.Get("/health", handlers.Health(srv))

	addr := fmt.Sprintf("%s:%d", cfg.Settings.ListenHost, cfg.Settings.ListenPort)
	httpServer := &http.Server{Addr: addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("aigateway listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
}
