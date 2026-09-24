//go:build integration

package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSoleWriterCAGGCoverage_MigratedSchema executes the Phase-4 pre-flip
// gate's query against the fully migrated schema: every continuous
// aggregate is listed with the offset its latest migration set, and the
// gate refuses the flip on exactly the views whose lookback is shorter
// than the projector's stall bound.
func TestSoleWriterCAGGCoverage_MigratedSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	windows, err := store.CAGGRefreshWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var caggs int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM timescaledb_information.continuous_aggregates`).Scan(&caggs); err != nil {
		t.Fatal(err)
	}
	if len(windows) != caggs || caggs == 0 {
		t.Fatalf("CAGGRefreshWindows returned %d rows for %d continuous aggregates", len(windows), caggs)
	}
	byView := map[string]timescale.CAGGRefreshWindow{}
	for _, w := range windows {
		if !w.HasPolicy {
			t.Errorf("%s: no refresh policy found", w.View)
		}
		byView[w.View] = w
	}
	for view, want := range map[string]time.Duration{
		"prices_1m":        15 * time.Minute, // 0165
		"oracle_prices_1m": 5 * time.Minute,  // 0034
		"twap_1d":          7 * 24 * time.Hour,
	} {
		if got := byView[view]; got.StartOffset != want || got.Unbounded {
			t.Errorf("%s = %+v, want start_offset %s", view, got, want)
		}
	}

	// oracle_prices_1m's 5 minutes is short of the bound, so on this schema
	// the flip is refused.
	err = pipeline.VerifySoleWriterCAGGCoverage(ctx, store, pipeline.SinkModeSkipProjected)
	if !errors.Is(err, pipeline.ErrSoleWriterCAGGWindow) || !strings.Contains(err.Error(), "oracle_prices_1m (start_offset 5m0s)") {
		t.Fatalf("gate err = %v, want ErrSoleWriterCAGGWindow naming oracle_prices_1m", err)
	}
	if err := pipeline.VerifySoleWriterCAGGCoverage(ctx, store, pipeline.SinkModeSkipSoleWriter); err != nil {
		t.Fatalf("Phase-3 mode must never be gated, err = %v", err)
	}
}
