// Package secret provides a string wrapper for values that must never be
// logged or persisted (gateway shared keys, per-user gateway keys). It is
// its own package (rather than living in config or server) so both can
// depend on it without an import cycle.
package secret

import "log/slog"

// String wraps a secret value. It implements slog.LogValuer and fmt
// stringing so an accidental log call or %v format prints "[REDACTED]"
// instead of the real value — a durable guardrail that plain Go strings
// don't get for free.
type String string

func (String) LogValue() slog.Value {
	return slog.StringValue("[REDACTED]")
}

func (String) String() string {
	return "[REDACTED]"
}
