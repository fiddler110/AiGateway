// Package audit logs one structured record per request that reaches the
// middleware pipeline. The request phase only captures request metadata
// (message count, streaming flag); the record is written in Finish, after
// the response, so it carries the upstream that actually served the request,
// the status sent, latency, provider-reported tokens, and the block outcome,
// including for blocked requests and upstream failures (P0.14). Records
// never contain message or response bodies, matched content, or block
// reasons. It is the reference example of a fail-open middleware: a bug here
// must never take down the gateway.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

// Log files hold source IPs and client identities: owner read/write, group
// read, nothing for others.
const (
	fileMode = 0o640
	dirMode  = 0o750
)

type Middleware struct {
	logFile string
	// openFile is os.OpenFile; tests swap it to observe the mode requested
	// (file modes don't map onto Windows ACLs, so the result can't be
	// checked there).
	openFile func(name string, flag int, perm os.FileMode) (*os.File, error)

	mu     sync.Mutex // guards f and closed, and serializes appends
	f      *os.File   // opened on first write and kept open
	closed bool
}

func New(cfg map[string]any) (pipeline.Middleware, error) {
	m := &Middleware{openFile: os.OpenFile}
	if v, ok := cfg["log_file"].(string); ok {
		m.logFile = v
	}
	return m, nil
}

func (m *Middleware) Name() string { return "audit_log" }

// Process captures request metadata for Finish. It writes nothing: the
// outcome isn't known yet.
func (m *Middleware) Process(_ context.Context, req *chatmodel.ChatRequest, gctx *pipeline.GatewayContext) error {
	// Defensive re-sanitization even though the auth layer already
	// sanitized ClientID — defense in depth against a future auth-layer
	// regression writing an unsanitized value here.
	gctx.ClientID = chatmodel.SanitizeClientID(gctx.ClientID)
	gctx.Scratch.MessageCount = len(req.Messages)
	gctx.Scratch.Stream = req.Stream
	gctx.Scratch.AuditCaptured = true
	return nil
}

func (m *Middleware) ProcessResponse(_ context.Context, text string, _ *pipeline.GatewayContext) (string, error) {
	return text, nil // audit_log records in Finish, not per response field
}

// Record is one audit line. MessageCount and Stream are absent when a
// middleware ahead of audit_log blocked the request before its request phase
// ran. Upstream is absent when no upstream answered. Token counts are
// present only when the upstream reported usage.
type Record struct {
	Time             string `json:"time"`
	RequestID        string `json:"request_id"`
	ClientID         string `json:"client_id"`
	Authenticated    bool   `json:"authenticated"`
	SourceIP         string `json:"source_ip"`
	Model            string `json:"model"`
	MessageCount     *int   `json:"message_count,omitempty"`
	Stream           *bool  `json:"stream,omitempty"`
	Upstream         string `json:"upstream,omitempty"`
	Status           int    `json:"status"`
	LatencyMS        int64  `json:"latency_ms"`
	PromptTokens     *int   `json:"prompt_tokens,omitempty"`
	CompletionTokens *int   `json:"completion_tokens,omitempty"`
	Blocked          bool   `json:"blocked"`
	BlockMiddleware  string `json:"block_middleware,omitempty"`
	BlockDirection   string `json:"block_direction,omitempty"`
}

func newRecord(gctx *pipeline.GatewayContext, now time.Time) Record {
	r := Record{
		Time:             now.UTC().Format(time.RFC3339Nano),
		RequestID:        gctx.RequestID,
		ClientID:         chatmodel.SanitizeClientID(gctx.ClientID),
		Authenticated:    gctx.Authenticated,
		SourceIP:         gctx.SourceIP,
		Model:            gctx.Scratch.RequestModel,
		Upstream:         gctx.ServedBy,
		Status:           gctx.Status,
		PromptTokens:     gctx.Scratch.ActualPromptTokens,
		CompletionTokens: gctx.Scratch.ActualComplTokens,
		Blocked:          gctx.Blocked,
		BlockMiddleware:  gctx.BlockMiddleware,
		BlockDirection:   gctx.BlockDirection,
	}
	if !gctx.StartedAt.IsZero() {
		r.LatencyMS = now.Sub(gctx.StartedAt).Milliseconds()
	}
	if gctx.Scratch.AuditCaptured {
		count, stream := gctx.Scratch.MessageCount, gctx.Scratch.Stream
		r.MessageCount, r.Stream = &count, &stream
	}
	return r
}

// Finish writes the request's record to the log and, if configured, the
// audit file.
func (m *Middleware) Finish(_ context.Context, gctx *pipeline.GatewayContext) error {
	r := newRecord(gctx, time.Now())
	slog.Info("audit", "request_id", r.RequestID, "client_id", r.ClientID, "model", r.Model,
		"upstream", r.Upstream, "status", r.Status, "latency_ms", r.LatencyMS, "blocked", r.Blocked,
		"block_middleware", r.BlockMiddleware, "source_ip", r.SourceIP)
	if m.logFile == "" {
		return nil
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.append(append(line, '\n'))
}

// append writes line to the audit file, opening it on first use. After a
// write error the handle is dropped so the next request reopens the file
// (e.g. after it was removed).
func (m *Middleware) append(line []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("audit log is closed")
	}
	if m.f == nil {
		if dir := filepath.Dir(m.logFile); dir != "." {
			if err := os.MkdirAll(dir, dirMode); err != nil {
				return err
			}
		}
		f, err := m.openFile(m.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return err
		}
		m.f = f
	}
	if _, err := m.f.Write(line); err != nil {
		_ = m.f.Close()
		m.f = nil
		return err
	}
	return nil
}

// Close closes the audit file. Later records go only to the log.
func (m *Middleware) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.f == nil {
		return nil
	}
	err := m.f.Close()
	m.f = nil
	return err
}
