//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
)

// TestTradesCAGGRefresh_RematerialisesARewrittenLedgerRange is the
// executing proof for `trades-cagg-refresh` (#782), the step
// scripts/ops/ch-rebuild-projected.sh now runs after each window whose
// trades it rewrote. On real TimescaleDB it pins that:
//
//  1. a rewrite of historical trades is NOT visible through prices_1d
//     until something refreshes it (the defect, as a control);
//  2. the command refreshes the aggregates over the ledger range's time
//     span, INCLUDING the day buckets the range's first and last rows sit
//     in — Timescale refreshes only buckets wholly inside the window, so
//     an unpadded [min(ts), max(ts)] would leave both edges stale;
//  3. the monthly rung and the prices_1m-derived twap_1d follow;
//  4. a range with no trades is refused, not silently skipped.
func TestTradesCAGGRefresh_RematerialisesARewrittenLedgerRange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	day := func(d, h int) time.Time { return time.Date(2025, 3, d, h, 0, 0, 0, time.UTC) }
	// Outside the rewritten range, but in the same day bucket as its first row.
	seedCAGGTrade(t, ctx, db, 60_999_000, 0, day(10, 6), 5)
	// The pre-repair rows of [61_000_000, 61_001_000].
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, day(10, 12), 10)
	seedCAGGTrade(t, ctx, db, 61_000_500, 0, day(12, 12), 20)
	seedCAGGTrade(t, ctx, db, 61_001_000, 0, day(14, 12), 30)
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', '2025-03-01'::timestamptz, '2025-04-01'::timestamptz)`); err != nil {
		t.Fatalf("materialise prices_1d: %v", err)
	}

	// The script's DELETE + re-derive: same ledgers, correctly keyed rows.
	if _, err := db.ExecContext(ctx, `DELETE FROM trades WHERE ledger BETWEEN 61000000 AND 61001000`); err != nil {
		t.Fatal(err)
	}
	seedCAGGTrade(t, ctx, db, 61_000_000, 1, day(10, 12), 11)
	seedCAGGTrade(t, ctx, db, 61_000_500, 1, day(12, 12), 21)
	seedCAGGTrade(t, ctx, db, 61_001_000, 1, day(14, 12), 31)

	if got := caggVolume(t, ctx, db, "prices_1d", day(10, 0)); got != "15" {
		t.Fatalf("control: prices_1d[2025-03-10] = %s before any refresh, want the pre-repair 15 — the rewrite must be invisible until refreshed, or this test proves nothing", got)
	}

	run := func(from, to string) (string, error) {
		return captureStdout(t, func() error {
			return chops.Run([]string{"trades-cagg-refresh", "-config", cfgPath, "-from", from, "-to", to})
		})
	}
	out, err := run("61000000", "61001000")
	if err != nil {
		t.Fatalf("trades-cagg-refresh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "trades-cagg-refresh: refreshed [61000000,61001000]") {
		t.Errorf("no success line on stdout: %q", out)
	}

	for _, c := range []struct {
		view   string
		bucket time.Time
		want   string
	}{
		{"prices_1d", day(10, 0), "16"}, // first row's bucket: 5 + 11
		{"prices_1d", day(12, 0), "21"},
		{"prices_1d", day(14, 0), "31"}, // last row's bucket
		{"prices_1mo", day(1, 0), "68"}, // 5 + 11 + 21 + 31
	} {
		if got := caggVolume(t, ctx, db, c.view, c.bucket); got != c.want {
			t.Errorf("%s[%s] volume = %s after the refresh, want %s", c.view, c.bucket.Format("2006-01-02"), got, c.want)
		}
	}
	var twapTrades int
	if err := db.QueryRowContext(ctx,
		`SELECT coalesce(sum(trade_count), 0) FROM twap_1d WHERE bucket = $1`, day(10, 0)).Scan(&twapTrades); err != nil {
		t.Fatal(err)
	}
	if twapTrades != 2 {
		t.Errorf("twap_1d[2025-03-10] trade_count = %d, want 2 — the prices_1m-derived rung was not refreshed after its source", twapTrades)
	}

	if out, err := run("70000000", "70000100"); err == nil || !strings.Contains(err.Error(), "no trades in ledgers") {
		t.Errorf("empty range: err = %v (stdout %q), want a refusal naming the missing time span", err, out)
	}
}

func seedCAGGTrade(t *testing.T, ctx context.Context, db *sql.DB, ledger uint32, opIndex int, ts time.Time, base int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ('soroswap', $1, $2, $3, $4, 'native', 'fiat:USD', $5::numeric, $5::numeric / 10, 1::numeric)`,
		ledger, fmt.Sprintf("%064x", ledger), opIndex, ts, base,
	); err != nil {
		t.Fatalf("seed trade at ledger %d: %v", ledger, err)
	}
}

func caggVolume(t *testing.T, ctx context.Context, db *sql.DB, view string, bucket time.Time) string {
	t.Helper()
	var v sql.NullString
	// view is one of this test's literals.
	q := fmt.Sprintf(`SELECT sum(volume)::text FROM %s WHERE bucket = $1 AND base_asset = 'native' AND quote_asset = 'fiat:USD'`, view)
	if err := db.QueryRowContext(ctx, q, bucket).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", view, err)
	}
	return v.String
}
