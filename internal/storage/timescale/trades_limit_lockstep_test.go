package timescale_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMaxTradesInRangeLimit_matchesConfigCeiling keeps the two copies of
// the trade-scan ceiling in lockstep.
//
// internal/config is a leaf package and must not import a storage
// adapter, so its validator carries the ceiling as a literal. If the two
// ever drift, config either refuses a legal value or — far worse —
// accepts one the reader silently clamps, which is the exact defect this
// validation was added to prevent: raising max_trades_per_window above
// the ceiling does not widen the scan AND permanently blinds the
// orchestrator's truncation detector (cold audit 2026-08-04).
func TestMaxTradesInRangeLimit_matchesConfigCeiling(t *testing.T) {
	cfg := config.Default()
	cfg.Aggregate.MaxTradesPerWindow = timescale.MaxTradesInRangeLimit
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config rejected max_trades_per_window = the store ceiling (%d): %v",
			timescale.MaxTradesInRangeLimit, err)
	}

	cfg.Aggregate.MaxTradesPerWindow = timescale.MaxTradesInRangeLimit + 1
	if err := cfg.Validate(); err == nil {
		t.Fatalf("config ACCEPTED max_trades_per_window = %d, one above the store ceiling (%d) — the reader will clamp it silently and the truncation detector will never fire again",
			timescale.MaxTradesInRangeLimit+1, timescale.MaxTradesInRangeLimit)
	}
}
