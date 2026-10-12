package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The single-host port-collision shift must key off whether the operator set obs.metrics_listen (MetricsListenSet),
// not equality with the indexer's default: an explicit 127.0.0.1:9464 must be honoured verbatim, not rewritten to
// :9465.
func TestMetricsListenAddr_ExplicitDefaultIsHonoured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.ObsConfig{
		MetricsListen:    aggregatorMetricsCollidingDefault,
		MetricsListenSet: true,
	}

	got := metricsListenAddr(cfg, logger)

	if got != aggregatorMetricsCollidingDefault {
		t.Fatalf("metricsListenAddr shifted an explicitly-configured value: got %q, want %q",
			got, aggregatorMetricsCollidingDefault)
	}
}

// Unset (left at Default()'s value) still gets the single-host shift.
func TestMetricsListenAddr_UnsetDefaultShifts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.ObsConfig{
		MetricsListen:    aggregatorMetricsCollidingDefault,
		MetricsListenSet: false,
	}

	got := metricsListenAddr(cfg, logger)

	if got != aggregatorMetricsShiftedAddr {
		t.Fatalf("metricsListenAddr did not shift an unset default: got %q, want %q",
			got, aggregatorMetricsShiftedAddr)
	}
}
