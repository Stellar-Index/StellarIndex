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

// TestTradesCAGGRefresh_NonForcedRebuildsOnlyInvalidatedBuckets pins
// `trades-cagg-refresh -force=false` and `-size` on real TimescaleDB: a
// trade written late into an already-materialised bucket is sized from the
// invalidation log, then the non-forced run re-materialises that bucket —
// through prices_1m into twap_1d — and leaves every other bucket's rows
// untouched (same xmin), which a forced run would rewrite.
func TestTradesCAGGRefresh_NonForcedRebuildsOnlyInvalidatedBuckets(t *testing.T) {
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
	run := func(args ...string) string {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return chops.Run(tradesCAGGRefreshCmd(cfgPath, args))
		})
		if err != nil {
			t.Fatalf("trades-cagg-refresh %v: %v\n%s", args, err, out)
		}
		return out
	}

	day := func(d, h int) time.Time { return time.Date(2025, 3, d, h, 0, 0, 0, time.UTC) }
	// Older history, so the non-forced run's twap windows start after
	// prices_1m's earliest bucket; otherwise it refuses.
	seedCAGGTrade(t, ctx, db, 60_990_000, 0, time.Date(2025, 2, 20, 12, 0, 0, 0, time.UTC), 5)
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, day(10, 12), 10)
	seedCAGGTrade(t, ctx, db, 61_000_500, 0, day(12, 12), 20)
	seedCAGGTrade(t, ctx, db, 61_001_000, 0, day(14, 12), 30)
	run("-from", "60990000", "-to", "61001000")
	if out := run("-size"); !strings.Contains(out, "trades-cagg-refresh: pending none:") {
		t.Fatalf("control: -size after a forced refresh = %q, want nothing pending", out)
	}

	before := map[string]map[time.Time]string{"prices_1d": matXmins(t, ctx, db, "prices_1d"), "prices_1m": matXmins(t, ctx, db, "prices_1m")}
	for _, b := range []time.Time{day(10, 0), day(12, 0), day(14, 0)} {
		if before["prices_1d"][b] == "" {
			t.Fatalf("control: prices_1d has no row at %s after the forced refresh", b)
		}
	}

	// The late write: below the invalidation threshold, so Timescale logs it.
	seedCAGGTrade(t, ctx, db, 61_000_501, 0, day(12, 13), 7)
	if got := caggVolume(t, ctx, db, "prices_1d", day(12, 0)); got != "20" {
		t.Fatalf("control: prices_1d[03-12] = %s before any refresh, want 20", got)
	}
	size := run("-size")
	for _, want := range []string{
		"trades-cagg-refresh: pending prices_1d ranges=0 span=0s from=- to=- open-ended=2 source-log=1\n",
		"trades-cagg-refresh: pending hull=[2025-03-12T13:00:00Z,2025-03-12T13:00:00Z] ledgers=[61000501,61000501] catch-up: -force=false -from 61000501 -to 61000501\n",
	} {
		if !strings.Contains(size, want) {
			t.Errorf("-size output lacks %q:\n%s", want, size)
		}
	}

	out := run("-force=false", "-from", "61000000", "-to", "61001000")
	if !strings.Contains(out, "trades-cagg-refresh: refreshed [61000000,61001000]") || !strings.Contains(out, " forced=false ") {
		t.Errorf("no non-forced success line: %q", out)
	}

	for _, c := range []struct {
		view   string
		bucket time.Time
		want   string
	}{
		{"prices_1d", day(12, 0), "27"},
		{"prices_1d", day(10, 0), "10"},
		{"prices_1mo", day(1, 0), "67"},
	} {
		if got := caggVolume(t, ctx, db, c.view, c.bucket); got != c.want {
			t.Errorf("%s[%s] volume = %s, want %s", c.view, c.bucket.Format("2006-01-02"), got, c.want)
		}
	}
	var twapTrades int
	if err := db.QueryRowContext(ctx,
		`SELECT coalesce(sum(trade_count), 0) FROM twap_1d WHERE bucket = $1`, day(12, 0)).Scan(&twapTrades); err != nil {
		t.Fatal(err)
	}
	if twapTrades != 2 {
		t.Errorf("twap_1d[03-12] trade_count = %d, want 2: prices_1m's re-materialisation did not reach the twap", twapTrades)
	}

	after := map[string]map[time.Time]string{"prices_1d": matXmins(t, ctx, db, "prices_1d"), "prices_1m": matXmins(t, ctx, db, "prices_1m")}
	if after["prices_1d"][day(12, 0)] == before["prices_1d"][day(12, 0)] {
		t.Error("prices_1d[03-12] row was not rewritten, yet holds the late trade")
	}
	invalidated := map[string]time.Time{"prices_1d": day(12, 0), "prices_1m": day(12, 13)}
	for view, rows := range before {
		for b, xmin := range rows {
			if b.Equal(invalidated[view]) {
				continue
			}
			if after[view][b] != xmin {
				t.Errorf("%s bucket %s was rewritten (xmin %s -> %s); no invalidation names it", view, b.UTC().Format(time.RFC3339), xmin, after[view][b])
			}
		}
	}

	if out := run("-size"); !strings.Contains(out, "trades-cagg-refresh: pending none:") {
		t.Errorf("-size after the catch-up = %q, want nothing pending", out)
	}
}

// TestTradesCAGGRefresh_NonForcedRefusesBelowDroppedPrices1m pins the
// drop-then-disarm case: prices_1m's retention dropped its old minute rows
// and is disarmed again, so the armed check passes, yet a non-forced run
// over that history would recompute twap_1d from the one minute a late
// trade re-materialises. It must refuse, and -size must keep that range
// out of its catch-up line.
func TestTradesCAGGRefresh_NonForcedRefusesBelowDroppedPrices1m(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, run, mustRun, twapTrades := tradesCAGGRefreshHarness(t, ctx)

	old := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	seedCAGGTrade(t, ctx, db, 50_000_000, 0, old, 10)
	seedCAGGTrade(t, ctx, db, 50_000_000, 1, old.Add(30*time.Minute), 10)
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC), 20)
	seedCAGGTrade(t, ctx, db, 61_000_400, 0, time.Date(2025, 3, 5, 12, 0, 0, 0, time.UTC), 30)
	mustRun("-from", "50000000", "-to", "50000000")
	mustRun("-from", "61000000", "-to", "61000400")
	oldDay := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)
	if got := twapTrades(oldDay); got != 2 {
		t.Fatalf("control: twap_1d[2024-01-10] trade_count = %d after the forced refresh, want 2", got)
	}

	// What the armed policy does on its run; the policy itself stays
	// disarmed, as migration 0156 ships it and as the operator leaves it.
	dropPrices1mBefore2024June(t, ctx, db)
	var armed bool
	if err := db.QueryRowContext(ctx, `SELECT coalesce(bool_or(scheduled), false) FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m'`).Scan(&armed); err != nil {
		t.Fatal(err)
	}
	if armed {
		t.Fatal("control: prices_1m's retention policy is armed; this case is the disarmed one")
	}

	// Late trades: one into the dropped history, one above the floor.
	seedCAGGTrade(t, ctx, db, 50_000_001, 0, old.Add(time.Hour), 5)
	seedCAGGTrade(t, ctx, db, 61_000_300, 0, time.Date(2025, 3, 4, 13, 0, 0, 0, time.UTC), 7)

	// The drop logged twap_1d's invalidation from the dropped chunk's start;
	// the late 2024 trade, and the never-refreshed stretch between the two
	// forced runs, sit below the floor too.
	size := mustRun("-size")
	for _, want := range []string{
		"trades-cagg-refresh: pending twap_1d below-floor from=2023-11-09T00:00:00Z to=2025-02-27T23:59:59Z: starts before 2025-03-03T00:00:00Z, " +
			"where twap windows reach below prices_1m's earliest bucket 2025-03-01T12:00:00Z, so -force=false refuses it; refresh it with -force=true\n",
		"trades-cagg-refresh: pending prices_1m below-floor from=2024-01-10T13:00:00Z to=",
		"trades-cagg-refresh: pending hull=[2025-03-04T13:00:00Z,2025-03-04T13:00:00Z] ledgers=[61000300,61000300] catch-up: -force=false -from 61000300 -to 61000300\n",
	} {
		if !strings.Contains(size, want) {
			t.Errorf("-size output lacks %q:\n%s", want, size)
		}
	}
	out, err := run("-force=false", "-from", "50000000", "-to", "50000001")
	if err == nil || !strings.Contains(err.Error(), "the twap_1h window [2024-01-10T10:30:00Z, 2024-01-10T14:30:00Z)") ||
		!strings.Contains(err.Error(), "starts before prices_1m's earliest materialised bucket 2025-03-01T12:00:00Z") {
		t.Errorf("non-forced over the dropped range: err = %v, want a refusal naming the twap window and the floor\n%s", err, out)
	}
	if got := twapTrades(oldDay); got != 2 {
		t.Errorf("twap_1d[2024-01-10] trade_count = %d after the refusal, want 2: it was recomputed from the emptied minute rows", got)
	}

	// The catch-up -size suggested passes the guard.
	if out := mustRun("-force=false", "-from", "61000300", "-to", "61000300"); !strings.Contains(out, " forced=false ") {
		t.Errorf("no non-forced success line for the suggested catch-up: %q", out)
	}

	// The remedy: forced rebuilds prices_1m from trades, then the twaps.
	mustRun("-from", "50000000", "-to", "50000001")
	if got := twapTrades(oldDay); got != 3 {
		t.Errorf("twap_1d[2024-01-10] trade_count = %d after the forced remedy, want 3", got)
	}
}

// TestTradesCAGGRefresh_NonForcedRefusesGapAbovePrices1mFloor pins a
// dropped stretch of prices_1m that sits ABOVE its earliest bucket, once a
// forced run over older history moved that bucket down: the floor no longer
// covers it, so the non-forced run must prove prices_1m against trades over
// the twap buckets it recomputes, and -size must not suggest a catch-up there.
func TestTradesCAGGRefresh_NonForcedRefusesGapAbovePrices1mFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, run, mustRun, twapTrades := tradesCAGGRefreshHarness(t, ctx)

	gapDay := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	seedCAGGTrade(t, ctx, db, 50_000_000, 0, time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC), 10)
	seedCAGGTrade(t, ctx, db, 52_000_000, 0, gapDay.Add(12*time.Hour), 10)
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC), 20)
	mustRun("-from", "50000000", "-to", "61000000")
	if got := twapTrades(gapDay); got != 1 {
		t.Fatalf("control: twap_1d[2024-03-10] trade_count = %d after the forced refresh, want 1", got)
	}
	dropPrices1mBefore2024June(t, ctx, db)
	// The forced remedy for the oldest range moves prices_1m's earliest
	// bucket back below the gap day's still-dropped minutes.
	mustRun("-from", "50000000", "-to", "50000000")

	size := mustRun("-size")
	if want := "trades-cagg-refresh: pending prices_1m gap [2024-03-10T00:00:00Z,2024-03-11T00:00:00Z): "; !strings.Contains(size, want) {
		t.Errorf("-size output lacks %q:\n%s", want, size)
	}
	if strings.Contains(size, "-from 52000000") {
		t.Errorf("-size suggests a non-forced catch-up over the gap:\n%s", size)
	}

	out, err := run("-force=false", "-from", "52000000", "-to", "52000000")
	if err == nil || !strings.Contains(err.Error(), "prices_1m disagrees with trades over [2024-03-10T00:00:00Z,2024-03-11T00:00:00Z)") ||
		!strings.Contains(err.Error(), "-force=true") {
		t.Errorf("non-forced over the gap: err = %v, want a refusal naming the gap and -force=true\n%s", err, out)
	}
	if got := twapTrades(gapDay); got != 1 {
		t.Errorf("twap_1d[2024-03-10] trade_count = %d after the refusal, want 1: it was recomputed from the dropped minute rows", got)
	}

	mustRun("-from", "52000000", "-to", "52000000")
	if got := twapTrades(gapDay); got != 1 {
		t.Errorf("twap_1d[2024-03-10] trade_count = %d after the forced remedy, want 1", got)
	}
	if out := mustRun("-force=false", "-from", "52000000", "-to", "52000000"); !strings.Contains(out, " forced=false ") {
		t.Errorf("non-forced after the forced remedy: no success line: %q", out)
	}
}

// tradesCAGGRefreshHarness migrates a fresh TimescaleDB and returns it, the
// command run with and without failing on error, and twap_1d's trade_count
// at a bucket.
func tradesCAGGRefreshHarness(t *testing.T, ctx context.Context) (
	*sql.DB, func(...string) (string, error), func(...string) string, func(time.Time) int,
) {
	t.Helper()
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
	run := func(args ...string) (string, error) {
		t.Helper()
		return captureStdout(t, func() error {
			return chops.Run(tradesCAGGRefreshCmd(cfgPath, args))
		})
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("trades-cagg-refresh %v: %v\n%s", args, err, out)
		}
		return out
	}
	twapTrades := func(bucket time.Time) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT coalesce(sum(trade_count), 0) FROM twap_1d WHERE bucket = $1`, bucket).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return db, run, mustRun, twapTrades
}

// dropPrices1mBefore2024June does what an armed prices_1m retention policy
// does on its run; the policy itself stays disarmed.
func dropPrices1mBefore2024June(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var dropped int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM drop_chunks('prices_1m', older_than => '2024-06-01'::timestamptz)`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	var oldMinutes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM prices_1m WHERE bucket < '2024-06-01'`).Scan(&oldMinutes); err != nil {
		t.Fatal(err)
	}
	if dropped == 0 || oldMinutes != 0 {
		t.Fatalf("control: drop_chunks dropped %d chunk(s), %d old minute row(s) left; want the 2024 minutes gone", dropped, oldMinutes)
	}
}

// matXmins maps each bucket of view's materialization hypertable to the
// xmin of its row: a re-materialised bucket gets a new one.
func matXmins(t *testing.T, ctx context.Context, db *sql.DB, view string) map[time.Time]string {
	t.Helper()
	var table string
	if err := db.QueryRowContext(ctx, `
		SELECT format('%I.%I', h.schema_name, h.table_name)
		  FROM _timescaledb_catalog.continuous_agg ca
		  JOIN _timescaledb_catalog.hypertable h ON h.id = ca.mat_hypertable_id
		 WHERE ca.user_view_name = $1`, view).Scan(&table); err != nil {
		t.Fatalf("materialization hypertable of %s: %v", view, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT bucket, xmin::text FROM `+table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[time.Time]string{}
	for rows.Next() {
		var b time.Time
		var x string
		if err := rows.Scan(&b, &x); err != nil {
			t.Fatal(err)
		}
		out[b.UTC()] = x
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tradesCAGGRefreshCmd adds -write to every refresh; -size is read-only and
// refuses any other flag.
func tradesCAGGRefreshCmd(cfgPath string, args []string) []string {
	cmd := append([]string{"trades-cagg-refresh", "-config", cfgPath}, args...)
	if len(args) == 1 && args[0] == "-size" {
		return cmd
	}
	return append(cmd, "-write")
}
