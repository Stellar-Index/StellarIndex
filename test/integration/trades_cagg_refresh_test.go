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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	if !strings.Contains(out, "trades-cagg-refresh: refreshed [61000000,61001000]") || !strings.Contains(out, "drift-windows=8\n") {
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

// TestTradesCAGGRefresh_RebuildsDroppedMinuteRowsBeforeTheTwaps pins the
// migration-0156 hazard on real TimescaleDB: prices_1m's retention drops
// minute chunks without an invalidation, and twap_1d is materialised from
// prices_1m. The refresh must force-rebuild prices_1m over every day
// twap_1d re-materialises, or twap_1d is recomputed from the missing
// minute rows and loses them. While the policy is armed, the twaps are
// refused.
func TestTradesCAGGRefresh_RebuildsDroppedMinuteRowsBeforeTheTwaps(t *testing.T) {
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
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	day10 := time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC)
	// Outside the rewritten ledger, same twap_1d bucket.
	seedCAGGTrade(t, ctx, db, 60_999_000, 0, day10.Add(6*time.Hour), 5)
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, day10.Add(12*time.Hour), 10)
	exec(`CALL refresh_continuous_aggregate('prices_1m', '2025-03-01'::timestamptz, '2025-04-01'::timestamptz)`)
	exec(`CALL refresh_continuous_aggregate('twap_1d', '2025-03-01'::timestamptz, '2025-04-01'::timestamptz)`)
	if got := twapTradeCount(t, ctx, db, day10); got != 2 {
		t.Fatalf("setup: twap_1d[2025-03-10] trade_count = %d, want 2", got)
	}

	// What an armed 0156 policy does to this old range.
	exec(`SELECT drop_chunks('prices_1m', older_than => INTERVAL '90 days')`)
	var minuteRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM prices_1m WHERE bucket >= '2025-03-01' AND bucket < '2025-04-01'`).Scan(&minuteRows); err != nil {
		t.Fatal(err)
	}
	if minuteRows != 0 {
		t.Fatalf("control: %d prices_1m rows survived drop_chunks — this test proves nothing", minuteRows)
	}

	exec(`DELETE FROM trades WHERE ledger = 61000000`)
	seedCAGGTrade(t, ctx, db, 61_000_000, 1, day10.Add(12*time.Hour), 11)
	run := func() (string, error) {
		return captureStdout(t, func() error {
			return chops.Run([]string{"trades-cagg-refresh", "-config", cfgPath, "-from", "61000000", "-to", "61000000"})
		})
	}
	if out, err := run(); err != nil {
		t.Fatalf("trades-cagg-refresh: %v\n%s", err, out)
	}
	if got := twapTradeCount(t, ctx, db, day10); got != 2 {
		t.Errorf("twap_1d[2025-03-10] trade_count = %d after the refresh, want 2 — twap_1d was re-materialised over minute rows prices_1m was not force-rebuilt over", got)
	}

	exec(`SELECT alter_job(job_id, scheduled => true) FROM timescaledb_information.jobs
	       WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m'`)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `SELECT alter_job(job_id, scheduled => false) FROM timescaledb_information.jobs
		       WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m'`)
	})
	if out, err := run(); err == nil || !strings.Contains(err.Error(), "retention policy is armed") {
		t.Errorf("armed prices_1m retention: err = %v (stdout %q), want the twaps refused", err, out)
	}
}

// TestTradesPrices1mDrift_FindsWhatTheAggregateStillHolds is the executing
// proof for the check trades-cagg-refresh ends on (#782): on real
// TimescaleDB, prices_1m and `trades` agree exactly once refreshed, and a
// row rewritten, added or deleted behind the aggregate's back is reported
// with both sides, exact, for its pair only.
func TestTradesPrices1mDrift_FindsWhatTheAggregateStillHolds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	h := time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC)
	from, to := h, h.Add(time.Hour)
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, h.Add(90*time.Second), 10)
	seedCAGGTrade(t, ctx, db, 61_000_001, 0, h.Add(59*time.Minute+30*time.Second), 20)
	// Another pair, and a row just past the window: neither may be reported.
	exec(`INSERT INTO trades (source, ledger, tx_hash, op_index, ts, base_asset, quote_asset, base_amount, quote_amount, usd_volume)
	      VALUES ('soroswap', 61000002, repeat('b', 64), 0, '2025-03-10 12:30:00+00', 'crypto:BTC', 'fiat:USD', 1, 60000, 60000)`)
	seedCAGGTrade(t, ctx, db, 61_000_003, 0, to, 40)
	exec(`CALL refresh_continuous_aggregate('prices_1m', '2025-03-10 00:00+00', '2025-03-11 00:00+00')`)

	drift, err := store.TradesPrices1mDrift(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("control: drift over a freshly refreshed window = %+v, want none — this test proves nothing", drift)
	}

	// The rebuild's shape: one row rewritten with a new amount, one added.
	exec(`UPDATE trades SET base_amount = 11, quote_amount = 1.1 WHERE ledger = 61000000`)
	seedCAGGTrade(t, ctx, db, 61_000_000, 1, h.Add(2*time.Minute), 7)
	drift, err = store.TradesPrices1mDrift(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := timescale.TradesPrices1mDrift{
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		TradeCount: "3", CAGGCount: "2",
		TradeVolume: "38", CAGGVolume: "30",
		TradeUSD: "3", CAGGUSD: "2",
	}
	if len(drift) != 1 || drift[0] != want {
		t.Fatalf("drift = %+v, want exactly %+v", drift, want)
	}

	// Rows deleted behind the aggregate: the pair is gone from trades only.
	exec(`DELETE FROM trades WHERE base_asset = 'crypto:BTC'`)
	drift, err = store.TradesPrices1mDrift(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 2 || drift[0].BaseAsset != "crypto:BTC" || drift[0].TradeCount != "0" || drift[0].CAGGCount != "1" ||
		drift[0].TradeVolume != "0" || drift[0].CAGGVolume != "1" {
		t.Fatalf("drift after a delete = %+v, want crypto:BTC with trades n=0 vol=0 against prices_1m n=1 vol=1, then native", drift)
	}

	if _, err := store.TradesPrices1mDrift(ctx, from.Add(30*time.Second), to); err == nil {
		t.Errorf("a window that does not start on a minute was accepted; its rows and buckets would not line up")
	}
}

func twapTradeCount(t *testing.T, ctx context.Context, db *sql.DB, bucket time.Time) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT coalesce(sum(trade_count), 0) FROM twap_1d WHERE bucket = $1`, bucket).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
