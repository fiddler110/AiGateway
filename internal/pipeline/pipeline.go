package pipeline

import (
	"context"
	"fmt"
	"log/slog"

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
// only participates in the request direction (e.g. audit_log).
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

// RunResponseAccountingOnly runs every middleware's ProcessResponse in
// order WITHOUT breaking on a block: used by passthrough-mode streaming,
// where response bytes have already reached the client, so DLP middleware
// can flag but never block, and later accounting middleware (cost_tracker,
// token_counter) must still get a chance to finalize even if an earlier
// middleware set gctx.Blocked. BlockMiddleware/BlockDirection are still
// recorded (once) for logging/metrics purposes.
func (p *Pipeline) RunResponseAccountingOnly(ctx context.Context, text string, gctx *GatewayContext) (string, error) {
	for _, e := range p.Entries {
		newText, err := e.MW.ProcessResponse(ctx, text, gctx)
		if err != nil {
			if e.FailOpen {
				slog.Warn("middleware failed open", "middleware", e.MW.Name(), "err", err)
				continue
			}
			return text, fmt.Errorf("middleware %s failed closed: %w", e.MW.Name(), err)
		}
		text = newText
		if gctx.Blocked && gctx.BlockMiddleware == "" {
			gctx.BlockMiddleware = e.MW.Name()
			gctx.BlockDirection = "response"
		}
	}
	return text, nil
}

// RunResponse executes the response-phase pipeline over one piece of
// response text (one message choice), mirroring Run's fail-open/closed and
// break-on-block semantics but in the response direction.
func (p *Pipeline) RunResponse(ctx context.Context, text string, gctx *GatewayContext) (string, error) {
	for _, e := range p.Entries {
		newText, err := e.MW.ProcessResponse(ctx, text, gctx)
		if err != nil {
			if e.FailOpen {
				slog.Warn("middleware failed open", "middleware", e.MW.Name(), "err", err)
				continue
			}
			return text, fmt.Errorf("middleware %s failed closed: %w", e.MW.Name(), err)
		}
		text = newText
		if gctx.Blocked {
			if gctx.BlockMiddleware == "" {
				gctx.BlockMiddleware = e.MW.Name()
				gctx.BlockDirection = "response"
			}
			break
		}
	}
	return text, nil
}
