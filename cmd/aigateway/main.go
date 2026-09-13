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

	"github.com/scottymacleod/aigateway/internal/app"
	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/httpclient"
	"github.com/scottymacleod/aigateway/internal/server"
)

// newHTTPServer bounds header reads and idle keep-alives against
// slowloris-style connection exhaustion (P0.14). WriteTimeout and
// ReadTimeout stay unset: they would cut off long streams and large uploads,
// which settings.stream_timeout, request_timeout and max_request_bytes bound
// instead.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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
	for _, w := range cfg.Warnings {
		slog.Warn("config: " + w)
	}

	client := httpclient.New()
	registry := app.NewRegistry()

	srv := server.New(client)
	initialState, err := app.BuildState(cfg, client, registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build initial state: %v\n", err)
		os.Exit(1)
	}
	srv.Swap(initialState)

	addr := fmt.Sprintf("%s:%d", cfg.Settings.ListenHost, cfg.Settings.ListenPort)
	httpServer := newHTTPServer(addr, app.NewRouter(srv))

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
	if err := srv.CurrentState().Close(); err != nil {
		slog.Error("close gateway state", "err", err)
	}
}
