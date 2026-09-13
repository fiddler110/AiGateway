// Package audit logs one structured entry per request (client, model,
// message count, upstream, streaming flag, source IP). It is the reference
// example of a fail-open middleware: a bug here must never take down the
// gateway.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type Middleware struct {
	logFile string
	mu      sync.Mutex // serializes appends to logFile
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{}
	if v, ok := cfg["log_file"].(string); ok {
		m.logFile = v
	}
	return m, nil
}

func (m *Middleware) Name() string { return "audit_log" }

func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	// Defensive re-sanitization even though the auth layer already
	// sanitized ClientID — defense in depth against a future auth-layer
	// regression writing an unsanitized value here.
	gctx.ClientID = chatmodel.SanitizeClientID(gctx.ClientID)

	entry := map[string]any{
		"client_id":      gctx.ClientID,
		"model":          req.Model,
		"message_count":  len(req.Messages),
		"upstream":       gctx.Upstream,
		"stream":         req.Stream,
		"source_ip":      gctx.SourceIP,
	}
	slog.Info("audit", "client_id", gctx.ClientID, "model", req.Model, "upstream", gctx.Upstream, "source_ip", gctx.SourceIP)

	if m.logFile != "" {
		if err := m.appendToFile(entry); err != nil {
			return err // caller applies fail_open policy (default true for this middleware)
		}
	}
	return nil
}

func (m *Middleware) appendToFile(entry map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if dir := filepath.Dir(m.logFile); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(m.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, _ *pipeline.GatewayContext) (string, error) {
	return text, nil // audit_log is request-phase only
}
