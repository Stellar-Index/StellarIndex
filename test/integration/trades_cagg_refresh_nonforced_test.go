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
			return chops.Run(append([]string{"trades-cagg-refresh", "-config", cfgPath}, args...))
		})
		if err != nil {
			t.Fatalf("trades-cagg-refresh %v: %v\n%s", args, err, out)
		}
		return out
	}

	day := func(d, h int) time.Time { return time.Date(2025, 3, d, h, 0, 0, 0, time.UTC) }
	seedCAGGTrade(t, ctx, db, 61_000_000, 0, day(10, 12), 10)
	seedCAGGTrade(t, ctx, db, 61_000_500, 0, day(12, 12), 20)
	seedCAGGTrade(t, ctx, db, 61_001_000, 0, day(14, 12), 30)
	run("-from", "61000000", "-to", "61001000")
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
