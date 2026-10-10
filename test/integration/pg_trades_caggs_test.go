//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCAGGHistoryDetector executes the data-freshness watchdog's
// emptied-continuous-aggregate SQL — the SHIPPED bytes, read out of
// configs/ansible/roles/archival-node/files/data-freshness.sh — against
// a real TimescaleDB carrying the real migrations.
//
// The state under test is the one migrations 0115 and 0147 leave behind
// and the one a fresh database applying them from zero starts in: the
// nine price/TWAP views recreated WITH NO DATA, with each view's own
// refresh policy re-filling only its trailing start_offset sliver. Every
// newest-bar signal reads green there — last refresh, bar age, the
// ADR-0033 verdict — while the API serves no OHLC, no chart and no
// since-inception history for the whole back-history.
//
// Two properties are pinned:
//
//  1. every one of the nine views is judged, not just the two TWAPs;
//  2. the judgement's reference survives the migration that causes the
//     defect. Deriving it from prices_1m (the previous shape) made the
//     detector self-muting: 0147 empties prices_1m too, so the
//     reference collapses to the same sliver as the subject and the
//     gauge publishes a healthy 0 for a database with no history at all.
func TestCAGGHistoryDetector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	// Ten days of trades — the lake the nine views aggregate.
	now := time.Now().UTC()
	for i := range 10 {
		seedDetectorTrade(t, ctx, db, 100+i, now.AddDate(0, 0, -9+i))
	}

	// The post-migration state: the coarse views hold nothing at all,
	// and prices_1m holds only what a trailing-window policy tick would
	// have re-filled since the recreate.
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', $1::timestamptz, $2::timestamptz)`,
		now.Add(-2*time.Hour), now,
	); err != nil {
		t.Fatalf("refresh prices_1m sliver: %v", err)
	}

	t.Run("every emptied view is flagged", func(t *testing.T) {
		got := runFreshnessCAGGDetector(t, ctx, db)
		for _, view := range []string{
			"prices_1m", "prices_15m", "prices_1h", "prices_4h",
			"prices_1d", "prices_1w", "prices_1mo", "twap_1h", "twap_1d",
		} {
			v, ok := got[view]
			if !ok {
				t.Errorf("no history-missing sample for %q — the watchdog does not judge this view, so an emptied %s is invisible", view, view)
				continue
			}
			if v != "1" {
				t.Errorf("history-missing{%s} = %s, want 1 — the view holds none of the 10 days of trades the lake holds", view, v)
			}
		}
	})

	t.Run("a view whose armed retention explains its floor is not flagged", func(t *testing.T) {
		// An ARMED retention policy makes a short history correct, and
		// the detector has to know the difference between "trimmed by
		// policy" and "never re-materialized". Migration 0156 ships one
		// of these disabled on prices_1m; an operator may arm it.
		if _, err := db.ExecContext(ctx,
			`SELECT add_retention_policy('prices_15m', drop_after => INTERVAL '2 days', if_not_exists => true)`,
		); err != nil {
			t.Fatalf("add retention policy: %v", err)
		}
		// Armed, but parked a day out so the job cannot race the test.
		if _, err := db.ExecContext(ctx, `
			SELECT alter_job(job_id, scheduled => true, next_start => now() + INTERVAL '1 day')
			  FROM timescaledb_information.jobs
			 WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_15m'`,
		); err != nil {
			t.Fatalf("arm retention policy: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`CALL refresh_continuous_aggregate('prices_15m', $1::timestamptz, $2::timestamptz)`,
			now.Add(-48*time.Hour), now,
		); err != nil {
			t.Fatalf("refresh prices_15m over its retained window: %v", err)
		}

		got := runFreshnessCAGGDetector(t, ctx, db)
		if v, ok := got["prices_15m"]; !ok || v != "0" {
			t.Errorf("history-missing{prices_15m} = %q (present=%v), want 0 — the view holds everything its armed 2-day retention lets it hold", v, ok)
		}
		if v, ok := got["prices_1h"]; !ok || v != "1" {
			t.Errorf("history-missing{prices_1h} = %q (present=%v), want 1 — no retention explains ITS empty state", v, ok)
		}
	})

	t.Run("a fully materialized view reads healthy", func(t *testing.T) {
		for _, view := range []string{
			"prices_1m", "prices_1h", "prices_4h",
			"prices_1d", "prices_1w", "prices_1mo", "twap_1h", "twap_1d",
		} {
			if _, err := db.ExecContext(ctx,
				fmt.Sprintf(`CALL refresh_continuous_aggregate('%s', NULL, NULL)`, view),
			); err != nil {
				t.Fatalf("refresh %s: %v", view, err)
			}
		}
		got := runFreshnessCAGGDetector(t, ctx, db)
		for view, v := range got {
			if view == "prices_15m" {
				continue // retention-trimmed on purpose by the sub-test above
			}
			if v != "0" {
				t.Errorf("history-missing{%s} = %s, want 0 — every view now holds the whole lake", view, v)
			}
		}
	})
}

// runFreshnessCAGGDetector executes every emptied-CAGG detector block
// in the shipped watchdog and returns view → value. Reading the SQL out
// of the script rather than restating it here is the point: a detector
// that is correct in a test file and absent from the script protects
// nobody.
func runFreshnessCAGGDetector(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, stmt := range freshnessSQLBlocks(t, "_history_missing") {
		rows, err := db.QueryContext(ctx, stmt)
		if err != nil {
			t.Fatalf("execute watchdog block: %v\n--- SQL ---\n%s", err, stmt)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan exposition line: %v", err)
			}
			view, value := parseExpositionSample(t, line)
			out[view] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("watchdog block rows: %v", err)
		}
		_ = rows.Close()
	}
	return out
}

// freshnessSQLBlocks returns the psql heredocs of the shipped
// data-freshness watchdog whose body mentions `needle`.
func freshnessSQLBlocks(t *testing.T, needle string) []string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"configs", "ansible", "roles", "archival-node", "files", "data-freshness.sh")
	b, err := os.ReadFile(path) //nolint:gosec // fixed in-repo path
	if err != nil {
		t.Fatalf("read watchdog: %v", err)
	}
	var blocks []string
	var cur []string
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case !inBlock && strings.HasSuffix(line, "<<'SQL'"):
			inBlock = true
			cur = nil
		case inBlock && line == "SQL":
			inBlock = false
			block := strings.Join(cur, "\n")
			if strings.Contains(block, needle) {
				blocks = append(blocks, block)
			}
		case inBlock:
			cur = append(cur, line)
		}
	}
	if len(blocks) == 0 {
		t.Fatalf("no SQL block in data-freshness.sh emits %q — nothing detects an emptied continuous aggregate", needle)
	}
	return blocks
}

// parseExpositionSample splits `name{view="x"} 1` into ("x", "1").
func parseExpositionSample(t *testing.T, line string) (string, string) {
	t.Helper()
	open := strings.Index(line, `{view="`)
	closeIdx := strings.Index(line, `"}`)
	space := strings.LastIndex(line, " ")
	if open < 0 || closeIdx < 0 || space < 0 || space < closeIdx {
		t.Fatalf("un-parseable exposition line %q", line)
	}
	return line[open+len(`{view="`) : closeIdx], strings.TrimSpace(line[space+1:])
}

func seedDetectorTrade(t *testing.T, ctx context.Context, db *sql.DB, nonce int, ts time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ('sdex', $1, $2, 0, $3, 'native', 'fiat:USD', 1::numeric, 1::numeric, 1::numeric)`,
		80_000_000+nonce, fmt.Sprintf("%064x", 800_000+nonce), ts,
	); err != nil {
		t.Fatalf("seed detector trade %d: %v", nonce, err)
	}
}

// wantRealtimeCAGGs is the exact set of views the migrations leave
// real-time (0069), sorted. Each must also be allowed by
// timescale.RealTimeCAGGAllowed, the list the runtime readiness check uses.
// 0187 recreates pools_per_source_1h (real-time since 0076) materialized-only;
// the operator restores its real-time tail once it has been refreshed.
var wantRealtimeCAGGs = []string{"source_volume_1h"}

// pinnedCAGGs are the served price / TWAP / oracle / supply / DEX-volume
// views whose unguarded readers (the catalogue snapshot, /v1/markets, the
// DEX pages, FX resolution, RWA daily history) depend on the view exposing
// CLOSED buckets only. Listed so the test fails if one is renamed away.
var pinnedCAGGs = []string{
	"prices_1m", "prices_15m", "prices_1h", "prices_4h",
	"prices_1d", "prices_1w", "prices_1mo",
	"twap_1h", "twap_1d",
	"oracle_prices_1m", "oracle_prices_15m", "oracle_prices_1h", "oracle_prices_4h",
	"oracle_prices_1d", "oracle_prices_1w", "oracle_prices_1mo",
	"supply_1d", "dex_volume_by_pair_1d",
}

// TestCAGGMaterializedOnlyPinned pins ADR-0015's closed-bucket invariant at
// the schema: every served CAGG outside realtimeCAGGs is materialized_only,
// whatever the TimescaleDB default was when it was created and whatever an
// earlier flip left behind. Before 0172 the property held only because the
// current image defaults it on and 0115/0126/0147 carried the prior value
// forward, so a view that was ever real-time stayed real-time.
func TestCAGGMaterializedOnlyPinned(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 164)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Simulate a pre-2.13 TimescaleDB (real-time aggregation on by default)
	// or a 0069-style flip on a price view: every served view is real-time
	// going into the remaining migrations.
	for _, v := range pinnedCAGGs {
		q := fmt.Sprintf(`ALTER MATERIALIZED VIEW %s SET (timescaledb.materialized_only = false)`, v)
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("flip %s to real-time: %v", v, err)
		}
	}

	applyMigrations(t, dsn)

	got := caggMaterializedOnly(t, ctx, db)
	for _, v := range pinnedCAGGs {
		mo, ok := got[v]
		if !ok {
			t.Errorf("continuous aggregate %s is missing after migrating", v)
			continue
		}
		if !mo {
			t.Errorf("%s: materialized_only = false after migrating; its readers would serve the open bucket", v)
		}
	}
	var realtime []string
	for v, mo := range got {
		if !mo && !timescale.RealTimeCAGGAllowed(v) {
			t.Errorf("%s: materialized_only = false and not allowed by timescale.RealTimeCAGGAllowed", v)
		}
		if !mo {
			realtime = append(realtime, v)
		}
	}
	sort.Strings(realtime)
	if !slices.Equal(realtime, wantRealtimeCAGGs) {
		t.Errorf("real-time CAGGs = %v, want exactly %v", realtime, wantRealtimeCAGGs)
	}

	assertZeroLegOnlyCountsInPrices(t, ctx, db)
	assertClosedBucketReadiness(t, ctx, dsn, db)
}

// assertZeroLegOnlyCountsInPrices inserts a zero-quote fill through the
// migrated schema (no CHECK left to reject it) beside a real one: until a
// refresh neither reaches materialized-only prices_1m, and after it the
// zero-leg row adds to trade_count but never to a price.
func assertZeroLegOnlyCountsInPrices(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	p := ohlcDustPair{base: "MOZL-" + priceableIssuer, quote: "native"}
	seed(t, db, ctx, p, []seedTrade{
		{off: 5 * time.Second, base: "10000000", quote: "50000000", usd: "1000"},
		{off: 50 * time.Second, base: "5000000000", quote: "0", usd: "1000"},
	}, t0)

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1m WHERE base_asset = $1 AND quote_asset = $2`,
		p.base, p.quote).Scan(&n); err != nil {
		t.Fatalf("count prices_1m before refresh: %v", err)
	}
	if n != 0 {
		t.Fatalf("prices_1m holds %d row(s) for an unrefreshed bucket; materialized_only is not in effect", n)
	}
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', $1::timestamptz, $2::timestamptz)`,
		t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil {
		t.Fatalf("refresh prices_1m over a zero-leg row: %v", err)
	}
	got := readPriceableRow(t, ctx, db, "1m", p, t0)
	assertNumeric(t, "vwap", got.vwap, "5")
	assertNumeric(t, "last_price", got.last, "5")
	assertNumeric(t, "low_price", got.low, "5")
	if got.tradeCount != 2 {
		t.Errorf("trade_count = %d, want 2 — the zero-leg row counts as a trade", got.tradeCount)
	}
}

// assertClosedBucketReadiness drives the runtime chokepoint against the
// migrated schema: clean, it is ready; after an out-of-band flip of a price
// and an oracle view (the 0069/0076 answer to "the current bucket is
// invisible"), Store.OpenBucketCAGGs names exactly those views and the
// critical /readyz checker fails, while the allowlisted real-time volume
// counters stay unreported.
func assertClosedBucketReadiness(t *testing.T, ctx context.Context, dsn string, db *sql.DB) {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	checker := v1.NewClosedBucketChecker(store)

	open, err := store.OpenBucketCAGGs(ctx)
	if err != nil {
		t.Fatalf("OpenBucketCAGGs on migrated schema: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("OpenBucketCAGGs on migrated schema = %v, want none", open)
	}
	if err := checker.Ping(ctx); err != nil {
		t.Fatalf("closed-bucket readiness on migrated schema: %v", err)
	}

	flipped := []string{"oracle_prices_1d", "prices_1m"}
	for _, v := range flipped {
		q := fmt.Sprintf(`ALTER MATERIALIZED VIEW %s SET (timescaledb.materialized_only = false)`, v)
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("flip %s to real-time: %v", v, err)
		}
	}
	open, err = store.OpenBucketCAGGs(ctx)
	if err != nil {
		t.Fatalf("OpenBucketCAGGs after flip: %v", err)
	}
	if !slices.Equal(open, flipped) {
		t.Errorf("OpenBucketCAGGs after flip = %v, want %v", open, flipped)
	}
	if err := checker.Ping(ctx); err == nil || !strings.Contains(err.Error(), "oracle_prices_1d, prices_1m") {
		t.Errorf("closed-bucket readiness after flip = %v, want a failure naming both views", err)
	}
}

func caggMaterializedOnly(t *testing.T, ctx context.Context, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT view_name, materialized_only
		  FROM timescaledb_information.continuous_aggregates
		 WHERE view_schema = 'public'`)
	if err != nil {
		t.Fatalf("list continuous aggregates: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var mo bool
		if err := rows.Scan(&name, &mo); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = mo
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no continuous aggregates found — the assertion would be vacuous")
	}
	return out
}

// TestOracleCAGGsMatchCatalog is TestTradesCAGGsMatchCatalog for the
// oracle_updates root: every aggregate built (at any depth) on
// oracle_updates is in timescale.OracleCAGGs, which backfill refreshes
// after each chunk, and nothing else is.
func TestOracleCAGGsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	inCatalog := catalogNames(t, ctx, store, `
		WITH RECURSIVE chain AS (
		    SELECT ca.user_view_name, ca.raw_hypertable_id, ca.parent_mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		    UNION ALL
		    SELECT c.user_view_name, p.raw_hypertable_id, p.parent_mat_hypertable_id
		      FROM chain c
		      JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		)
		SELECT DISTINCT c.user_view_name
		  FROM chain c
		  JOIN _timescaledb_catalog.hypertable h ON h.id = c.raw_hypertable_id
		 WHERE c.parent_mat_hypertable_id IS NULL
		   AND h.table_name = 'oracle_updates'
		 ORDER BY 1`)
	var listed []string
	for _, spec := range timescale.OracleCAGGs {
		listed = append(listed, spec.Name)
		if !timescale.IsRefreshableCAGG(spec.Name) {
			t.Errorf("%s is in OracleCAGGs but RefreshContinuousAggregate refuses it", spec.Name)
		}
	}
	slices.Sort(listed)
	if len(inCatalog) == 0 || !slices.Equal(inCatalog, listed) {
		t.Errorf("oracle_updates-rooted aggregates in the schema: %v, timescale.OracleCAGGs: %v", inCatalog, listed)
	}
}

// TestOracleCAGGsRefreshOverABackfilledRange is the oracle half of
// this on a real TimescaleDB: oracle rows written for a historical
// ledger range are invisible to oracle_prices_1d (created WITH NO DATA,
// policy reach 7 days) until refreshed, and the store now finds that
// range's time span and accepts every oracle_prices_* view.
func TestOracleCAGGsRefreshOverABackfilledRange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	for i, ts := range []time.Time{day1, day1.Add(24 * time.Hour)} {
		u := c.OracleUpdate{
			Source: "reflector-dex", Ledger: uint32(50_000_001 + i),
			TxHash:    "3333333333333333333333333333333333333333333333333333333333333333",
			Timestamp: ts, Asset: c.NativeAsset(), Quote: usdc,
			Price: c.NewAmount(big.NewInt(int64(1_000_000 + i))), Decimals: 14,
		}
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate: %v", err)
		}
	}
	// A raw: row lands in the CAGG too; the day read must never serve it.
	raw, err := c.NewOracleRawAsset("USDT0")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertOracleUpdate(ctx, c.OracleUpdate{
		Source: "reflector-dex", Ledger: 50_000_003,
		TxHash:    "4444444444444444444444444444444444444444444444444444444444444444",
		Timestamp: day1, Asset: raw, Quote: usdc,
		Price: c.NewAmount(big.NewInt(1_000_000)), Decimals: 14,
	}); err != nil {
		t.Fatalf("InsertOracleUpdate raw: %v", err)
	}
	if _, _, err := store.LedgerRangeToOracleTimeRange(ctx, 1, 2); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("empty ledger range: err = %v, want ErrNotFound", err)
	}
	from, to, err := store.LedgerRangeToOracleTimeRange(ctx, 50_000_000, 50_000_010)
	if err != nil {
		t.Fatalf("LedgerRangeToOracleTimeRange: %v", err)
	}
	if !from.Equal(day1) || !to.Equal(day1.Add(24*time.Hour)) {
		t.Fatalf("span = [%s, %s], want [%s, %s]", from, to, day1, day1.Add(24*time.Hour))
	}

	readDays := func() int {
		t.Helper()
		pts, err := store.DailyOraclePrices(ctx, []c.Asset{c.NativeAsset(), raw}, usdc,
			day1.Truncate(24*time.Hour), day1.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("DailyOraclePrices: %v", err)
		}
		for _, p := range pts {
			if !p.Asset.IsMapped() {
				t.Fatalf("DailyOraclePrices served unmapped asset %s", p.Asset)
			}
		}
		return len(pts)
	}
	if n := readDays(); n != 0 {
		t.Fatalf("oracle_prices_1d served %d day(s) before any refresh; this test's premise (materialised-only, WITH NO DATA) no longer holds", n)
	}
	for _, spec := range timescale.OracleCAGGs {
		f, tt := timescale.PadRefreshWindow(from, to, spec.MinWindow)
		if err := store.RefreshContinuousAggregate(ctx, spec.Name, f, tt); err != nil {
			t.Fatalf("refresh %s: %v", spec.Name, err)
		}
	}
	if n := readDays(); n != 2 {
		t.Fatalf("oracle_prices_1d serves %d day(s) after the refresh, want 2", n)
	}
}

// TestCAGGBucketsMatchCatalog holds every CAGGSpec.Bucket to the view's
// real time_bucket: RefreshPieces cuts a refresh on that grid, and a cut
// off the grid leaves the bucket it splits unrefreshed.
func TestCAGGBucketsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	for _, spec := range slices.Concat(timescale.TradesCAGGs, timescale.OracleCAGGs) {
		want := fmt.Sprintf("%d seconds", int64(spec.Bucket/time.Second))
		if spec.Bucket == timescale.MonthBucket {
			want = "1 month"
		}
		var width, origin, offset, tz string
		var same bool
		err := store.DB().QueryRowContext(ctx, `
			SELECT bf.bucket_width, bf.bucket_width::interval = $2::interval,
			       coalesce(bf.bucket_origin, ''), coalesce(bf.bucket_offset, ''), coalesce(bf.bucket_timezone, '')
			  FROM _timescaledb_catalog.continuous_agg ca
			  JOIN _timescaledb_catalog.continuous_aggs_bucket_function bf ON bf.mat_hypertable_id = ca.mat_hypertable_id
			 WHERE ca.user_view_name = $1`, spec.Name, want).Scan(&width, &same, &origin, &offset, &tz)
		if err != nil {
			t.Fatalf("%s: bucket function: %v", spec.Name, err)
		}
		if !same || origin != "" || offset != "" || (tz != "" && tz != "UTC") {
			t.Errorf("%s: catalog bucket width=%q origin=%q offset=%q tz=%q, CAGGSpec.Bucket %s (%s) with the default origin",
				spec.Name, width, origin, offset, tz, spec.Bucket, want)
		}
	}
}

// TestRunCAGGRefreshStepPiecesMatchOneCall runs the cut refresh of every
// trades aggregate against TimescaleDB, then one forced CALL over the same
// window, and requires the same materialised rows: no bucket is lost at a
// cut. Trades sit one second either side of every cut, so a cut off the
// view's real grid would split a populated bucket.
func TestRunCAGGRefreshStepPiecesMatchOneCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	from := time.Date(2024, 1, 10, 13, 17, 23, 0, time.UTC)
	plan := timescale.PlanCAGGRefresh(timescale.TradesCAGGs, func(s timescale.CAGGSpec) (time.Time, time.Time) {
		span := max(timescale.CAGGRefreshPieceSpan, s.MinWindow)
		return from, from.Add(span * 5 / 2)
	})

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	var stamps []time.Time
	for _, st := range plan {
		pieces := timescale.RefreshPieces(st)
		if len(pieces) < 2 {
			t.Fatalf("%s over [%s, %s) is one CALL; the test needs a cut", st.View, st.From, st.To)
		}
		for _, p := range pieces[1:] {
			stamps = append(stamps, p.From.Add(-time.Second), p.From.Add(time.Second))
		}
		for ts := st.From; ts.Before(st.To); ts = ts.Add(5 * time.Hour) {
			stamps = append(stamps, ts)
		}
	}
	slices.SortFunc(stamps, func(a, b time.Time) int { return a.Compare(b) })
	stamps = slices.CompactFunc(stamps, func(a, b time.Time) bool { return a.Equal(b) })
	for i, ts := range stamps {
		tr := c.Trade{
			Source: "integ-cagg-pieces", Ledger: uint32(60_000_000 + i), TxHash: fmt.Sprintf("%064x", i),
			Timestamp: ts, Pair: pair,
			BaseAmount: c.NewAmount(big.NewInt(1_000_000_000)), QuoteAmount: c.NewAmount(big.NewInt(int64(12_000_000 + i))),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	matRows := func(view string) int {
		t.Helper()
		var ht string
		if err := store.DB().QueryRowContext(ctx, `
			SELECT format('%I.%I', materialization_hypertable_schema, materialization_hypertable_name)
			  FROM timescaledb_information.continuous_aggregates WHERE view_name = $1`, view).Scan(&ht); err != nil {
			t.Fatalf("%s: materialisation hypertable: %v", view, err)
		}
		var n int
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM `+ht).Scan(&n); err != nil {
			t.Fatalf("%s: count: %v", view, err)
		}
		return n
	}

	cut := map[string]int{}
	for _, st := range plan {
		if err := timescale.RunCAGGRefreshStep(ctx, store, st, false); err != nil {
			t.Fatalf("RunCAGGRefreshStep(%s): %v", st.View, err)
		}
		cut[st.View] = matRows(st.View)
	}
	for _, st := range plan {
		if err := store.RefreshContinuousAggregateForced(ctx, st.View, st.From, st.To); err != nil {
			t.Fatalf("one forced CALL over %s: %v", st.View, err)
		}
		if whole := matRows(st.View); cut[st.View] == 0 || cut[st.View] != whole {
			t.Errorf("%s: %d rows from %d cut CALLs, %d from one CALL over the same window",
				st.View, cut[st.View], len(timescale.RefreshPieces(st)), whole)
		}
	}
}

// TestRefreshContinuousAggregate_PerCallBound proves, on a
// real TimescaleDB: a refresh_continuous_aggregate CALL made through
// the store runs under a per-CALL statement_timeout, and when that
// bound fires the caller gets a typed error naming the view and the
// window, the BACKEND (not the client socket) cancelled the refresh,
// and the pooled connection it ran on is returned healthy with its
// previous statement_timeout restored.
//
// The pool is pinned to ONE connection for the whole test so every
// statement after the timed-out CALL demonstrably rides the same
// connection the CALL was cancelled on — the "does not poison the
// pool" claim is about that connection, and a fresh dial would prove
// nothing.
func TestRefreshContinuousAggregate_PerCallBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.DB().SetMaxOpenConns(1)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc)

	// Two disjoint windows of three one-minute buckets each, well in the
	// past so no policy refresh can race the test's own CALLs.
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	windowA := base
	windowB := base.Add(2 * time.Hour)
	nonce := 0
	for _, start := range []time.Time{windowA, windowB} {
		for i := range 3 {
			nonce++
			tr := mkAPITrade(nonce, start.Add(time.Duration(i)*time.Minute), xlmUSDC, 1_000_000, 500_000)
			if err := store.InsertTrade(ctx, tr); err != nil {
				t.Fatalf("InsertTrade: %v", err)
			}
		}
	}
	// A window must span >= 2 buckets; [start, start+3m] does for prices_1m.
	fromA, toA := timescale.PadRefreshWindow(windowA, windowA.Add(3*time.Minute), 2*time.Minute)
	fromB, toB := timescale.PadRefreshWindow(windowB, windowB.Add(3*time.Minute), 2*time.Minute)

	bucketsIn := func(from, to time.Time) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM prices_1m WHERE bucket >= $1::timestamptz AND bucket < $2::timestamptz`,
			from, to).Scan(&n); err != nil {
			t.Fatalf("count prices_1m: %v", err)
		}
		return n
	}
	sessionTimeout := func() string {
		t.Helper()
		var v string
		if err := store.DB().QueryRowContext(ctx, `SELECT current_setting('statement_timeout')`).Scan(&v); err != nil {
			t.Fatalf("current_setting: %v", err)
		}
		return v
	}

	// ── generous bound: the refresh lands ─────────────────────────────
	if err := store.RefreshContinuousAggregateWithTimeout(ctx, "prices_1m", fromA, toA, time.Minute); err != nil {
		t.Fatalf("refresh under a generous bound: %v", err)
	}
	if got := bucketsIn(fromA, toA); got != 3 {
		t.Fatalf("window A: %d prices_1m buckets after refresh, want 3", got)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after a successful bounded refresh the connection's statement_timeout = %q, want \"0\" (restored)", got)
	}

	// ── 1 ms bound: the typed timeout error, from the backend ─────────
	err = store.RefreshContinuousAggregateWithTimeout(ctx, "prices_1m", fromB, toB, time.Millisecond)
	var tErr *timescale.CAGGRefreshTimeoutError
	if !errors.As(err, &tErr) {
		t.Fatalf("refresh under a 1ms bound: err = %v, want *timescale.CAGGRefreshTimeoutError", err)
	}
	if tErr.View != "prices_1m" || !tErr.From.Equal(fromB) || !tErr.To.Equal(toB) || tErr.Timeout != time.Millisecond {
		t.Errorf("typed error = {View:%s From:%s To:%s Timeout:%s}, want {prices_1m %s %s 1ms}",
			tErr.View, tErr.From, tErr.To, tErr.Timeout, fromB, toB)
	}
	// A PgError carrying the statement_timeout message on the chain
	// proves the SERVER cancelled the statement — the refresh lock is
	// released and the connection is intact. A Go-side deadline
	// (context.DeadlineExceeded) would mean the socket was closed under
	// a still-running backend. The code is asserted as observed: the
	// procedure re-throws the cancellation as XX000 (internal_error),
	// not 57014 — the first run of this test against
	// timescale/timescaledb:2.26.4-pg15 is how that was learned, and a
	// Timescale release that starts surfacing 57014 should fail here so
	// the classifier's comment is corrected with it.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "canceling statement due to statement timeout") {
		t.Fatalf("timeout must be the backend's statement_timeout cancellation; got %v", err)
	}
	if pgErr.Code != "XX000" {
		t.Fatalf("refresh_continuous_aggregate surfaced the cancellation as SQLSTATE %s, not the XX000 the classifier documents — update isStatementTimeoutErr's comment", pgErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must not have come from the Go-side deadline: %v", err)
	}

	// ── the same connection still serves, with its timeout restored ───
	var one int
	if err := store.DB().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 on the pool after the timed-out CALL: %d, %v", one, err)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after the timed-out refresh the connection's statement_timeout = %q, want \"0\" — the CALL's bound leaked onto the pooled connection", got)
	}
	// And the abandoned window is refreshable again: nothing is left
	// holding the view's refresh lock, and the default (window-derived)
	// bound is the path the backfill takes.
	if err := store.RefreshContinuousAggregate(ctx, "prices_1m", fromB, toB); err != nil {
		t.Fatalf("re-refresh of the interrupted window: %v", err)
	}
	if got := bucketsIn(fromB, toB); got != 3 {
		t.Fatalf("window B: %d prices_1m buckets after the re-refresh, want 3", got)
	}
	if got := sessionTimeout(); got != "0" {
		t.Fatalf("after the default-bound refresh the connection's statement_timeout = %q, want \"0\"", got)
	}
}

// Migration 0187 (migrations/0187_price_caggs_priceable_filter.up.sql)
// restricts every price expression in the trades-derived CAGGs to rows with
// both legs > 0 and adds volume_quote / volume_priced. `trades` still
// CHECKs both legs > 0, so the test drops those CHECKs to store the
// zero-leg rows a later migration will admit.

const priceableIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// The operator recipe from 0187's headers
// (0187_price_caggs_priceable_filter.up.sql and
// 0187_price_caggs_priceable_filter.down.sql), run verbatim.
var priceableRefreshRecipe = []string{
	"CALL refresh_continuous_aggregate('prices_1m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_15m', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_4h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1d', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1w', now() - INTERVAL '8 weeks', now(), force => true)",
	"CALL refresh_continuous_aggregate('prices_1mo', now() - INTERVAL '6 months', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1h', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('twap_1d', now() - INTERVAL '7 days', now(), force => true)",
	"CALL refresh_continuous_aggregate('pools_per_source_1h', now() - INTERVAL '7 days', now(), force => true)",
	// The recipe's windows cover no closed week or month bucket for data
	// two days old; materialise the open ones so those views are asserted too.
	"CALL refresh_continuous_aggregate('prices_1w', NULL, NULL)",
	"CALL refresh_continuous_aggregate('prices_1mo', NULL, NULL)",
}

const poolsRealtimeRestore = "ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false)"

var priceableGrains = []string{"1m", "15m", "1h", "4h", "1d", "1w", "1mo"}

func TestPriceCAGGsPriceableFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	assertPriceableSchema(t, ctx, db, true)

	for _, stmt := range []string{
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_base_amount_check`,
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_quote_amount_check`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// One minute inside a closed UTC day inside the recipe's 7-day window.
	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(time.Hour)
	mixed := ohlcDustPair{base: "PRMX-" + priceableIssuer, quote: "native"}
	allZero := ohlcDustPair{base: "PRZZ-" + priceableIssuer, quote: "native"}

	// Real fills at 5 and 6 bracketed by a zero-base row first and a
	// zero-quote row last, both above the notional floor, so an unfiltered
	// expression would divide by zero, open at infinity or close at 0.
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "7000", usd: "5"},
		{off: 10 * time.Second, base: "10000000", quote: "50000000", usd: "1000"},
		{off: 20 * time.Second, base: "20000000", quote: "120000000", usd: "1000"},
		{off: 50 * time.Second, base: "3000000", quote: "0", usd: "5"},
	}, t0)
	seed(t, db, ctx, allZero, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "100", usd: "1"},
		{off: 10 * time.Second, base: "100", quote: "0", usd: ""},
	}, t0)

	refreshAll := func(t *testing.T) {
		t.Helper()
		for _, stmt := range priceableRefreshRecipe {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	refreshAll(t)

	for _, grain := range priceableGrains {
		t.Run("mixed/"+grain, func(t *testing.T) {
			got := readPriceableRow(t, ctx, db, grain, mixed, t0)
			assertNumeric(t, "vwap", got.vwap, "5.666666666666666667")
			assertNumeric(t, "first_price", got.first, "5")
			assertNumeric(t, "last_price", got.last, "6")
			assertNumeric(t, "high_price", got.high, "6")
			assertNumeric(t, "low_price", got.low, "5")
			assertNumeric(t, "twap", got.twap, "5.5")
			assertExact(t, "volume", got.volume, "33000000")
			assertExact(t, "volume_quote", got.volumeQuote, "170007000")
			assertExact(t, "volume_priced", got.volumePriced, "30000000")
			if got.tradeCount != 4 {
				t.Errorf("trade_count = %d, want 4 — zero-leg rows still count as trades", got.tradeCount)
			}
		})
		t.Run("all-zero-leg/"+grain, func(t *testing.T) {
			got := readPriceableRow(t, ctx, db, grain, allZero, t0)
			for col, v := range map[string]sql.NullString{
				"vwap": got.vwap, "twap": got.twap, "first_price": got.first, "last_price": got.last,
				"high_price": got.high, "low_price": got.low, "volume_priced": got.volumePriced,
			} {
				if v.Valid {
					t.Errorf("%s = %s, want NULL for a bucket with no priceable trade", col, v.String)
				}
			}
			assertExact(t, "volume_quote", got.volumeQuote, "100")
			assertExact(t, "volume", got.volume, "100")
		})
	}

	var notional int64
	if err := db.QueryRowContext(ctx, `SELECT notional_trade_count FROM prices_1m
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
		mixed.base, mixed.quote, t0).Scan(&notional); err != nil {
		t.Fatalf("read notional_trade_count: %v", err)
	}
	if notional != 2 {
		t.Errorf("prices_1m.notional_trade_count = %d, want 2 (zero-leg rows are not priceable)", notional)
	}

	for _, view := range []string{"twap_1h", "twap_1d"} {
		t.Run(view, func(t *testing.T) {
			got := readTWAPRow(t, db, ctx, view, mixed, t0)
			assertNumeric(t, view+".twap", got.twap, "5.5")
			if got.sampleCount != 1 {
				t.Errorf("sample_count = %d, want 1", got.sampleCount)
			}
			got = readTWAPRow(t, db, ctx, view, allZero, t0)
			if got.twap.Valid || got.sampleCount != 0 {
				t.Errorf("all-zero-leg: twap = %v, sample_count = %d; want NULL, 0", got.twap, got.sampleCount)
			}
		})
	}

	t.Run("pools_per_source_1h", func(t *testing.T) {
		assertNumeric(t, "bucket_last_price", readPoolsLastPrice(t, ctx, db, mixed, t0), "6")
		if got := readPoolsLastPrice(t, ctx, db, allZero, t0); got.Valid {
			t.Errorf("all-zero-leg bucket_last_price = %s, want NULL", got.String)
		}
		// The header's real-time restore, run once the view is whole.
		if _, err := db.ExecContext(ctx, poolsRealtimeRestore); err != nil {
			t.Fatalf("%s: %v", poolsRealtimeRestore, err)
		}
		assertNumeric(t, "real-time bucket_last_price", readPoolsLastPrice(t, ctx, db, mixed, t0), "6")
	})

	// Down then up again. 0191's down refuses zero-leg rows and 0187's
	// restores views that cannot be refreshed over them, so they are removed
	// before the down and stored again before the second up is refreshed.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM trades WHERE base_amount = 0 OR quote_amount = 0`); err != nil {
		t.Fatalf("delete zero-leg rows: %v", err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"), dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(186); err != nil {
		t.Fatalf("migrate down 0187: %v", err)
	}
	quiesceCAGGRefreshPolicies(t, ctx, db)
	assertPriceableSchema(t, ctx, db, false)
	refreshAll(t)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up 0187 again: %v", err)
	}
	quiesceCAGGRefreshPolicies(t, ctx, db)
	assertPriceableSchema(t, ctx, db, true)
	seed(t, db, ctx, mixed, []seedTrade{
		{off: 5 * time.Second, base: "0", quote: "7000", usd: "5"},
		{off: 50 * time.Second, base: "3000000", quote: "0", usd: "5"},
	}, t0)
	refreshAll(t)
	got := readPriceableRow(t, ctx, db, "1m", mixed, t0)
	assertExact(t, "volume_quote after up/down/up", got.volumeQuote, "170007000")
	assertNumeric(t, "vwap after up/down/up", got.vwap, "5.666666666666666667")
}

type priceableRow struct {
	vwap, twap, first, last, high, low sql.NullString
	volume, volumeQuote, volumePriced  sql.NullString
	tradeCount                         int64
}

func readPriceableRow(t *testing.T, ctx context.Context, db *sql.DB, grain string, p ohlcDustPair, t0 time.Time) priceableRow {
	t.Helper()
	var r priceableRow
	if err := db.QueryRowContext(ctx, `
		SELECT vwap::text, twap::text, first_price::text, last_price::text,
		       high_price::text, low_price::text,
		       volume::text, volume_quote::text, volume_priced::text, trade_count
		  FROM prices_`+grain+`
		 WHERE base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0,
	).Scan(&r.vwap, &r.twap, &r.first, &r.last, &r.high, &r.low,
		&r.volume, &r.volumeQuote, &r.volumePriced, &r.tradeCount); err != nil {
		t.Fatalf("read prices_%s for %s: %v", grain, p.base, err)
	}
	return r
}

func readPoolsLastPrice(t *testing.T, ctx context.Context, db *sql.DB, p ohlcDustPair, t0 time.Time) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT bucket_last_price::text FROM pools_per_source_1h
		 WHERE source = 'sdex' AND base_asset = $1 AND quote_asset = $2 AND bucket <= $3
		 ORDER BY bucket DESC LIMIT 1`,
		p.base, p.quote, t0).Scan(&got); err != nil {
		t.Fatalf("read pools_per_source_1h for %s: %v", p.base, err)
	}
	return got
}

// assertExact compares volumes as exact NUMERIC strings: Σquote has to
// reconstruct to the stroop, not to a float tolerance.
func assertExact(t *testing.T, col string, got sql.NullString, want string) {
	t.Helper()
	if !got.Valid || got.String != want {
		t.Errorf("%s = %v, want exactly %s", col, got, want)
	}
}

// assertPriceableSchema checks the 0187 shape (up) or the restored
// pre-0187 shape (down): the new columns, pools_per_source_1h's
// real-time flag, and 0156's retention policy re-attached disarmed.
func assertPriceableSchema(t *testing.T, ctx context.Context, db *sql.DB, up bool) {
	t.Helper()
	for _, grain := range priceableGrains {
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_name = $1 AND column_name IN ('volume_quote', 'volume_priced')`,
			"prices_"+grain).Scan(&n); err != nil {
			t.Fatalf("columns of prices_%s: %v", grain, err)
		}
		if want := map[bool]int{true: 2, false: 0}[up]; n != want {
			t.Errorf("prices_%s has %d of volume_quote/volume_priced, want %d (up=%v)", grain, n, want, up)
		}
	}
	mo := caggMaterializedOnly(t, ctx, db)
	for _, v := range []string{"prices_1m", "prices_1mo", "twap_1h", "twap_1d"} {
		if !mo[v] {
			t.Errorf("%s: materialized_only = false (up=%v)", v, up)
		}
	}
	if mo["pools_per_source_1h"] != up {
		t.Errorf("pools_per_source_1h materialized_only = %v, want %v", mo["pools_per_source_1h"], up)
	}
	var total, active int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE scheduled)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m'`).Scan(&total, &active); err != nil {
		t.Fatalf("read prices_1m retention job: %v", err)
	}
	if total != 1 || active != 0 {
		t.Errorf("prices_1m retention jobs = %d (scheduled %d), want 1 disarmed (up=%v)", total, active, up)
	}
}

// TestPriceCAGGsAreMaterializedOnly pins the setting most prices_1m readers
// rely on to exclude the in-progress bucket: with real-time aggregation off,
// that bucket is never materialized. Enabling it (as 0069/0076 did for
// sibling CAGGs) must be a deliberate change that audits every reader.
func TestPriceCAGGsAreMaterializedOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rows, err := store.DB().QueryContext(ctx, `
		SELECT view_name, materialized_only
		  FROM timescaledb_information.continuous_aggregates
		 WHERE view_name LIKE 'prices\_%' OR view_name LIKE 'twap\_%'`)
	if err != nil {
		t.Fatalf("query continuous_aggregates: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var name string
		var matOnly bool
		if err := rows.Scan(&name, &matOnly); err != nil {
			t.Fatal(err)
		}
		seen++
		if !matOnly {
			t.Errorf("%s has real-time aggregation enabled; its readers would serve the in-progress bucket", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Seven prices_* grains + twap_1h/twap_1d; fewer means the filter missed.
	if seen < 9 {
		t.Fatalf("checked %d price CAGGs, want >= 9", seen)
	}
}

// TestTrailing24hVolume_ExcludesInProgressMinute enables real-time
// aggregation on prices_1m so the current minute is visible, then asserts
// the trailing-24h volume readers sum only CLOSED buckets (ADR-0015).
// A `bucket < now()` ceiling admits the current minute and fails this.
func TestTrailing24hVolume_ExcludesInProgressMinute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cryptoXLM, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(cryptoXLM, usd)

	// Keep the live trade's minute open for the whole assertion window.
	if s := time.Now().UTC().Second(); s >= 40 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}
	now := time.Now().UTC()
	closedTS := now.Add(-10 * time.Minute).Truncate(time.Minute)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("binance", 1, closedTS, pair, 100_000_000, 30_000_000),
		mkIntegrationTrade("binance", 2, now, pair, 500_000_000, 150_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`ALTER MATERIALIZED VIEW prices_1m SET (timescaledb.materialized_only = false)`); err != nil {
		t.Fatalf("enable real-time aggregation: %v", err)
	}

	asset := cryptoXLM.String()
	closedOnly, all := closedAndLiveVolume(t, ctx, store, asset, closedTS)
	if !(closedOnly > 0 && all > closedOnly) {
		t.Fatalf("fixture did not surface a live minute: closed=%.4f all=%.4f", closedOnly, all)
	}

	got := map[string]string{}
	if got["Volume24hUSDForAsset"], _, err = store.Volume24hUSDForAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if got["SorobanVolume24hUSDForAsset"], _, err = store.SorobanVolume24hUSDForAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	row, err := store.LatestAssetStats(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if row.Volume24hUSD == nil {
		t.Fatal("LatestAssetStats.Volume24hUSD = nil")
	}
	got["LatestAssetStats"] = *row.Volume24hUSD

	if time.Now().UTC().Truncate(time.Minute) != now.Truncate(time.Minute) {
		t.Fatalf("the live minute closed mid-test (started %s); the assertion window is invalid", now)
	}
	for name, v := range got {
		if f := mustFloat(t, v); f < closedOnly-1e-6 || f > closedOnly+1e-6 {
			t.Errorf("%s = %s, want closed-bucket-only %.6f (in-progress minute included: %.6f)",
				name, v, closedOnly, all)
		}
	}
}

// closedAndLiveVolume reads prices_1m's USD volume for the asset in the one
// closed fixture bucket and across every bucket, including the live one.
func closedAndLiveVolume(t *testing.T, ctx context.Context, store *timescale.Store, asset string, closedBucket time.Time) (float64, float64) {
	t.Helper()
	var closedOnly, all float64
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COALESCE(sum(volume_usd) FILTER (WHERE bucket = $2), 0)::float8,
		       COALESCE(sum(volume_usd), 0)::float8
		  FROM prices_1m
		 WHERE base_asset = $1 OR quote_asset = $1`, asset, closedBucket).Scan(&closedOnly, &all); err != nil {
		t.Fatalf("ground-truth volume: %v", err)
	}
	return closedOnly, all
}

// TestPrices1mStartOffsetWidened executes migration 0165 up and down.
// prices_1m's refresh lookback must exceed the ch-live-catchup period
// (deploy/systemd/ch-live-catchup.timer, 10 minutes): trades drained after a
// lake-hole stall carry ledger-close timestamps up to that old, and a
// shorter lookback never re-aggregates their buckets.
func TestPrices1mStartOffsetWidened(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	const (
		before = 5 * time.Minute  // as 0147 left it
		want   = 15 * time.Minute // 0165
	)

	applyMigrationsUpTo(t, dsn, 164)
	if got := prices1mStartOffset(t, ctx, db); got != before {
		t.Fatalf("pre-0165 prices_1m start_offset = %s, want %s", got, before)
	}

	applyMigrationsUpTo(t, dsn, 165)
	got := prices1mStartOffset(t, ctx, db)
	if got != want {
		t.Fatalf("prices_1m start_offset after 0165 = %s, want %s", got, want)
	}
	if catchUpPeriod := 10 * time.Minute; got <= catchUpPeriod {
		t.Fatalf("prices_1m start_offset = %s, must exceed the %s ch-live-catchup period", got, catchUpPeriod)
	}

	applyMigrationsUpTo(t, dsn, 164)
	if got := prices1mStartOffset(t, ctx, db); got != before {
		t.Fatalf("prices_1m start_offset after 0165 down = %s, want %s", got, before)
	}
}

// prices1mStartOffset reads prices_1m's refresh-policy start_offset,
// requiring exactly one refresh policy on the view.
func prices1mStartOffset(t *testing.T, ctx context.Context, db *sql.DB) time.Duration {
	t.Helper()
	var jobs int
	var seconds int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*),
		       coalesce(max(extract(epoch FROM (j.config->>'start_offset')::interval))::bigint, -1)
		  FROM timescaledb_information.jobs j
		  JOIN timescaledb_information.continuous_aggregates c
		    -- Refresh jobs name the cagg view on TimescaleDB 2.26 and its
		    -- materialization hypertable on older 2.x; match either.
		    ON j.hypertable_name IN (c.view_name, c.materialization_hypertable_name)
		 WHERE c.view_name = 'prices_1m'
		   AND j.proc_name = 'policy_refresh_continuous_aggregate'`).Scan(&jobs, &seconds)
	if err != nil {
		t.Fatalf("read prices_1m refresh policy start_offset: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("prices_1m has %d refresh policies, want 1", jobs)
	}
	return time.Duration(seconds) * time.Second
}

// TestProtocolEventRollup_RoundTrip proves the protocol-event rollup end-to-end:
// RefreshProtocolEventCounts folds the trailing-24h census into
// protocol_events_24h, CountRecentEventsBySource reads it back, only
// recent trades count, the refresh is idempotent, and a source that
// ages out of the 24h window is pruned on the next pass.
func TestProtocolEventRollup_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := c.NewCryptoAsset("XLM")
	usdc, _ := c.NewCryptoAsset("USDC")
	pair, _ := c.NewPair(xlm, usdc)

	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	old := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Second)

	// 3 recent sdex trades + 1 recent soroswap trade + 1 old aquarius
	// trade (outside the 24h window → must not count).
	seed := []c.Trade{
		mkIntegrationTrade("sdex", 1, recent, pair, 100, 100),
		mkIntegrationTrade("sdex", 2, recent, pair, 100, 100),
		mkIntegrationTrade("sdex", 3, recent, pair, 100, 100),
		mkIntegrationTrade("soroswap", 4, recent, pair, 100, 100),
		mkIntegrationTrade("aquarius", 5, old, pair, 100, 100),
	}
	for _, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts: %v", err)
	}

	counts, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource: %v", err)
	}
	if counts["sdex"] != 3 {
		t.Errorf("sdex = %d, want 3", counts["sdex"])
	}
	if counts["soroswap"] != 1 {
		t.Errorf("soroswap = %d, want 1", counts["soroswap"])
	}
	// aquarius is summed from its non-trade tables too, so like blend it is
	// always present; the > 24h-old trade must still not count.
	if counts["aquarius"] != 0 {
		t.Errorf("aquarius = %d, want 0 (trade is > 24h old)", counts["aquarius"])
	}

	// Idempotent: a second refresh yields identical counts.
	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts (2nd): %v", err)
	}
	counts2, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource (2nd): %v", err)
	}
	if counts2["sdex"] != 3 || counts2["soroswap"] != 1 {
		t.Errorf("after 2nd refresh sdex=%d soroswap=%d, want 3/1", counts2["sdex"], counts2["soroswap"])
	}

	// Prune: a stale sentinel row (a source no longer counted this pass)
	// must be dropped by the next refresh, while live sources survive.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO protocol_events_24h (source, events_24h, computed_at)
		 VALUES ('zzz_stale_source', 999, now() - interval '1 hour')`); err != nil {
		t.Fatalf("insert stale sentinel: %v", err)
	}
	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts (3rd): %v", err)
	}
	counts3, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource (3rd): %v", err)
	}
	if _, ok := counts3["zzz_stale_source"]; ok {
		t.Errorf("stale sentinel still present after refresh, want pruned")
	}
	if counts3["sdex"] != 3 || counts3["soroswap"] != 1 {
		t.Errorf("live sources changed across prune: sdex=%d soroswap=%d, want 3/1", counts3["sdex"], counts3["soroswap"])
	}
}

// TestRollupRefresh_ZeroRowPassKeepsLastGood runs the three rollup refreshes
// against a database whose upstream (prices_1m, trades) is EMPTY — the
// state a CAGG rebuilt WITH NO DATA or a stalled refresh policy leaves. The
// pass must keep every last-good row still inside its rollup's validity
// bound and drop only the rows past it, instead of deleting the whole
// served table on its now()-stamped prune.
func TestRollupRefresh_ZeroRowPassKeepsLastGood(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	for _, q := range []string{
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at) VALUES
		   ('ZZZ-FRESH', 12345, now() - interval '5 minutes'),
		   ('ZZZ-EXPIRED', 999, now() - interval '25 hours')`,
		`INSERT INTO asset_price_snapshot (asset_id, price_usd, computed_at) VALUES
		   ('ZZZ-FRESH', 0.1234567, now() - interval '5 minutes'),
		   ('ZZZ-EXPIRED', 7.5, now() - interval '16 minutes')`,
		`INSERT INTO asset_volume_character
		 (asset_id, window_days, volume_usd, distinct_makers, distinct_takers,
		  top_account_pair_vol_share, self_cross_share, issuer_side_share,
		  market_styled_share, is_market_styled, character, computed_at)
		 VALUES
		   ('ZZZ-FRESH', 14, 1, 0, 0, 0, 0, 0, 0, false, 'market', now() - interval '1 hour'),
		   ('ZZZ-EXPIRED', 14, 1, 0, 0, 0, 0, 0, 0, false, 'market', now() - interval '15 days')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}

	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}
	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}

	for _, tc := range []struct{ table, idsQ, valueQ, want string }{
		{
			"asset_volume_24h",
			`SELECT asset_id FROM asset_volume_24h ORDER BY asset_id`,
			`SELECT vol_usd::text FROM asset_volume_24h WHERE asset_id = 'ZZZ-FRESH'`,
			"12345",
		},
		{
			"asset_price_snapshot",
			`SELECT asset_id FROM asset_price_snapshot ORDER BY asset_id`,
			`SELECT price_usd::text FROM asset_price_snapshot WHERE asset_id = 'ZZZ-FRESH'`,
			"0.1234567",
		},
		{
			"asset_volume_character",
			`SELECT asset_id FROM asset_volume_character ORDER BY asset_id`,
			`SELECT volume_usd::text FROM asset_volume_character WHERE asset_id = 'ZZZ-FRESH'`,
			"1",
		},
	} {
		assetIDs := rollupAssetIDs(ctx, t, db, tc.idsQ)
		if len(assetIDs) != 1 || assetIDs[0] != "ZZZ-FRESH" {
			t.Errorf("%s after a zero-row pass = %v, want only the in-bound last-good row [ZZZ-FRESH]", tc.table, assetIDs)
			continue
		}
		var got string
		if err := db.QueryRowContext(ctx, tc.valueQ).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.table, err)
		}
		if got != tc.want {
			t.Errorf("%s last-good value = %q, want %q unchanged", tc.table, got, tc.want)
		}
	}
}

func rollupAssetIDs(ctx context.Context, t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("%s scan: %v", q, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", q, err)
	}
	return out
}

// TestSoleWriterCAGGCoverage_MigratedSchema executes the sole-writer
// gate's query against the fully migrated schema: every continuous
// aggregate is listed with the offset its latest migration set and the
// raw table it reads, and the Phase-4 flip is refused on exactly the
// views whose lookback is shorter than the projector's stall bound.
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
	for view, want := range map[string]struct {
		offset time.Duration
		table  string
	}{
		"prices_1m":        {15 * time.Minute, "trades"},        // 0165
		"oracle_prices_1m": {5 * time.Minute, "oracle_updates"}, // 0034
		"twap_1d":          {7 * 24 * time.Hour, "trades"},      // on prices_1m
	} {
		if got := byView[view]; got.StartOffset != want.offset || got.Unbounded || got.Hypertable != want.table {
			t.Errorf("%s = %+v, want start_offset %s over %s", view, got, want.offset, want.table)
		}
	}

	// oracle_prices_1m's 5 minutes is short of the bound, so on this schema
	// the flip is refused.
	err = pipeline.VerifySoleWriterCAGGCoverage(ctx, store, pipeline.SinkModeSkipProjected)
	if !errors.Is(err, pipeline.ErrSoleWriterCAGGWindow) || !strings.Contains(err.Error(), "oracle_prices_1m (start_offset 5m0s)") {
		t.Fatalf("gate err = %v, want ErrSoleWriterCAGGWindow naming oracle_prices_1m", err)
	}
	// No aggregate reads a sep41 or rozo table, and the dispatcher still feeds
	// oracle_updates live in Phase 3, so that mode starts.
	if err := pipeline.VerifySoleWriterCAGGCoverage(ctx, store, pipeline.SinkModeSkipSoleWriter); err != nil {
		t.Fatalf("Phase-3 gate err = %v, want nil", err)
	}
}

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
		"trades-cagg-refresh: pending hull=[2025-03-12T13:00:00Z,2025-03-12T13:00:00Z] ledgers=[61000501,61000501] catch-up: -force=false -from 61000501 -to 61000501 -write\n",
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
		"trades-cagg-refresh: pending hull=[2025-03-04T13:00:00Z,2025-03-04T13:00:00Z] ledgers=[61000300,61000300] catch-up: -force=false -from 61000300 -to 61000300 -write\n",
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

// TestTradesCAGGRefresh_RematerialisesARewrittenLedgerRange is the
// executing proof for `trades-cagg-refresh`, the step
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
			return chops.Run([]string{"trades-cagg-refresh", "-config", cfgPath, "-from", from, "-to", to, "-write"})
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
			return chops.Run([]string{"trades-cagg-refresh", "-config", cfgPath, "-from", "61000000", "-to", "61000000", "-write"})
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
// proof for the check trades-cagg-refresh ends on: on real
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

// TestTradesCAGGsMatchCatalog holds timescale.TradesCAGGs in lockstep
// with the migrated schema: every continuous aggregate whose ROOT
// hypertable is `trades` (hierarchical ones resolved through their
// parent) must be in the list, and nothing else may be. A migration
// that adds a trades-rooted aggregate without listing it leaves a
// served surface the restamp follow-up and the refresh allow-list
// cannot reach; a stale name in the list would fail at refresh time.
func TestTradesCAGGsMatchCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Walk each aggregate up its parent chain to the raw hypertable it
	// is ultimately built on (twap_* → prices_1m → trades).
	const q = `
		WITH RECURSIVE chain AS (
		    SELECT ca.user_view_name, ca.raw_hypertable_id, ca.parent_mat_hypertable_id
		      FROM _timescaledb_catalog.continuous_agg ca
		    UNION ALL
		    SELECT c.user_view_name, p.raw_hypertable_id, p.parent_mat_hypertable_id
		      FROM chain c
		      JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		)
		SELECT DISTINCT c.user_view_name
		  FROM chain c
		  JOIN _timescaledb_catalog.hypertable h ON h.id = c.raw_hypertable_id
		 WHERE c.parent_mat_hypertable_id IS NULL
		   AND h.table_name = 'trades'
		 ORDER BY 1
	`
	rows, err := store.DB().QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("catalog query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var inCatalog []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		inCatalog = append(inCatalog, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(inCatalog) == 0 {
		t.Fatal("catalog walk found no trades-rooted continuous aggregate — the query, not the list, is broken")
	}

	var listed []string
	for _, c := range timescale.TradesCAGGs {
		listed = append(listed, c.Name)
	}
	sort.Strings(listed)

	catalogSet := map[string]bool{}
	for _, n := range inCatalog {
		catalogSet[n] = true
	}
	listedSet := map[string]bool{}
	for _, n := range listed {
		if listedSet[n] {
			t.Errorf("TradesCAGGs lists %s twice", n)
		}
		listedSet[n] = true
		if !catalogSet[n] {
			t.Errorf("TradesCAGGs lists %s, which is not a trades-rooted continuous aggregate in the migrated schema", n)
		}
	}
	for _, n := range inCatalog {
		if !listedSet[n] {
			t.Errorf("continuous aggregate %s is rooted on trades but missing from TradesCAGGs", n)
		}
	}
	t.Logf("trades-rooted aggregates in catalog: %d, listed: %d", len(inCatalog), len(listed))

	// CAGGsOnPrices1m decides which views the trades refresh forces after
	// a forced prices_1m; a hierarchical view missing from it would be
	// refreshed over minute rows a retention drop removed.
	onMinute := catalogNames(t, ctx, store, `
		SELECT c.user_view_name
		  FROM _timescaledb_catalog.continuous_agg c
		  JOIN _timescaledb_catalog.continuous_agg p ON p.mat_hypertable_id = c.parent_mat_hypertable_id
		 WHERE p.user_view_name = 'prices_1m'
		 ORDER BY 1`)
	want := append([]string(nil), timescale.CAGGsOnPrices1m...)
	sort.Strings(want)
	if strings.Join(onMinute, ",") != strings.Join(want, ",") {
		t.Errorf("views built on prices_1m in catalog = %v, timescale.CAGGsOnPrices1m = %v", onMinute, want)
	}
}

func catalogNames(t *testing.T, ctx context.Context, store *timescale.Store, q string) []string {
	t.Helper()
	rows, err := store.DB().QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("catalog query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
