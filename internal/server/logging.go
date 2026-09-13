package server

import (
	"log/slog"
	"os"
)

// SetupLogging configures structured JSON logging to stdout, one JSON
// object per line, matching the reference gateway's log shape so existing
// log-aggregation setups keep working. levelName is one of
// debug/info/warn/error (case-insensitive), defaulting to info.
func SetupLogging(levelName string) {
	level := slog.LevelInfo
	switch levelName {
	case "debug", "DEBUG":
		level = slog.LevelDebug
	case "warn", "WARN", "warning", "WARNING":
		level = slog.LevelWarn
	case "error", "ERROR":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

