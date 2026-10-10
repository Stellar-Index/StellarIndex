//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricelesscoverage"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const zeroLegIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// TestPriceReadersOnZeroLegBuckets runs every prices_* reader that combines
// vwap with a volume over buckets holding zero-leg trades (migration 0187).
// vwap covers only trades with both legs > 0, so a price must be weighted by
// volume_priced and a quote volume read from volume_quote; `vwap * volume`
// counts a zero-quote trade's base at the bucket price and drops a zero-base
// trade's quote.
//
// ZLEG/USDC, all amounts in 1e7 units:
//
//	H1 (t0+10s)  ZLEG/USDC 10/20 · 30/0     USDC/ZLEG 40/10 · 0/5
//	H2 (t0+1h)   ZLEG/USDC 10/40 · 5/0      ZLEG/USDC-SAC 20/20
//
// H1's priceable union is 20 ZLEG for 60 USDC = 3 (2.4 weighted by volume).
func TestPriceReadersOnZeroLegBuckets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	for _, stmt := range []string{
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_base_amount_check`,
		`ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_quote_amount_check`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	const usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	usdcID := "USDC-" + zeroLegIssuer
	zleg := ohlcDustPair{base: "ZLEG-" + zeroLegIssuer, quote: usdcID}
	flip := ohlcDustPair{base: usdcID, quote: zleg.base}
	viaSAC := ohlcDustPair{base: zleg.base, quote: usdcSAC}
	dust := ohlcDustPair{base: "ZDST-" + zeroLegIssuer, quote: usdcID}

	day := time.Now().UTC().Add(-48 * time.Hour).Truncate(24 * time.Hour)
	h1, h2 := day.Add(time.Hour), day.Add(2*time.Hour)
	seed(t, db, ctx, zleg, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"},
		{off: 20 * time.Second, base: "300000000", quote: "0", usd: "1"},
	}, h1)
	seed(t, db, ctx, flip, []seedTrade{
		{off: 30 * time.Second, base: "400000000", quote: "100000000", usd: "40"},
		{off: 40 * time.Second, base: "0", quote: "50000000", usd: "1"},
	}, h1)
	seed(t, db, ctx, zleg, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "400000000", usd: "40"},
		{off: 20 * time.Second, base: "50000000", quote: "0", usd: "1"},
	}, h2)
	seed(t, db, ctx, viaSAC, []seedTrade{
		{off: 30 * time.Second, base: "200000000", quote: "200000000", usd: "20"},
	}, h2)
	// ZDST: a real bucket at 2, then a newer one whose only priceable fill
	// is one stroop each way beside a whole-unit zero-quote trade.
	seed(t, db, ctx, dust, []seedTrade{
		{off: 10 * time.Second, base: "100000000", quote: "200000000", usd: "20"},
	}, h1)
	seed(t, db, ctx, dust, []seedTrade{
		{off: 10 * time.Second, base: "1", quote: "1", usd: ""},
		{off: 20 * time.Second, base: "10000000", quote: "0", usd: ""},
	}, h1.Add(30*time.Minute))
	for _, v := range []string{"prices_1m", "prices_1h", "prices_1mo"} {
		if _, err := db.ExecContext(ctx, "CALL refresh_continuous_aggregate('"+v+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", v, err)
		}
	}

	usdc, err := c.NewClassicAsset("USDC", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	zlegAsset, err := c.NewClassicAsset("ZLEG", zeroLegIssuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(zlegAsset, usdc)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("OHLCSeries", func(t *testing.T) {
		bars, err := store.OHLCSeries(ctx, pair, timescale.Granularity1h, h1, h1.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("OHLCSeries: %v", err)
		}
		if len(bars) != 1 {
			t.Fatalf("bars = %d, want 1", len(bars))
		}
		// base = 10+30 stored + 10+5 flipped quote; quote = 20+0 + 40+0 flipped base.
		if bars[0].BaseVolume != "550000000" || bars[0].QuoteVolume != "600000000" {
			t.Errorf("base/quote volume = %s/%s, want 550000000/600000000", bars[0].BaseVolume, bars[0].QuoteVolume)
		}
	})
	t.Run("OHLCSeriesReBucketed", func(t *testing.T) {
		bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours", day, day.Add(4*time.Hour), 10)
		if err != nil {
			t.Fatalf("OHLCSeriesReBucketed: %v", err)
		}
		if len(bars) != 1 {
			t.Fatalf("bars = %d, want 1", len(bars))
		}
		if bars[0].BaseVolume != "700000000" || bars[0].QuoteVolume != "1000000000" {
			t.Errorf("base/quote volume = %s/%s, want 700000000/1000000000", bars[0].BaseVolume, bars[0].QuoteVolume)
		}
	})
	t.Run("HistoryPoints", func(t *testing.T) {
		pts, err := store.HistoryPoints(ctx, pair, timescale.Granularity1h, 0)
		if err != nil {
			t.Fatalf("HistoryPoints: %v", err)
		}
		if len(pts) == 0 || !pts[0].Bucket.Equal(h1) || pts[0].VWAP != "3" {
			t.Errorf("first point = %+v, want bucket %s vwap 3", pts, h1)
		}
	})
	t.Run("TimedVWAPsForPair1m", func(t *testing.T) {
		pts, err := store.TimedVWAPsForPair1m(ctx, pair, h1, h1.Add(time.Minute))
		if err != nil {
			t.Fatalf("TimedVWAPsForPair1m: %v", err)
		}
		if len(pts) != 1 || pts[0].VWAP != 3 {
			t.Errorf("points = %+v, want one at 3", pts)
		}
	})
	t.Run("TimedVWAPs1mForChangeSummary", func(t *testing.T) {
		pts, err := store.TimedVWAPs1mForChangeSummary(ctx, pair, h1, h1.Add(time.Minute))
		if err != nil {
			t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
		}
		if len(pts) != 1 || pts[0].Value != "3" {
			t.Errorf("points = %+v, want one at 3", pts)
		}
	})
	t.Run("DailyMarketDays", func(t *testing.T) {
		// H1 at 2 and H2 at 4 on 10 priced units each; `volume` would
		// weight them 40:15.
		days, err := store.DailyMarketDays(ctx, []c.Asset{zlegAsset}, []c.Asset{usdc}, day, day)
		if err != nil {
			t.Fatalf("DailyMarketDays: %v", err)
		}
		if len(days) != 1 {
			t.Fatalf("days = %d, want 1", len(days))
		}
		var ok bool
		if err := db.QueryRowContext(ctx, `SELECT $1::numeric = 3`, days[0].VWAP).Scan(&ok); err != nil || !ok {
			t.Errorf("day vwap = %s, want 3 (err %v)", days[0].VWAP, err)
		}
	})
	t.Run("MonthlyUSDVWAPs", func(t *testing.T) {
		// 60 USDC / 20 ZLEG and 20 USDC-SAC / 20 ZLEG fold to 80/40.
		month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
		rows, err := store.MonthlyUSDVWAPs(ctx, month, month.AddDate(0, 1, 0),
			func(src string) int { return external.Lookup(src).AmountScaleDecimals() })
		if err != nil {
			t.Fatalf("MonthlyUSDVWAPs: %v", err)
		}
		found := false
		for _, r := range rows {
			if r.Asset == zleg.base {
				found = true
				if r.VWAPUSD != "2" {
					t.Errorf("ZLEG monthly vwap = %s, want 2", r.VWAPUSD)
				}
			}
		}
		if !found {
			t.Errorf("no ZLEG row in %+v", rows)
		}
	})
	t.Run("AssetPriceSnapshot", func(t *testing.T) {
		// H2's minute: 10 ZLEG for 40 USDC and 20 for 20 USDC-SAC = 60/30.
		if err := store.RefreshAssetListingRollups(ctx); err != nil {
			t.Fatalf("RefreshAssetListingRollups: %v", err)
		}
		var price string
		var ok bool
		if err := db.QueryRowContext(ctx,
			`SELECT price_usd::text, price_usd = 2 FROM asset_price_snapshot WHERE asset_id = $1`,
			zleg.base).Scan(&price, &ok); err != nil {
			t.Fatalf("read asset_price_snapshot: %v", err)
		}
		if !ok {
			t.Errorf("price_usd = %s, want 2", price)
		}
	})
	t.Run("USDPriceAt dust floor", func(t *testing.T) {
		resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
			USDPegs:   []string{usdcID},
			Freshness: -1,
			// Two buckets are no market; this pins the dust floor, not the gate.
			DisableSubstanceGate: true,
		})
		if err != nil {
			t.Fatalf("NewVWAPUSDFXResolver: %v", err)
		}
		zdst, err := c.NewClassicAsset("ZDST", zeroLegIssuer)
		if err != nil {
			t.Fatal(err)
		}
		// The newer bucket's priced notional is one stroop; its zero-quote
		// trade must not lift it over the one-cent floor.
		got, ok, err := resolver.USDPriceAt(ctx, zdst, h1.Add(40*time.Minute))
		r, parsed := new(big.Rat).SetString(got)
		if err != nil || !ok || !parsed || r.Cmp(big.NewRat(2, 1)) != 0 {
			t.Errorf("USDPriceAt = %q ok=%v err=%v, want 2 from the older real bucket", got, ok, err)
		}
	})
}

func migrateUpToErr(dsn string, version uint) error {
	_, thisFile, _, _ := runtime.Caller(0)
	m, err := migrate.New("file://"+filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"), dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	return m.Migrate(version)
}

func openContributionsDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execAll(t *testing.T, ctx context.Context, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func scalarInt(t *testing.T, ctx context.Context, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

// TestPriceSourceContributions_Migration0207 pins the release-N+2 step
// against real TimescaleDB, with the legacy chunks COMPRESSED:
//
//   - NULL-window rows are removed (whole legacy chunks dropped, the chunk
//     straddling the cut-over row-deleted) and every windowed row survives;
//   - window_seconds is NOT NULL and CHECK (> 0), the 0026 key is gone, so
//     a 5m row on a 1h row's bucket is accepted;
//   - down restores the nullable column and the 0026 key.
func TestPriceSourceContributions_Migration0207(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	if err := migrateUpToErr(dsn, 169); err != nil {
		t.Fatalf("migrate to 169: %v", err)
	}
	db := openContributionsDB(t, dsn)

	const ins = `INSERT INTO price_source_contributions (asset_id, quote_id, window_seconds, bucket, source, weight, trade_count) VALUES `
	execAll(t, ctx, db,
		// Two whole legacy chunks and a legacy row in the straddling chunk.
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-01-15 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-02-15 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',NULL,TIMESTAMPTZ '2025-03-10 00:00:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',300,TIMESTAMPTZ '2025-03-10 00:30:00Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',3600,TIMESTAMPTZ '2025-03-10 00:30:01Z','binance',1,1)`,
		ins+`('crypto:BTC','fiat:USD',300,TIMESTAMPTZ '2025-04-01 00:00:00Z','binance',1,1)`,
	)
	chunksBefore := scalarInt(t, ctx, db, `SELECT count(*) FROM show_chunks('price_source_contributions')`)
	if compressed := scalarInt(t, ctx, db, `
		SELECT count(*) FROM (SELECT compress_chunk(c) FROM show_chunks('price_source_contributions') c) s`); compressed < 3 {
		t.Fatalf("compressed %d chunks, want >= 3 — the compressed-chunk claim would be vacuous", compressed)
	}

	if err := migrateUpToErr(dsn, 207); err != nil {
		t.Fatalf("migrate to 200: %v", err)
	}

	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions`); n != 3 {
		t.Errorf("rows after 0207 = %d, want the 3 windowed rows", n)
	}
	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions WHERE window_seconds IS NOT NULL`); n != 3 {
		t.Errorf("windowed rows after 0207 = %d, want 3", n)
	}
	if after := scalarInt(t, ctx, db, `SELECT count(*) FROM show_chunks('price_source_contributions')`); after >= chunksBefore {
		t.Errorf("chunks %d -> %d, want the all-legacy chunks dropped", chunksBefore, after)
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds' AND is_nullable = 'NO'`); n != 1 {
		t.Error("window_seconds is still nullable")
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'price_source_contributions'::regclass AND contype = 'p'`); n != 0 {
		t.Errorf("%d primary keys remain, want the 0026 key dropped", n)
	}

	// The 0026 key refused this; the window key accepts it.
	if _, err := db.ExecContext(ctx, ins+`('crypto:BTC','fiat:USD',86400,TIMESTAMPTZ '2025-03-10 00:30:00Z','binance',1,1)`); err != nil {
		t.Errorf("a second window on an occupied bucket was refused: %v", err)
	}
	if _, err := db.ExecContext(ctx, ins+`('crypto:ETH','fiat:USD',NULL,now(),'kraken',1,1)`); err == nil {
		t.Error("a NULL-window row was accepted after 0207")
	}
	if _, err := db.ExecContext(ctx, ins+`('crypto:ETH','fiat:USD',0,now(),'kraken',1,1)`); err == nil {
		t.Error("a zero-window row was accepted after 0207")
	}

	// Down cannot restore the 0026 key over rows that collide on it.
	execAll(t, ctx, db, `DELETE FROM price_source_contributions WHERE window_seconds = 86400`)
	if err := migrateUpToErr(dsn, 205); err != nil {
		t.Fatalf("migrate down to 199: %v", err)
	}
	if n := scalarInt(t, ctx, db, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds' AND is_nullable = 'YES'`); n != 1 {
		t.Error("after 0207 down window_seconds is not nullable again")
	}
}

// TestPriceSourceContributions_Migration0207_RefusesActiveLegacyWriter pins
// the guard: NULL rows with no windowed row after them mean a pre-0169
// writer is (or was last) active, and nothing may be deleted.
func TestPriceSourceContributions_Migration0207_RefusesActiveLegacyWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	if err := migrateUpToErr(dsn, 169); err != nil {
		t.Fatalf("migrate to 169: %v", err)
	}
	db := openContributionsDB(t, dsn)
	execAll(t, ctx, db, `
		INSERT INTO price_source_contributions (asset_id, quote_id, bucket, source, weight, trade_count)
		VALUES ('crypto:BTC','fiat:USD', now(), 'binance', 1, 1)`)

	err := migrateUpToErr(dsn, 207)
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("0207 applied over live NULL-window rows: err = %v", err)
	}
	if n := scalarInt(t, ctx, db, `SELECT count(*) FROM price_source_contributions`); n != 1 {
		t.Errorf("rows after refused 0207 = %d, want 1 (nothing deleted)", n)
	}
}

// TestInsertPriceSourceContributions_BatchIsAtomic pins that one
// bucket's contribution rows commit together or not at all. The rows'
// weights only mean anything as a set (they sum to 1); a per-row
// autocommit left the prefix before a failing row persisted, so a
// reader would see a bucket whose weights sum to less than 1.
func TestInsertPriceSourceContributions_BatchIsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	countRows := func(asset string) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM price_source_contributions WHERE asset_id = $1 AND bucket = $2`,
			asset, bucket,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", asset, err)
		}
		return n
	}

	// Second row violates CHECK (trade_count >= 0), after the first
	// row has already been sent.
	failing := []timescale.PriceSourceContribution{
		{AssetID: "native", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "sdex", Weight: "0.6", TradeCount: 3},
		{AssetID: "native", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "binance", Weight: "0.4", TradeCount: -1},
	}
	if err := store.InsertPriceSourceContributions(ctx, failing); err == nil {
		t.Fatal("InsertPriceSourceContributions accepted a trade_count of -1")
	}
	if n := countRows("native"); n != 0 {
		t.Fatalf("failed batch left %d committed row(s); want 0 (partial bucket)", n)
	}

	vol := "1234.5"
	ok := []timescale.PriceSourceContribution{
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "sdex", Weight: "0.25", VolumeUSD: &vol, TradeCount: 2},
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "kraken", Weight: "0.75", TradeCount: 9},
	}
	if err := store.InsertPriceSourceContributions(ctx, ok); err != nil {
		t.Fatalf("InsertPriceSourceContributions: %v", err)
	}
	var (
		n      int
		sumW   string
		sumVol string
	)
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*), sum(weight)::text, sum(volume_usd)::text
		   FROM price_source_contributions WHERE asset_id = 'crypto:BTC' AND bucket = $1`, bucket,
	).Scan(&n, &sumW, &sumVol); err != nil {
		t.Fatalf("read committed batch: %v", err)
	}
	if n != 2 || sumW != "1.00" || sumVol != "1234.5" {
		t.Errorf("committed batch = %d rows, Σweight %s, Σvolume_usd %s; want 2, 1.00, 1234.5", n, sumW, sumVol)
	}
}

// TestPriceSourceContributions_Migration0169 pins the window behaviour against real
// TimescaleDB:
//
//   - 0169 applies to a price_source_contributions hypertable that already
//     holds a COMPRESSED chunk written by the previous binary, and that
//     legacy row keeps window_seconds NULL (its window is unrecoverable);
//   - one tick's 5m, 1h and 24h breakdowns of a pair are three rows, each
//     carrying its own window, and "latest bucket per window" returns each
//     window's own weights — before 0169 the rows had no window and that
//     read returned whichever window ran last;
//   - while 0026's key survives, a second window on an occupied bucket is
//     REFUSED instead of silently overwriting the other window's row;
//   - the previous binary's exact INSERT … ON CONFLICT still executes
//     (migrations/README.md rule 9);
//   - a row with no window is refused and nothing is written;
//   - the retention policy exists and is NOT scheduled;
//   - 0169's down drops the column and the policy and keeps the rows.
func TestPriceSourceContributions_Migration0169(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 168)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedAndCompressContributionChunk(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 169)
	assertContributionChunkCompressed(t, ctx, db)

	var legacyWindow sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT window_seconds FROM price_source_contributions
		 WHERE bucket = TIMESTAMPTZ '2025-01-15 00:00:00Z'`).Scan(&legacyWindow); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if legacyWindow.Valid {
		t.Errorf("legacy row window_seconds = %d, want NULL", legacyWindow.Int64)
	}

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket := time.Now().UTC().Truncate(time.Microsecond)
	assertThreeWindowsStayDistinct(t, ctx, db, store, bucket)
	assertOldBinaryInsertStillWorks(t, ctx, db, bucket)
	assertMissingWindowWritesNothing(t, ctx, db, store, bucket)
	assertRetentionPolicyDisabled(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 168)
	assertDownDropsWindowAndPolicy(t, ctx, db)
}

func seedAndCompressContributionChunk(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_source_contributions (asset_id, quote_id, bucket, source, weight, trade_count)
		VALUES ('crypto:BTC', 'fiat:USD', TIMESTAMPTZ '2025-01-15 00:00:00Z', 'binance', 1, 4)`); err != nil {
		t.Fatalf("seed legacy contribution row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('price_source_contributions') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no price_source_contributions chunk compressed — the compressed-chunk claim would be vacuous")
	}
}

func assertContributionChunkCompressed(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'price_source_contributions' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read chunk compression state: %v", err)
	}
	if n == 0 {
		t.Fatal("no compressed chunk after migrating — 0169 was NOT exercised against compressed data")
	}
}

func assertThreeWindowsStayDistinct(t *testing.T, ctx context.Context, db *sql.DB, store *timescale.Store, bucket time.Time) {
	t.Helper()
	// One tick, as the orchestrator writes it: the three windows in
	// order, each stamped a few microseconds after the last.
	ethRow := func(w time.Duration, at time.Time, weight float64) []timescale.PriceSourceContribution {
		return []timescale.PriceSourceContribution{{
			AssetID: "crypto:ETH", QuoteID: "fiat:USD", Window: w, Bucket: at,
			Source: "kraken", Weight: strconv.FormatFloat(weight, 'f', -1, 64), TradeCount: 2,
		}}
	}
	for i, w := range []time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour} {
		// A first write with a wrong weight, then the same (window, bucket)
		// again: the window-aware ON CONFLICT arm must update it in place.
		at := bucket.Add(time.Duration(i) * time.Microsecond)
		if err := store.InsertPriceSourceContributions(ctx, ethRow(w, at, 0.01)); err != nil {
			t.Fatalf("insert window %s: %v", w, err)
		}
		if err := store.InsertPriceSourceContributions(ctx, ethRow(w, at, float64(i+1)/4)); err != nil {
			t.Fatalf("upsert window %s: %v", w, err)
		}
	}
	assertLatestPerWindow(t, ctx, db, map[int64]float64{300: 0.25, 3600: 0.5, 86400: 0.75})

	// While 0026's key survives (release N), a second window on an
	// occupied bucket must FAIL, not overwrite the other window's row.
	if err := store.InsertPriceSourceContributions(ctx, ethRow(5*time.Minute, bucket.Add(time.Microsecond), 0.9)); err == nil {
		t.Error("a 5m row on the 1h row's bucket was accepted; it must be refused while the 0026 key stands")
	}
	assertLatestPerWindow(t, ctx, db, map[int64]float64{300: 0.25, 3600: 0.5, 86400: 0.75})
}

// assertLatestPerWindow runs the read the table documents — the latest
// bucket per (asset_id, quote_id, window_seconds) — for crypto:ETH.
func assertLatestPerWindow(t *testing.T, ctx context.Context, db *sql.DB, want map[int64]float64) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT ON (window_seconds) window_seconds, weight::float8
		  FROM price_source_contributions
		 WHERE asset_id = 'crypto:ETH' AND quote_id = 'fiat:USD' AND source = 'kraken'
		   AND window_seconds IS NOT NULL
		 ORDER BY window_seconds, bucket DESC`)
	if err != nil {
		t.Fatalf("read windows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[int64]float64{}
	for rows.Next() {
		var ws int64
		var weight float64
		if err := rows.Scan(&ws, &weight); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[ws] = weight
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("latest row per window = %v, want %v", got, want)
	}
	for ws, w := range want {
		if got[ws] != w {
			t.Errorf("window_seconds=%d weight = %v, want %v", ws, got[ws], w)
		}
	}
}

// The exact statement the previous binary issues. It names the 0026 key in
// ON CONFLICT, so it only keeps working while that key survives.
func assertOldBinaryInsertStillWorks(t *testing.T, ctx context.Context, db *sql.DB, bucket time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_source_contributions (
		    asset_id, quote_id, bucket, source,
		    weight, volume_usd, trade_count
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (asset_id, quote_id, bucket, source) DO UPDATE SET
		    weight       = EXCLUDED.weight,
		    volume_usd   = EXCLUDED.volume_usd,
		    trade_count  = EXCLUDED.trade_count`,
		"crypto:XRP", "fiat:USD", bucket, "coinbase", 1.0, nil, 1); err != nil {
		t.Errorf("previous binary's INSERT fails against 0169 (rule 9): %v", err)
	}
}

func assertMissingWindowWritesNothing(t *testing.T, ctx context.Context, db *sql.DB, store *timescale.Store, bucket time.Time) {
	t.Helper()
	err := store.InsertPriceSourceContributions(ctx, []timescale.PriceSourceContribution{
		{AssetID: "crypto:SOL", QuoteID: "fiat:USD", Window: time.Hour, Bucket: bucket, Source: "kraken", Weight: "1", TradeCount: 1},
		{AssetID: "crypto:SOL", QuoteID: "fiat:USD", Bucket: bucket, Source: "binance", Weight: "1", TradeCount: 1},
	})
	if !errors.Is(err, timescale.ErrContributionWindowRequired) {
		t.Errorf("windowless row: err = %v, want ErrContributionWindowRequired", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM price_source_contributions WHERE asset_id = 'crypto:SOL'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a batch holding a windowless row wrote %d rows, want 0", n)
	}
}

func assertRetentionPolicyDisabled(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var scheduled bool
	var dropAfter string
	if err := db.QueryRowContext(ctx, `
		SELECT scheduled, config->>'drop_after' FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'price_source_contributions'`).Scan(&scheduled, &dropAfter); err != nil {
		t.Fatalf("read retention job: %v", err)
	}
	if scheduled {
		t.Error("0169's retention policy is scheduled; it must ship disabled")
	}
	if dropAfter != "90 days" {
		t.Errorf("retention drop_after = %q, want 90 days", dropAfter)
	}
}

func assertDownDropsWindowAndPolicy(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var cols, jobs, rows int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM information_schema.columns
		         WHERE table_name = 'price_source_contributions' AND column_name = 'window_seconds'),
		       (SELECT count(*) FROM timescaledb_information.jobs
		         WHERE proc_name = 'policy_retention' AND hypertable_name = 'price_source_contributions'),
		       (SELECT count(*) FROM price_source_contributions)`).Scan(&cols, &jobs, &rows); err != nil {
		t.Fatalf("read post-down state: %v", err)
	}
	if cols != 0 || jobs != 0 {
		t.Errorf("after 0169 down: window_seconds columns=%d retention jobs=%d, want 0 and 0", cols, jobs)
	}
	if rows != 5 {
		t.Errorf("after 0169 down: %d rows, want 5 (legacy + three windows + old-binary row)", rows)
	}
}

// TestPricelessCoverage_AMMConcentration executes the priceless-popular
// tripwire's candidate SQL (timescale.Store.PopularPricelessCandidates)
// and its classifier against a real TimescaleDB, on the four counterparty
// populations the trades hypertable actually holds.
//
// The concentration NUMERATOR must not require `maker IS NOT NULL
// AND taker IS NOT NULL` while the vol7d DENOMINATOR takes every row, or the
// two are measured over DIFFERENT populations. Only the SDEX decoder
// records both sides; every Soroban AMM (aquarius, soroswap, phoenix,
// comet, sushiswap_v3) leaves the resting side to the pool and records a
// taker only — on r1, 100% of the 27.8k 24h rows of all five AMM sources
// have maker NULL. The share of an AMM-only asset would then be 0 BY
// CONSTRUCTION, the wash exclusion could never fire for it, and a farm
// painting volume on an AMM would self-select straight into the coverage
// alert the tripwire exists to keep honest. Two such assets were live on r1,
// above the $10k popularity floor with 0.95 / 0.9999 of their volume
// swapped by ONE account, both reporting a 0 concentration share.
//
// The four fixtures are one population each, and each pins the CORRECTED
// share, not merely "non-zero":
//
//	AMM wash   aquarius, maker NULL: 19/20 of $20k one taker  → 0.95, excluded
//	AMM broad  aquarius, maker NULL: five takers, 4/20 each   → 0.20, fires
//	SDEX pair  sdex, both sides: two pairs, 10/20 each        → 0.50, fires
//	CEX feed   binance, neither side recorded                 → 0.00, fires
//
// The SDEX and CEX rows are the no-regression half: the order book's
// pair keying is unchanged by the fix, and volume from a venue that
// records no account at all must still PAGE (it cannot be measured, and
// the tripwire fails loud, never quiet) instead of being suppressed.
func TestPricelessCoverage_AMMConcentration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	mustAsset := func(code string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mustPair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Four base assets, one per counterparty population, quoted against a
	// single non-proxy asset so nothing here is priceable (prices_1m is
	// never refreshed) and only the four bases become candidates.
	ammWash := mustAsset("AMMWSH")
	ammBroad := mustAsset("AMMBRD")
	sdexPair := mustAsset("SDXPAR")
	cexFeed := mustAsset("CEXFED")
	quote := mustAsset("ZQOT")

	// Counterparty accounts. Only string identity matters to the roll —
	// the stored columns are plain text.
	const (
		washer    = "GAWASHERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		passerby  = "GAPASSERBYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		restingA  = "GARESTINGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		crossingA = "GACROSSINGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		crossingB = "GACROSSINGBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	broadTakers := []string{
		"GABROADONEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADTWOAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADSIXAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADFORAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"GABROADFIVAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	add := func(source string, base c.Asset, maker, taker string) {
		nonce++
		tr := mkIntegrationTrade(source, nonce, now.Add(-time.Duration(nonce)*time.Minute),
			mustPair(base, quote), 1_000_000_000, 1_000_000_000)
		tr.Maker, tr.Taker = maker, taker
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s/%d: %v", source, nonce, err)
		}
	}

	for i := 0; i < 20; i++ {
		// AMM wash: one account round-tripping through the pool for 19 of
		// the 20 fills; the pool itself is never an account (maker NULL).
		taker := washer
		if i == 19 {
			taker = passerby
		}
		add("aquarius", ammWash, "", taker)

		// AMM broad: the same venue shape, five distinct swappers.
		add("aquarius", ammBroad, "", broadTakers[i%len(broadTakers)])

		// Order book: both sides recorded, two pairs sharing one maker.
		counter := crossingA
		if i%2 == 1 {
			counter = crossingB
		}
		add("sdex", sdexPair, restingA, counter)

		// External CEX feed: neither side is recorded at all.
		add("binance", cexFeed, "", "")
	}

	// The quote leg is not a USD peg, so insert-time usd_volume is NULL.
	// Stamp a flat $2,000 per fill: every asset then holds $40,000 of 7d
	// and 24h volume. The popularity floor is measured on
	// MARKET-CHARACTER volume (raw minus the top counterparty pair's own
	// volume — see popularPriceless), so sdex_pair's legitimate 50%-share
	// two-market-maker book must clear the floor even AFTER half its
	// volume is subtracted; $1,000/fill left it sitting exactly on the
	// floor post-discount, which is what motivated the bump.
	if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 2000`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}

	sigs, err := store.PopularPricelessCandidates(ctx)
	if err != nil {
		t.Fatalf("PopularPricelessCandidates: %v", err)
	}
	byID := make(map[string]timescale.AssetCoverageSignals, len(sigs))
	for _, s := range sigs {
		byID[s.AssetID] = s
	}

	for _, tc := range []struct {
		label                   string
		asset                   c.Asset
		wantTopPair, wantAttrib float64
	}{
		{"amm_wash", ammWash, 0.95, 1.0},
		{"amm_broad", ammBroad, 0.20, 1.0},
		{"sdex_pair", sdexPair, 0.50, 1.0},
		{"cex_feed", cexFeed, 0.00, 0.0},
	} {
		sig, ok := byID[tc.asset.String()]
		if !ok {
			t.Errorf("%s: %s missing from the candidate set", tc.label, tc.asset)
			continue
		}
		if sig.Volume7dUSD != 40_000 {
			t.Errorf("%s: vol_7d = %v, want 40000", tc.label, sig.Volume7dUSD)
		}
		if math.Abs(sig.TopAccountPairVolShare-tc.wantTopPair) > 1e-9 {
			t.Errorf("%s: top_account_pair_share = %v, want %v",
				tc.label, sig.TopAccountPairVolShare, tc.wantTopPair)
		}
		if math.Abs(sig.AttributedVolShare-tc.wantAttrib) > 1e-9 {
			t.Errorf("%s: attributed_vol_share = %v, want %v",
				tc.label, sig.AttributedVolShare, tc.wantAttrib)
		}
	}

	// End to end: the real SQL through the real classifier. The wash farm
	// on the AMM must NOT be paged for; the other three must be.
	var logbuf bytes.Buffer
	w := pricelesscoverage.New(store, pricelesscoverage.Options{
		Logger: slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	w.Sweep(ctx)

	paged := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(logbuf.Bytes()))
	for sc.Scan() {
		var rec struct {
			Msg     string `json:"msg"`
			AssetID string `json:"asset_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("sweep log line %q: %v", sc.Text(), err)
		}
		if strings.HasPrefix(rec.Msg, "priceless-popular coverage gap") && rec.AssetID != "" {
			paged[rec.AssetID] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan sweep log: %v", err)
	}
	for _, tc := range []struct {
		label string
		asset c.Asset
		want  bool
	}{
		{"amm_wash", ammWash, false},
		{"amm_broad", ammBroad, true},
		{"sdex_pair", sdexPair, true},
		{"cex_feed", cexFeed, true},
	} {
		if got := paged[tc.asset.String()]; got != tc.want {
			t.Errorf("sweep paged %s (%s) = %v, want %v", tc.label, tc.asset, got, tc.want)
		}
	}
}

// TestPricelessCoverage_PricedDirectAppliesSubstanceFloors is a
// regression guard: priced_direct must not count an asset as priced on a
// SINGLE unqualified prices_1m row, while its sibling one_hop CTE applies
// three substance floors (vol_usd >= 1000, buckets >= 20, span_s >=
// 21600) and says in its own comment that they are "NOT decoration". The
// two arms of "is this asset priced?" must agree.
//
// THIN: one $50 trade against a USD proxy — a single bucket, single
// minute of span, well under every floor. Must NOT be priced.
//
// SUBSTANTIAL: 21 trades against the same proxy, 20 minutes apart,
// spanning ~6h40m and summing to $2,100 — clears all three floors. Must
// be priced, proving the added floors do not regress a real market.
func TestPricelessCoverage_PricedDirectAppliesSubstanceFloors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	thin, err := c.NewClassicAsset("THINQ", issuer)
	if err != nil {
		t.Fatal(err)
	}
	solid, err := c.NewClassicAsset("SOLIDQ", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	thinPair, err := c.NewPair(thin, usd)
	if err != nil {
		t.Fatal(err)
	}
	solidPair, err := c.NewPair(solid, usd)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Hour)
	nonce := 0

	// THIN: one trade, one bucket.
	nonce++
	tr := mkIntegrationTrade("sdex", nonce, now, thinPair, 1_000_000_000, 1_000_000_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade thin: %v", err)
	}

	// SOLID: 21 trades, 20 minutes apart -> 21 distinct 1-minute buckets,
	// ~400 minutes (6h40m) of span, comfortably clearing every floor.
	solidStart := now.Add(-7 * time.Hour)
	for i := 0; i < 21; i++ {
		nonce++
		ts := solidStart.Add(time.Duration(i) * 20 * time.Minute)
		tr := mkIntegrationTrade("sdex", nonce, ts, solidPair, 1_000_000_000, 1_000_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade solid %d: %v", i, err)
		}
	}

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE base_asset = $1`, thin.String()); err != nil {
		t.Fatalf("stamp thin usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE base_asset = $1`, solid.String()); err != nil {
		t.Fatalf("stamp solid usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	priced, err := store.AssetIsPriced(ctx, thin.String())
	if err != nil {
		t.Fatalf("AssetIsPriced thin: %v", err)
	}
	if priced {
		t.Errorf("thin (single row, $50, one bucket) reads priced=true, want false — " +
			"priced_direct must apply the same substance floors as one_hop")
	}

	priced, err = store.AssetIsPriced(ctx, solid.String())
	if err != nil {
		t.Fatalf("AssetIsPriced solid: %v", err)
	}
	if !priced {
		t.Errorf("solid ($2,100 over 21 buckets, ~6h40m span) reads priced=false, want true — " +
			"a genuinely substantial direct market must still be priced")
	}
}

// TestPricelessCoverage_QuoteLegOnlyAsset: an asset that only ever appears
// as the QUOTE of a stored trade (swap-direction sources) must still be a
// candidate, with the trade's volume, while a proxy quote never is.
func TestPricelessCoverage_QuoteLegOnlyAsset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := c.NewClassicAsset("BASEONLY", issuer)
	if err != nil {
		t.Fatal(err)
	}
	quoteOnly, err := c.NewClassicAsset("QUOTEONLY", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(base, quoteOnly)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	for i := 1; i <= 5; i++ {
		tr := mkIntegrationTrade("aquarius", i, now.Add(-time.Duration(i)*time.Minute), pair, 1_000_000_000, 1_000_000_000)
		tr.Taker = "GAQUOTELEGTAKERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE trades SET usd_volume = 2000`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}

	sigs, err := store.PopularPricelessCandidates(ctx)
	if err != nil {
		t.Fatalf("PopularPricelessCandidates: %v", err)
	}
	byID := make(map[string]timescale.AssetCoverageSignals, len(sigs))
	for _, s := range sigs {
		byID[s.AssetID] = s
	}
	for _, a := range []c.Asset{base, quoteOnly} {
		sig, ok := byID[a.String()]
		if !ok {
			t.Fatalf("%s missing from the candidate set", a)
		}
		if sig.Volume7dUSD != 10_000 || sig.Trades7d != 5 {
			t.Errorf("%s: vol_7d=%v trades_7d=%d, want 10000 / 5", a, sig.Volume7dUSD, sig.Trades7d)
		}
	}
}

// TestPricelessCoverage_ServedSnapshotCountsAsPriced: an asset with priced
// 7d volume but no proxy market above the substance floors is a candidate
// until the listing serves it a price. A fresh asset_price_snapshot row
// takes it out of the candidate set and makes AssetIsPriced true; a row
// past the listing's staleness bound does neither.
func TestPricelessCoverage_ServedSnapshotCountsAsPriced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	asset, err := c.NewClassicAsset("SERVEDQ", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(asset, usd)
	if err != nil {
		t.Fatal(err)
	}

	// One thin trade: priced 7d volume, far under every substance floor.
	tr := mkIntegrationTrade("sdex", 1, time.Now().UTC().Truncate(time.Minute).Add(-2*time.Hour), pair, 1_000_000_000, 1_000_000_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE base_asset = $1`, asset.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	check := func(label string, wantPriced bool) {
		t.Helper()
		priced, err := store.AssetIsPriced(ctx, asset.String())
		if err != nil {
			t.Fatalf("%s: AssetIsPriced: %v", label, err)
		}
		if priced != wantPriced {
			t.Errorf("%s: AssetIsPriced = %v, want %v", label, priced, wantPriced)
		}
		sigs, err := store.PopularPricelessCandidates(ctx)
		if err != nil {
			t.Fatalf("%s: PopularPricelessCandidates: %v", label, err)
		}
		candidate := false
		for _, s := range sigs {
			if s.AssetID == asset.String() {
				candidate = true
			}
		}
		if candidate == wantPriced {
			t.Errorf("%s: candidate = %v, want %v", label, candidate, !wantPriced)
		}
	}

	check("no snapshot row", false)

	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO asset_price_snapshot (asset_id, price_usd, source_count, computed_at)
		 VALUES ($1, 0.000002877964831796519168, 1, now() - INTERVAL '1 hour')`, asset.String()); err != nil {
		t.Fatalf("insert stale snapshot: %v", err)
	}
	check("stale snapshot row", false)

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE asset_price_snapshot SET computed_at = now() WHERE asset_id = $1`, asset.String()); err != nil {
		t.Fatalf("freshen snapshot: %v", err)
	}
	check("fresh snapshot row", true)
}

// TestRoutedViaTaggingAndRollup covers migration 0025 Phase B
// end-to-end against a real TimescaleDB:
//
//  1. TagTradesRoutedVia joins soroswap_router_swaps → trades on
//     (ledger, tx_hash), scoped to source='soroswap' — a same-tx
//     trade from another protocol is never tagged.
//  2. First-wins: an existing routed_via (from a different router)
//     is never overwritten.
//  3. Idempotence: re-running the same window tags zero rows.
//  4. AggregatorRollup math: per-router trade counts honour the
//     `since` bound, LastRoutedAt = max(ts), volume is NULL (not
//     zero) when no routed trade carries usd_volume, and the
//     registry seeds (0032/0033) surface with the 0072-aligned
//     'soroswap-router' name.
func TestRoutedViaTaggingAndRollup(t *testing.T) { //nolint:gocognit // linear scenario walk
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
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	t1 := t0.Add(5 * time.Minute)

	// Tx A (ledger 100): a 3-hop router swap → two soroswap leg
	// trades + one same-tx phoenix trade (must stay untagged).
	txA := mkIntegrationTrade("soroswap", 1, t0, pair, 1_000_000_000, 12_000_000)
	txA.Ledger = 100
	txA2 := txA
	txA2.OpIndex = txA.OpIndex + 1
	txAPhoenix := mkIntegrationTrade("phoenix", 1, t0, pair, 500_000_000, 6_000_000)
	txAPhoenix.Ledger = 100
	txAPhoenix.TxHash = txA.TxHash // same tx!

	// Tx B (ledger 101): one soroswap trade, pre-tagged by a
	// DIFFERENT router (first-wins fixture).
	txB := mkIntegrationTrade("soroswap", 2, t1, pair, 1_000_000_000, 12_100_000)
	txB.Ledger = 101

	// Unrelated soroswap trade (ledger 102, no router call).
	direct := mkIntegrationTrade("soroswap", 3, t1.Add(time.Minute), pair, 1_000_000_000, 12_200_000)
	direct.Ledger = 102

	for _, tr := range []c.Trade{txA, txA2, txAPhoenix, txB, direct} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	// Router invocations for tx A + tx B.
	for i, swap := range []struct {
		ledger uint32
		ts     time.Time
		txHash string
	}{
		{100, t0, txA.TxHash},
		{101, t1, txB.TxHash},
	} {
		row := timescale.SoroswapRouterSwap{
			Ledger:          swap.ledger,
			LedgerCloseTime: swap.ts,
			TxHash:          swap.txHash,
			OpIndex:         0,
			ContractID:      "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH",
			FunctionName:    "swap_exact_tokens_for_tokens",
			Recipient:       "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Path: []string{
				"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
				"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
			},
			AmountIn:  "1000000000",
			AmountOut: "12000000",
			CallSig:   "cafebabecafebabecafebabecafebab" + string(rune('0'+i)),
		}
		if err := store.InsertSoroswapRouterSwap(ctx, row); err != nil {
			t.Fatalf("InsertSoroswapRouterSwap: %v", err)
		}
	}

	windowFrom, windowTo := t0.Add(-time.Hour), t1.Add(time.Hour)

	// First-wins fixture: tag tx B's window as a different router
	// BEFORE the real pass runs.
	if n, err := store.TagTradesRoutedVia(ctx, "other-router", "soroswap", t1.Add(-time.Second), t1.Add(time.Second)); err != nil {
		t.Fatalf("pre-tag other-router: %v", err)
	} else if n != 1 {
		t.Fatalf("pre-tag tagged %d rows, want 1 (tx B)", n)
	}

	// ─── The real pass ──────────────────────────────────────────
	tagged, err := store.TagTradesRoutedVia(ctx, "soroswap-router", "soroswap", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("TagTradesRoutedVia: %v", err)
	}
	// Tx A's two soroswap legs only: the phoenix same-tx row is
	// source-scoped out, tx B is first-wins-protected, `direct` has
	// no router call.
	if tagged != 2 {
		t.Errorf("tagged = %d, want 2 (tx A's two soroswap legs)", tagged)
	}

	// Idempotence: an identical re-run is a no-op.
	again, err := store.TagTradesRoutedVia(ctx, "soroswap-router", "soroswap", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("TagTradesRoutedVia rerun: %v", err)
	}
	if again != 0 {
		t.Errorf("rerun tagged = %d, want 0 (idempotent)", again)
	}

	// Read back through the /v1/history read path.
	trades, err := store.TradesInRange(ctx, pair, windowFrom, windowTo.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	byKey := map[string]string{} // "<ledger>/<op>" → routed_via
	for _, tr := range trades {
		if tr.Source == "soroswap" {
			byKey[keyOf(tr.Ledger, tr.OpIndex)] = tr.RoutedVia
		}
	}
	if got := byKey[keyOf(100, txA.OpIndex)]; got != "soroswap-router" {
		t.Errorf("tx A leg 1 routed_via = %q, want soroswap-router", got)
	}
	if got := byKey[keyOf(100, txA2.OpIndex)]; got != "soroswap-router" {
		t.Errorf("tx A leg 2 routed_via = %q, want soroswap-router", got)
	}
	if got := byKey[keyOf(101, txB.OpIndex)]; got != "other-router" {
		t.Errorf("tx B routed_via = %q, want other-router (first-wins violated)", got)
	}
	if got := byKey[keyOf(102, direct.OpIndex)]; got != "" {
		t.Errorf("direct trade routed_via = %q, want empty", got)
	}

	// ─── Rollup math ────────────────────────────────────────────
	rollup, err := store.AggregatorRollup(ctx, windowFrom)
	if err != nil {
		t.Fatalf("AggregatorRollup: %v", err)
	}
	var router *timescale.AggregatorRollupRow
	vaults := 0
	for i := range rollup {
		switch rollup[i].Kind {
		case "router":
			if rollup[i].Name == "soroswap-router" {
				router = &rollup[i]
			}
		case "aggregator-vault":
			vaults++
		}
	}
	// Migration 0072 must have renamed the 0032 seed; 0033 seeds 3 vaults.
	if router == nil {
		t.Fatalf("no 'soroswap-router' registry row in rollup (0032 seed + 0072 rename missing?): %+v", rollup)
	}
	if vaults != 3 {
		t.Errorf("vault rows = %d, want 3 (0033 seed)", vaults)
	}
	if router.RoutedTrades != 2 {
		t.Errorf("RoutedTrades = %d, want 2", router.RoutedTrades)
	}
	// No usd_volume on any fixture trade → NULL volume, not "0".
	if router.RoutedVolume != nil {
		t.Errorf("RoutedVolume = %v, want nil (no USD valuation)", *router.RoutedVolume)
	}
	if router.LastRoutedAt == nil || !router.LastRoutedAt.Equal(t0) {
		t.Errorf("LastRoutedAt = %v, want %v (max ts of routed trades)", router.LastRoutedAt, t0)
	}
	// Vault entries carry zero routed stats.
	for i := range rollup {
		if rollup[i].Kind == "aggregator-vault" && rollup[i].RoutedTrades != 0 {
			t.Errorf("vault %s RoutedTrades = %d, want 0", rollup[i].Name, rollup[i].RoutedTrades)
		}
	}

	// `since` bound: a window starting after tx A excludes its trades.
	late, err := store.AggregatorRollup(ctx, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("AggregatorRollup(late): %v", err)
	}
	for i := range late {
		if late[i].Name == "soroswap-router" && late[i].RoutedTrades != 0 {
			t.Errorf("late-window RoutedTrades = %d, want 0", late[i].RoutedTrades)
		}
	}
}

func keyOf(ledger, op uint32) string {
	return fmt.Sprintf("%d/%d", ledger, op)
}

// TestRoutedViaCallPathAttribution covers the migration 0101/0103
// follow-on (ROADMAP #11 + #29): a router swap recorded as a
// sub_invocation is attributed to its OUTERMOST wrapping contract
// when that contract is a registered 'router'-kind entry, and falls
// back to the plain router name otherwise (unregistered wrapper, or
// a direct top_level call). Three same-shape router swaps, one per
// case, driven through the exact production primitive
// (TagTradesRoutedVia) both live paths call.
func TestRoutedViaCallPathAttribution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn) // includes the 0103 aggregator-exec seed

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		routerContract    = "CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH"
		knownAggregator   = "CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH" // migration 0103 seed
		unknownAggregator = "CBUNKNOWNWRAPPERNOTINTHEREGISTRYAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	direct := mkIntegrationTrade("soroswap", 10, t0, pair, 1_000_000_000, 12_000_000)
	direct.Ledger = 200
	viaKnown := mkIntegrationTrade("soroswap", 11, t0.Add(time.Minute), pair, 1_000_000_000, 12_000_000)
	viaKnown.Ledger = 201
	viaUnknown := mkIntegrationTrade("soroswap", 12, t0.Add(2*time.Minute), pair, 1_000_000_000, 12_000_000)
	viaUnknown.Ledger = 202

	for _, tr := range []c.Trade{direct, viaKnown, viaUnknown} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	path := []string{
		"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
	}
	swaps := []timescale.SoroswapRouterSwap{
		{ // top_level: direct call, no wrapper.
			Ledger: direct.Ledger, LedgerCloseTime: t0, TxHash: direct.TxHash, OpIndex: 0,
			ContractID: routerContract, FunctionName: "swap_exact_tokens_for_tokens",
			Recipient: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Path:      path, AmountIn: "1000000000", AmountOut: "12000000",
			CallSig:  "callpathtest000000000000000001a",
			CallPath: []string{routerContract}, CallDepth: 0, CallKind: "top_level",
		},
		{ // sub_invocation wrapped by the registered aggregator-exec seed.
			Ledger: viaKnown.Ledger, LedgerCloseTime: t0.Add(time.Minute), TxHash: viaKnown.TxHash, OpIndex: 0,
			ContractID: routerContract, FunctionName: "swap_exact_tokens_for_tokens",
			Recipient: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Path:      path, AmountIn: "1000000000", AmountOut: "12000000",
			CallSig:  "callpathtest000000000000000002b",
			CallPath: []string{knownAggregator, routerContract}, CallDepth: 1, CallKind: "sub_invocation",
		},
		{ // sub_invocation wrapped by a contract NOT in the registry.
			Ledger: viaUnknown.Ledger, LedgerCloseTime: t0.Add(2 * time.Minute), TxHash: viaUnknown.TxHash, OpIndex: 0,
			ContractID: routerContract, FunctionName: "swap_exact_tokens_for_tokens",
			Recipient: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			Path:      path, AmountIn: "1000000000", AmountOut: "12000000",
			CallSig:  "callpathtest000000000000000003c",
			CallPath: []string{unknownAggregator, routerContract}, CallDepth: 1, CallKind: "sub_invocation",
		},
	}
	for _, sw := range swaps {
		if err := store.InsertSoroswapRouterSwap(ctx, sw); err != nil {
			t.Fatalf("InsertSoroswapRouterSwap: %v", err)
		}
	}

	windowFrom, windowTo := t0.Add(-time.Minute), t0.Add(10*time.Minute)
	tagged, err := store.TagTradesRoutedVia(ctx, "soroswap-router", "soroswap", windowFrom, windowTo)
	if err != nil {
		t.Fatalf("TagTradesRoutedVia: %v", err)
	}
	if tagged != 3 {
		t.Fatalf("tagged = %d, want 3", tagged)
	}

	trades, err := store.TradesInRange(ctx, pair, windowFrom, windowTo.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	byLedger := map[uint32]string{}
	for _, tr := range trades {
		byLedger[tr.Ledger] = tr.RoutedVia
	}
	if got := byLedger[direct.Ledger]; got != "soroswap-router" {
		t.Errorf("direct call routed_via = %q, want soroswap-router (top_level fallback)", got)
	}
	if got := byLedger[viaKnown.Ledger]; got != "soroswap-router-aggregator-exec" {
		t.Errorf("known-wrapper call routed_via = %q, want soroswap-router-aggregator-exec (call_path attributed)", got)
	}
	if got := byLedger[viaUnknown.Ledger]; got != "soroswap-router" {
		t.Errorf("unknown-wrapper call routed_via = %q, want soroswap-router (unregistered-wrapper fallback)", got)
	}

	rollup, err := store.AggregatorRollup(ctx, windowFrom)
	if err != nil {
		t.Fatalf("AggregatorRollup: %v", err)
	}
	byName := map[string]timescale.AggregatorRollupRow{}
	for _, row := range rollup {
		byName[row.Name] = row
	}
	execRow, ok := byName["soroswap-router-aggregator-exec"]
	if !ok {
		t.Fatalf("no 'soroswap-router-aggregator-exec' registry row in rollup (0103 seed missing?): %+v", rollup)
	}
	if !execRow.AutoDiscovered {
		t.Errorf("aggregator-exec AutoDiscovered = false, want true (evidence-observed, not vetted)")
	}
	if execRow.RoutedTrades != 1 {
		t.Errorf("aggregator-exec RoutedTrades = %d, want 1 (only the known-wrapper trade)", execRow.RoutedTrades)
	}
	routerRow, ok := byName["soroswap-router"]
	if !ok {
		t.Fatalf("no 'soroswap-router' registry row in rollup: %+v", rollup)
	}
	if routerRow.RoutedTrades != 2 {
		t.Errorf("soroswap-router RoutedTrades = %d, want 2 (direct + unknown-wrapper fallback)", routerRow.RoutedTrades)
	}
}

// TestSubstanceGate_AliasUnionCountsSharedMinutesOnce executes the
// substance gate's alias union on real prices_1m rows. XLM's SDEX leg
// (native) and CEX leg (crypto:XLM) trading in the SAME ten minutes are
// ten distinct minutes of market; counting each spelling's minutes and
// adding them read twenty and cleared the default 20-minute floor.
func TestSubstanceGate_AliasUnionCountsSharedMinutesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cexXLM, err := c.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := c.NewClassicAsset("SHRD", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	disjoint, err := c.NewClassicAsset("DSJT", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}

	// Ten minutes spread over 7h03m, well inside the trailing day.
	start := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Hour)
	nonce := 0
	add := func(ts time.Time, base, quote c.Asset) {
		t.Helper()
		pair, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		nonce++
		if err := store.InsertTrade(ctx, mkAPITrade(nonce, ts.Add(10*time.Second), pair, 1_000_000, 500_000)); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		minute := start.Add(time.Duration(i) * 47 * time.Minute)
		// SHRD: both spellings in the same minute.
		add(minute, c.NativeAsset(), shared)
		add(minute, cexXLM, shared)
		// DSJT, the control: the same volume in ten DIFFERENT minutes
		// per spelling — twenty distinct minutes of real market.
		add(minute, c.NativeAsset(), disjoint)
		add(minute.Add(20*time.Minute), cexXLM, disjoint)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 500 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	sub, err := store.PairMarketSubstance(ctx, c.AssetAliases(c.NativeAsset()), c.AssetAliases(shared), 24*time.Hour)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	if mustFloat(t, sub.VolumeUSD) != 10_000 || sub.Buckets != 10 || sub.SpanSeconds != 9*47*60 {
		t.Errorf("SHRD union = {volume %s, buckets %d, span %ds}, want {10000, 10, %ds}: "+
			"volumes add across spellings, a shared minute is one bucket",
			sub.VolumeUSD, sub.Buckets, sub.SpanSeconds, 9*47*60)
	}

	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	allowed, measured := gate.Verdict(ctx, c.NativeAsset(), shared, "test")
	if !measured {
		t.Fatal("SHRD: verdict unmeasured")
	}
	if allowed {
		t.Error("SHRD cleared the 20-distinct-minute floor on 10 minutes quoted under two XLM spellings")
	}
	allowed, measured = gate.Verdict(ctx, c.NativeAsset(), disjoint, "test")
	if !measured || !allowed {
		t.Errorf("DSJT (control, 20 distinct minutes over 7h): allowed=%v measured=%v, want served", allowed, measured)
	}
}

// TestSubstanceGate_TellsAnUnvaluableMarketFromAThinOne executes both
// substance reads against real TimescaleDB and runs the gate over them
// . prices_1m stores sum(coalesce(usd_volume, 0)), so a market
// the insert-time waterfall could not value reads as $0 — the same
// number as a market that was valued and found empty. The reads now
// return how many active buckets carried a dollar value, and the gate
// names the two failures apart.
//
//   - UNVA/PAIR — 30 minutes of trades over 9h40m with usd_volume NULL
//     (the SEP-41/SEP-41 shape): clears buckets and span, $0 volume.
//   - THIN/native — the same activity, every trade valued at $1: a
//     genuinely thin $30 market.
//
// Both stay withheld; only the reason differs.
func TestSubstanceGate_TellsAnUnvaluableMarketFromAThinOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	unva, err := c.NewClassicAsset("UNVA", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pairLeg, err := c.NewClassicAsset("PAIR", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	thin, err := c.NewClassicAsset("THIN", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	unvaluedPair, _ := c.NewPair(unva, pairLeg)
	thinPair, _ := c.NewPair(thin, c.NativeAsset())

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	for i := 0; i < 30; i++ {
		ts := now.Add(-10*time.Hour + time.Duration(i)*20*time.Minute)
		for _, pair := range []c.Pair{unvaluedPair, thinPair} {
			nonce++
			if err := store.InsertTrade(ctx, mkAPITrade(nonce, ts, pair, 1_000_000, 500_000)); err != nil {
				t.Fatalf("InsertTrade: %v", err)
			}
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = NULL WHERE source = 'integ-api' AND base_asset = $1`, unva.String()); err != nil {
		t.Fatalf("clear usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 1 WHERE source = 'integ-api' AND base_asset = $1`, thin.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	const (
		wantBuckets = 30
		wantSpan    = 29 * 20 * 60
	)
	check := func(name string, got timescale.MarketSubstance, volume float64, valued int64) {
		t.Helper()
		if v := mustFloat(t, got.VolumeUSD); v != volume || got.Buckets != wantBuckets ||
			got.SpanSeconds != wantSpan || got.ValuedBuckets != valued {
			t.Errorf("%s: got {volume %s, buckets %d, span %ds, valued %d}, want {volume %v, buckets %d, span %ds, valued %d}",
				name, got.VolumeUSD, got.Buckets, got.SpanSeconds, got.ValuedBuckets,
				volume, wantBuckets, wantSpan, valued)
		}
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		pair   c.Pair
		volume float64
		valued int64
	}{
		{unvaluedPair, 0, 0},
		{thinPair, 30, wantBuckets},
	} {
		bases, quotes := c.AssetAliases(tc.pair.Base), c.AssetAliases(tc.pair.Quote)
		live, err := store.PairMarketSubstance(ctx, bases, quotes, day)
		if err != nil {
			t.Fatalf("PairMarketSubstance(%s): %v", tc.pair, err)
		}
		check(tc.pair.String()+" trailing", live, tc.volume, tc.valued)
		at, err := store.PairMarketSubstanceAt(ctx, bases, quotes, now, day, timescale.Granularity1m)
		if err != nil {
			t.Fatalf("PairMarketSubstanceAt(%s): %v", tc.pair, err)
		}
		check(tc.pair.String()+" at now", at, tc.volume, tc.valued)
	}

	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	for _, tc := range []struct {
		base, quote c.Asset
		want        pricingguard.SubstanceFloor
	}{
		{unva, pairLeg, pricingguard.FloorVolumeUnvalued},
		{thin, c.NativeAsset(), pricingguard.FloorVolume},
	} {
		allowed, measured, floor := gate.Probe(ctx, tc.base, tc.quote)
		if allowed || !measured || floor != tc.want {
			t.Errorf("Probe(%s, %s) = (allowed %v, measured %v, floor %q), want (false, true, %q)",
				tc.base, tc.quote, allowed, measured, floor, tc.want)
		}
	}
}

// TestTransitiveUSDPriceCandidates_RankedHops pins that the resolver
// returns EVERY priced hop, ranked by near-leg (asset<->hop) USD volume,
// rather than only the deepest one. The API gates each candidate and
// falls through to the next, which is only possible if the second-best
// hop reaches it.
//
// Fixture (closed buckets inside the trailing 24h):
//
//	XLM/USDC   vwap 0.40, before every hop leg → xlm_usd = 0.40 at each leg's minute
//	HOPA/XLM   vwap 2                 → HOPA = 0.80 USD
//	HOPB/XLM   vwap 0.5               → HOPB = 0.20 USD
//	TGT/HOPA   vwap 1,  $100 volume   → TGT  = 0.80 USD via HOPA
//	HOPB/TGT   vwap 0.25, $1000 volume (inverted) → TGT = 0.80 USD via HOPB
//	TGT/NOPX   NOPX has no USD route  → not a candidate
//
// HOPB outranks HOPA on volume though it sorts after it by id, so the
// order proves the volume ranking rather than the tie-break.
func TestTransitiveUSDPriceCandidates_RankedHops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		hopIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		tgtIssuer  = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	classic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	xlm := c.NativeAsset()
	usdc := classic("USDC", usdcIssuer)
	hopA := classic("HOPA", hopIssuer)
	hopB := classic("HOPB", hopIssuer)
	nopx := classic("NOPX", hopIssuer)
	tgt := classic("TGT", tgtIssuer)

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	insert := func(ts time.Time, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, ts, p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s: %v", p, err)
		}
	}
	insert(now.Add(-30*time.Minute), pair(xlm, usdc), 1_000_000_000, 400_000_000)
	insert(now.Add(-20*time.Minute), pair(hopA, xlm), 100_000_000, 200_000_000)
	insert(now.Add(-20*time.Minute), pair(hopB, xlm), 100_000_000, 50_000_000)
	insert(now.Add(-15*time.Minute), pair(tgt, hopA), 100_000_000, 100_000_000)
	insert(now.Add(-15*time.Minute), pair(hopB, tgt), 400_000_000, 100_000_000)
	insert(now.Add(-15*time.Minute), pair(tgt, nopx), 100_000_000, 100_000_000)

	stamp := func(base, quote c.Asset, usd int) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx,
			`UPDATE trades SET usd_volume = $3 WHERE base_asset = $1 AND quote_asset = $2`,
			base.String(), quote.String(), usd); err != nil {
			t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	stamp(xlm, usdc, 40)
	stamp(tgt, hopA, 100)
	stamp(hopB, tgt, 1000)
	stamp(tgt, nopx, 5000)
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	got, err := store.TransitiveUSDPriceCandidates(ctx, tgt.String())
	if err != nil {
		t.Fatalf("TransitiveUSDPriceCandidates: %v", err)
	}
	want := []struct{ hop, price string }{
		{hopB.String(), "0.80"},
		{hopA.String(), "0.80"},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want hops %s then %s", got, hopB, hopA)
	}
	for i, w := range want {
		if got[i].Hop != w.hop {
			t.Errorf("candidate %d hop = %s, want %s", i, got[i].Hop, w.hop)
		}
		a, ok1 := new(big.Rat).SetString(got[i].PriceUSD)
		b, _ := new(big.Rat).SetString(w.price)
		if !ok1 || a.Cmp(b) != 0 {
			t.Errorf("candidate %d price = %s, want %s", i, got[i].PriceUSD, w.price)
		}
	}
}

// TestTransitiveUSDPriceCandidates_LatestBucketAcrossDirections pins that
// both the asset->hop leg and the hop's XLM price take the LATEST bucket
// across the two stored directions, the as-is direction winning a tie. A
// preferred direction that went quiet 20h ago must not outrank the other
// direction's fresh bucket.
//
// Fixture (xlm_usd = 0.40 from -30m, before every hop's XLM leg):
//
//	HOPA/XLM 2 (-20m)                       → HOPA = 0.80 USD
//	T1/HOPA 1 (-20h), HOPA/T1 0.5 (-2m)     → T1 = 2 HOPA   = 1.60
//	T3/HOPA 1, HOPA/T3 0.5 (both -15m)      → T3 = 1 HOPA   = 0.80
//	HOPC/XLM 2 (-20h), XLM/HOPC 0.25 (-5m)  → HOPC = 4 XLM  = 1.60; T2/HOPC 1 → 1.60
//	HOPD/XLM 2, XLM/HOPD 0.25 (both -15m)   → HOPD = 2 XLM  = 0.80; T4/HOPD 1 → 0.80
func TestTransitiveUSDPriceCandidates_LatestBucketAcrossDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		hopIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		tgtIssuer  = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	classic := func(code, issuer string) c.Asset {
		t.Helper()
		a, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	pair := func(base, quote c.Asset) c.Pair {
		t.Helper()
		p, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	xlm := c.NativeAsset()
	usdc := classic("USDC", usdcIssuer)
	hopA := classic("HOPA", hopIssuer)
	hopC := classic("HOPC", hopIssuer)
	hopD := classic("HOPD", hopIssuer)
	t1 := classic("T1", tgtIssuer)
	t2 := classic("T2", tgtIssuer)
	t3 := classic("T3", tgtIssuer)
	t4 := classic("T4", tgtIssuer)

	now := time.Now().UTC().Truncate(time.Minute)
	nonce := 0
	insert := func(ago time.Duration, p c.Pair, base, quote int64) {
		t.Helper()
		nonce++
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", nonce, now.Add(-ago), p, base, quote)); err != nil {
			t.Fatalf("InsertTrade %s: %v", p, err)
		}
	}
	insert(30*time.Minute, pair(xlm, usdc), 1_000_000_000, 400_000_000)
	insert(20*time.Minute, pair(hopA, xlm), 100_000_000, 200_000_000)
	insert(20*time.Hour, pair(t1, hopA), 100_000_000, 100_000_000)
	insert(2*time.Minute, pair(hopA, t1), 200_000_000, 100_000_000)
	insert(15*time.Minute, pair(t3, hopA), 100_000_000, 100_000_000)
	insert(15*time.Minute, pair(hopA, t3), 200_000_000, 100_000_000)
	insert(20*time.Hour, pair(hopC, xlm), 100_000_000, 200_000_000)
	insert(5*time.Minute, pair(xlm, hopC), 400_000_000, 100_000_000)
	insert(15*time.Minute, pair(t2, hopC), 100_000_000, 100_000_000)
	insert(15*time.Minute, pair(hopD, xlm), 100_000_000, 200_000_000)
	insert(15*time.Minute, pair(xlm, hopD), 400_000_000, 100_000_000)
	insert(15*time.Minute, pair(t4, hopD), 100_000_000, 100_000_000)
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 40 WHERE base_asset = 'native' AND quote_asset = $1`, usdc.String()); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	cases := []struct {
		name       string
		asset, hop c.Asset
		price      string
	}{
		{"fresh inverted leg beats stale direct leg", t1, hopA, "1.60"},
		{"same-bucket leg tie takes the direct row", t3, hopA, "0.80"},
		{"fresh XLM/hop beats stale hop/XLM", t2, hopC, "1.60"},
		{"same-bucket hop tie takes hop/XLM", t4, hopD, "0.80"},
	}
	for _, tc := range cases {
		got, err := store.TransitiveUSDPriceCandidates(ctx, tc.asset.String())
		if err != nil {
			t.Fatalf("%s: TransitiveUSDPriceCandidates: %v", tc.name, err)
		}
		if len(got) != 1 || got[0].Hop != tc.hop.String() {
			t.Errorf("%s: candidates = %+v, want one via %s", tc.name, got, tc.hop)
			continue
		}
		a, ok := new(big.Rat).SetString(got[0].PriceUSD)
		b, _ := new(big.Rat).SetString(tc.price)
		if !ok || a.Cmp(b) != 0 {
			t.Errorf("%s: price = %s, want %s", tc.name, got[0].PriceUSD, tc.price)
		}
	}
}
