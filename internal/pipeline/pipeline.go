package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/scottymacleod/aigateway/internal/chatmodel"
)

// Middleware is the contract every DLP/accounting/policy plugin implements.
// Process runs in the request direction; ProcessResponse runs in the
// response direction, in the SAME configured order as Process (not
// reversed) — e.g. the pseudonymizer's reversal works because it looks up
// its own substitution map in ProcessResponse, not because of an ordering
// trick.
type Middleware interface {
	Name() string
	Process(ctx context.Context, req *chatmodel.ChatRequest, gctx *GatewayContext) error
	ProcessResponse(ctx context.Context, text string, gctx *GatewayContext) (string, error)
}

// NoResponsePhase gives a middleware a free no-op ProcessResponse when it
// only participates in the request direction (e.g. rate_limiter).
type NoResponsePhase struct{}

func (NoResponsePhase) ProcessResponse(_ context.Context, text string, _ *GatewayContext) (string, error) {
	return text, nil
}

// Entry pairs a middleware with its fail-open/fail-closed policy. Defaults
// (set by the caller building the pipeline): audit_log, token_counter, and
// cost_tracker fail OPEN (a bug there shouldn't take down the gateway);
// everything else — secrets_scanner, pii_redactor, content_policy,
// context_pseudonymizer, rate_limiter, token_rate_limiter — fails CLOSED (a
// bug there must hard-fail the request rather than silently skip
// protection). Configurable per-middleware via a `fail_open` config key.
type Entry struct {
	MW       Middleware
	FailOpen bool
}

// Pipeline runs an ordered chain of middleware entries.
type Pipeline struct {
	Entries []Entry
}

// Run executes the request-phase pipeline. On a middleware error: fail-open
// entries log and continue (as if that middleware's effect never happened);
// fail-closed entries propagate the error, which the caller must turn into
// a 500. On the first middleware that sets gctx.Blocked, BlockMiddleware and
// BlockDirection are recorded (once) and the remaining middleware are
// skipped.
func (p *Pipeline) Run(ctx context.Context, req *chatmodel.ChatRequest, gctx *GatewayContext) error {
	for _, e := range p.Entries {
		if err := e.MW.Process(ctx, req, gctx); err != nil {
			if e.FailOpen {
				slog.Warn("middleware failed open", "middleware", e.MW.Name(), "err", err)
				continue
			}
			return fmt.Errorf("middleware %s failed closed: %w", e.MW.Name(), err)
		}
		if gctx.Blocked {
			if gctx.BlockMiddleware == "" {
				gctx.BlockMiddleware = e.MW.Name()
				gctx.BlockDirection = "request"
			}
			break
		}
	}
	return nil
}

// ResponseField is one model-generated string of a response: message
// content, reasoning, refusal, or a string or number inside tool-call
// arguments (P0.16).
type ResponseField struct {
	Text string
	// Derived marks text that is also contained in another field (a value
	// decoded from tool-call arguments whose raw source is its own field),
	// so ResponseAccounting middleware doesn't count it twice.
	Derived bool
}

// ResponseAccounting is implemented by middleware that measure a response
// rather than inspect or rewrite its text (token_counter, cost_tracker).
// The response pipeline calls their ProcessResponse exactly once per
// response, with the text of every non-Derived field concatenated, however
// many fields the response has; the text they return is ignored. Every
// other middleware's ProcessResponse runs once per field.
type ResponseAccounting interface {
	AccountsWholeResponse()
}

// RunResponseAccountingOnly is RunResponse WITHOUT breaking on a block: used
// by passthrough-mode streaming, where response bytes have already reached
// the client, so DLP middleware can flag but never block, and later
// accounting middleware (cost_tracker, token_counter) must still get a
// chance to finalize even if an earlier middleware set gctx.Blocked.
// BlockMiddleware/BlockDirection are still recorded (once) for
// logging/metrics purposes.
func (p *Pipeline) RunResponseAccountingOnly(ctx context.Context, fields []ResponseField, gctx *GatewayContext) ([]ResponseField, error) {
	return p.runResponse(ctx, fields, gctx, false)
}

// RunResponse executes the response-phase pipeline over every field of one
// response (all choices), mirroring Run's fail-open/closed and
// break-on-block semantics in the response direction: each middleware, in
// configured order, processes every field before the next middleware runs,
// and a block from any field stops the pipeline. It returns the rewritten
// fields, index-aligned with fields; fields itself is not modified.
func (p *Pipeline) RunResponse(ctx context.Context, fields []ResponseField, gctx *GatewayContext) ([]ResponseField, error) {
	return p.runResponse(ctx, fields, gctx, true)
}

func (p *Pipeline) runResponse(ctx context.Context, fields []ResponseField, gctx *GatewayContext, stopOnBlock bool) ([]ResponseField, error) {
	out := slices.Clone(fields)
	for _, e := range p.Entries {
		next, err := processFields(ctx, e.MW, out, gctx)
		if err != nil {
			if e.FailOpen {
				// As if this middleware never ran: none of its rewrites apply.
				slog.Warn("middleware failed open", "middleware", e.MW.Name(), "err", err)
				continue
			}
			return out, fmt.Errorf("middleware %s failed closed: %w", e.MW.Name(), err)
		}
		out = next
		if gctx.Blocked {
			if gctx.BlockMiddleware == "" {
				gctx.BlockMiddleware = e.MW.Name()
				gctx.BlockDirection = "response"
			}
			if stopOnBlock {
				break
			}
		}
	}
	return out, nil
}

// Finisher is implemented by middleware that act once a request is over,
// after the response (or error) has been sent, e.g. audit_log's record of
// the served upstream, status, latency, tokens, and block outcome.
type Finisher interface {
	Finish(ctx context.Context, gctx *GatewayContext) error
}

// Finish runs every Finisher in configured order. The handler calls it once
// per request that reached the pipeline: served, blocked, or failed. The
// response is already on the wire, so an error is logged and never changes
// the outcome, whatever the middleware's fail_open setting.
func (p *Pipeline) Finish(ctx context.Context, gctx *GatewayContext) {
	for _, e := range p.Entries {
		f, ok := e.MW.(Finisher)
		if !ok {
			continue
		}
		if err := f.Finish(ctx, gctx); err != nil {
			slog.Warn("middleware finish failed", "middleware", e.MW.Name(), "err", err)
		}
	}
}

// Close releases resources held by middleware that implement io.Closer
// (audit_log's open file). Call it when this pipeline's AppState generation
// is retired, after its in-flight requests have finished.
func (p *Pipeline) Close() error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, e := range p.Entries {
		if c, ok := e.MW.(io.Closer); ok {
			errs = append(errs, c.Close())
		}
	}
	return errors.Join(errs...)
}

// processFields runs one middleware over fields, returning a rewritten copy.
func processFields(ctx context.Context, mw Middleware, fields []ResponseField, gctx *GatewayContext) ([]ResponseField, error) {
	if _, ok := mw.(ResponseAccounting); ok {
		var all strings.Builder
		for _, f := range fields {
			if !f.Derived {
				all.WriteString(f.Text)
			}
		}
		_, err := mw.ProcessResponse(ctx, all.String(), gctx)
		return fields, err
	}
	out := slices.Clone(fields)
	for i := range out {
		text, err := mw.ProcessResponse(ctx, out[i].Text, gctx)
		if err != nil {
			return nil, err
		}
		out[i].Text = text
		if gctx.Blocked {
			break
		}
	}
	return out, nil
}
