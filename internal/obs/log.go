package obs

import (
	"log/slog"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// NewLogger builds the shared *slog.Logger from the [obs] block (LogLevel, LogFormat), tagged with a `binary` attribute, writing to stderr.
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
