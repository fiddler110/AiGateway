// Package middleware wires the nine built-in middleware implementations
// into a pipeline.Pipeline based on gateway configuration. This replaces
// the Python reference's dynamic-import-by-name: an unknown middleware name
// in config is a config-time error here, not a runtime import path.
package middleware

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/scottymacleod/aigateway/internal/config"
	"github.com/scottymacleod/aigateway/internal/middleware/audit"
	"github.com/scottymacleod/aigateway/internal/middleware/contentpolicy"
	"github.com/scottymacleod/aigateway/internal/middleware/costtracker"
	"github.com/scottymacleod/aigateway/internal/middleware/piiredactor"
	"github.com/scottymacleod/aigateway/internal/middleware/pseudonymizer"
	"github.com/scottymacleod/aigateway/internal/middleware/ratelimiter"
	"github.com/scottymacleod/aigateway/internal/middleware/secretsscanner"
	"github.com/scottymacleod/aigateway/internal/middleware/tokencounter"
	"github.com/scottymacleod/aigateway/internal/middleware/tokenratelimiter"
	"github.com/scottymacleod/aigateway/internal/pipeline"
)

type constructor func(cfg map[string]any) (pipeline.Middleware, error)

var registry = map[string]constructor{
	"audit_log":             audit.New,
	"content_policy":        contentpolicy.New,
	"context_pseudonymizer": pseudonymizer.New,
	"cost_tracker":          costtracker.New,
	"pii_redactor":          piiredactor.New,
	"rate_limiter":          ratelimiter.New,
	"token_rate_limiter":    tokenratelimiter.New,
	"secrets_scanner":       secretsscanner.New,
	"token_counter":         tokencounter.New,
}

// failOpenDefaults: non-critical accounting middleware fails open (a bug
// there shouldn't take down the gateway); everything else — DLP and
// enforcement middleware — fails closed by default (a bug there must
// hard-fail the request rather than silently skip protection).
// Overridable per-middleware via a `fail_open` key in middleware_config.
var failOpenDefaults = map[string]bool{
	"audit_log":     true,
	"token_counter": true,
	"cost_tracker":  true,
}

// responseDLPMiddleware can rewrite or block model output in the response
// phase. Passthrough streaming sends that output before they run, so they
// could only flag a leaked credential or PII after the client already has
// it. context_pseudonymizer isn't listed: passthrough reverses its fakes
// chunk by chunk.
var responseDLPMiddleware = []string{"secrets_scanner", "pii_redactor", "content_policy"}

// ForcesStreamBuffer reports whether any configured middleware requires
// buffered streaming (P0.17), whatever settings.stream_buffer says.
func ForcesStreamBuffer(cfg *config.Config) bool {
	return slices.ContainsFunc(cfg.Middleware, func(name string) bool {
		return slices.Contains(responseDLPMiddleware, name)
	})
}

var validMiddlewareName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Build constructs a Pipeline from the configured middleware list, in the
// configured order (both request and response phases run in this same
// forward order — see pipeline.Pipeline's doc comment for why that's
// correct for the pseudonymizer's reversal).
func Build(cfg *config.Config) (*pipeline.Pipeline, error) {
	var entries []pipeline.Entry
	for _, name := range cfg.Middleware {
		if !validMiddlewareName.MatchString(name) {
			return nil, fmt.Errorf("middleware: invalid middleware name %q", name)
		}
		ctor, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("middleware: unknown middleware %q (configured in middleware:)", name)
		}
		mwCfg := cfg.MiddlewareConfig[name]

		mw, err := ctor(mwCfg)
		if err != nil {
			return nil, fmt.Errorf("middleware %s: %w", name, err)
		}

		failOpen := failOpenDefaults[name] // defaults to false (fail closed) if absent
		if v, ok := mwCfg["fail_open"].(bool); ok {
			failOpen = v
		}
		entries = append(entries, pipeline.Entry{MW: mw, FailOpen: failOpen})
	}
	return &pipeline.Pipeline{Entries: entries}, nil
}
