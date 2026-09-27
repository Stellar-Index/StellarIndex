package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// GH-1130: the single-host port-collision shift must key off whether the
// operator actually set obs.metrics_listen (config.ObsConfig.MetricsListenSet),
// never off comparing the value to the indexer's default by equality. An
// operator who explicitly pins the aggregator to 127.0.0.1:9464 — the same
// value the indexer defaults to, e.g. because they run each on a different
// host — must be honoured verbatim, not silently rewritten to :9465.
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
