package obs

import (
	"log/slog"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// NewLogger builds the shared *slog.Logger every binary uses.
//
// Configuration comes from the operator's [obs] block:
//
//   - LogLevel: case-insensitive "debug", "info", "warn" (alias "warning") or "error";
//     anything else falls back to info.
//   - LogFormat: case-insensitive "console" or "text" emits the text handler; anything
//     else (including the default "json") emits the JSON handler.
//
// Every logger carries a `binary` attribute (pass the program basename, e.g.
// "stellarindex-indexer") so the log aggregator can filter per binary.
//
// Output goes to stderr, which systemd captures into the journal; stdout is reserved for
// human-targeted CLI output.
func NewLogger(cfg config.ObsConfig, binaryName string) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "console", "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}

	logger := slog.New(handler)
	if binaryName != "" {
		logger = logger.With("binary", binaryName)
	}
	return logger
}
