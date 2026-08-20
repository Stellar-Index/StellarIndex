//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Audit finding B11-F1 — dust trades set OHLC chart extremes
// (docs/operations/finding-dust-trades-set-chart-extremes.md).
//
// The prices_* continuous aggregates built their OHLC extremes with NO size
// filter, so one economically-meaningless fill set high/low for the whole
// bucket. Production symptom (2026-07-17 06:00 UTC): the served XLM/USD low
// was 0.1333333333 — the inverse of a SINGLE `USDC-GA5Z…/native` print of 2
// stroops for 15 stroops (usd_volume $0.00000027, price 7.5). The real market
// low that hour was 0.1822.
//
// Migration 0115 adds a notional floor ($0.01 of usd_volume) to the extremes
// inside the CAGGs — it must be there, not in the serve layer, because the
// CAGG stores only the already-collapsed extremes.
//
// These tests seed the EXACT production shape (a 2↔15-stroop crumb inside an
// otherwise healthy bucket, stored in the REVERSE direction so the serve layer
// inverts it) and assert the corrected numbers, on a real TimescaleDB.

// ohlcDustPair identifies one seeded scenario pair.
type ohlcDustPair struct {
	base  string
	quote string
}

// dustFloorGrains is every price CAGG the notional floor must cover.
var dustFloorGrains = []string{"1m", "15m", "1h", "4h", "1d", "1w", "1mo"}

const (
	// usdcIssuer is the canonical Circle USDC asset id (the pair the
	// production wick was observed on).
	usdcIssuer = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// Scenario pairs reuse the same (valid) issuer with distinct codes so
	// each scenario gets its own row in every grain, at the SAME timestamp —
	// a single bucket per grain, uncontaminated by the other scenarios.
	dustOnlyAsset = "DSTO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	whaleAsset    = "WHAL-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	unpricedAsset = "NUSD-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestOHLCDustFloor_CAGGExtremes proves migration 0115's notional floor on all
// seven price CAGGs, across the four cases the fix has to get right:
//
//	A. mixed bucket   — dust must NOT set high/low/open/close; the normal
//	                    trades must; volume + trade_count keep ALL trades.
//	B. all-dust       — a bucket with only dust must still report an extreme
//	                    (the COALESCE fallback), never NULL.
//	C. legit whale    — a large trade far from VWAP is a REAL market event and
//	                    must be KEPT (we filter on SIZE, never on price
//	                    distance — see the finding's operator DECISION).
//	D. NULL usd_volume — an unpriced pair keeps today's unfiltered behaviour.
func TestOHLCDustFloor_CAGGExtremes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 3h back, snapped to the hour: the 1h bucket is closed (bucket+1h <=
	// now()) so the serve-layer assertion below sees it, and every coarser
	// grain buckets the same instant.
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)

	// ── A. the production shape ────────────────────────────────────────
	// Stored REVERSE (base=USDC, quote=native), exactly as the SDEX decoder
	// recorded the offending trade: price = XLM per USDC ≈ 5.4875, which the
	// serve layer inverts to XLM/USD ≈ 0.1822.
	mixed := ohlcDustPair{base: usdcIssuer, quote: "native"}
	seed(t, db, ctx, mixed, []seedTrade{
		// The crumb: 2 stroops for 15 stroops → price 7.5, $0.00000027.
		// FIRST in the bucket, so it also owns `open` when unfiltered.
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"}, // 5.4875 → open
		{off: 20 * time.Second, base: "10000000", quote: "54885000", usd: "1000"}, // 5.4885 → high
		{off: 30 * time.Second, base: "10000000", quote: "54200000", usd: "1000"}, // 5.42   → low
		{off: 40 * time.Second, base: "10000000", quote: "54500000", usd: "1000"}, // 5.45   → close
		// A second crumb on the other side, LAST in the bucket so it also
		// owns `close` when unfiltered.
		{off: 50 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"}, // 0.001
	}, t0)

	// ── B. all-dust bucket (COALESCE fallback) ─────────────────────────
	dustOnly := ohlcDustPair{base: dustOnlyAsset, quote: "native"}
	seed(t, db, ctx, dustOnly, []seedTrade{
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},          // 7.5
		{off: 30 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"}, // 0.001
	}, t0)

	// ── C. legitimate large trade far from market ──────────────────────
	whale := ohlcDustPair{base: whaleAsset, quote: "native"}
	seed(t, db, ctx, whale, []seedTrade{
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"},    // 5.4875
		{off: 20 * time.Second, base: "10000000", quote: "80000000", usd: "100000"},  // 8.0, $100k
		{off: 30 * time.Second, base: "10000000", quote: "20000000", usd: "100000"},  // 2.0, $100k
		{off: 40 * time.Second, base: "10000000", quote: "54900000", usd: "1000"},    // 5.49
		{off: 50 * time.Second, base: "2", quote: "15", usd: "0.00000027"},           // 7.5 crumb
		{off: 55 * time.Second, base: "10000000", quote: "10000", usd: "0.00000027"}, // 0.001 crumb
	}, t0)

	// ── D. unpriced pair (usd_volume NULL) ─────────────────────────────
	unpriced := ohlcDustPair{base: unpricedAsset, quote: "native"}
	seed(t, db, ctx, unpriced, []seedTrade{
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: ""}, // NULL
		{off: 20 * time.Second, base: "2", quote: "15", usd: ""},              // NULL crumb, 7.5
	}, t0)

	refreshPriceCAGGs(t, db, ctx)

	for _, grain := range dustFloorGrains {
		t.Run("mixed/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, mixed, t0)
			// The crumbs (7.5 and 0.001) must be gone from EVERY extreme.
			assertNumeric(t, "high_price", got.high, "5.4885")
			assertNumeric(t, "low_price", got.low, "5.4200")
			assertNumeric(t, "first_price", got.first, "5.4875")
			assertNumeric(t, "last_price", got.last, "5.4500")
			// …but the dust still counts as volume and as a trade: the floor
			// filters the EXTREMES only, never the volume/count semantics.
			if got.tradeCount != 6 {
				t.Errorf("trade_count = %d, want 6 — the floor must not drop trades from the count", got.tradeCount)
			}
			assertNumeric(t, "volume", got.volume, "50000002")
			assertNumeric(t, "volume_usd", got.volumeUSD, "4000.00000127")
		})

		t.Run("all-dust/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, dustOnly, t0)
			// COALESCE fallback: a bucket with ONLY dust still reports the
			// dust extreme rather than NULL (a NULL here would blank the bar).
			assertNumeric(t, "high_price", got.high, "7.5")
			assertNumeric(t, "low_price", got.low, "0.001")
			assertNumeric(t, "first_price", got.first, "7.5")
			assertNumeric(t, "last_price", got.last, "0.001")
		})

		t.Run("whale/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, whale, t0)
			// A $100k fill at 8.0 (≈1.46× VWAP) is a real market event and
			// MUST show. This is what the removed 2×-VWAP serve-layer band
			// could have clipped, and what a "drop prices far from VWAP"
			// filter would wrongly suppress.
			assertNumeric(t, "high_price", got.high, "8.0")
			assertNumeric(t, "low_price", got.low, "2.0")
		})

		t.Run("unpriced/"+grain, func(t *testing.T) {
			got := readCAGG(t, db, ctx, grain, unpriced, t0)
			// usd_volume IS NULL never satisfies `>= 0.01`, so an entirely
			// unpriced bucket falls through the COALESCE to the unfiltered
			// extreme — today's behaviour, deliberately unchanged (finding
			// open question 2: unpriced pairs keep current behaviour).
			assertNumeric(t, "high_price", got.high, "7.5")
			assertNumeric(t, "low_price", got.low, "5.4875")
		})
	}
}

// TestOHLCDustFloor_ServedSeriesReproducesTheWick is the end-to-end
// reproduction of the reported symptom through the real serve query
// (Store.OHLCSeries — the non-fiat `?interval=` path): the crumb is stored in
// the REVERSE direction, so `1/high_price` becomes the served LOW.
//
// Pre-fix this bar serves low = 0.1333333333 (1/7.5) and high = 1000 (1/0.001).
// Post-fix it serves the real market range, low 0.1822 / high 0.1845 —
// matching the CEX range (0.1822–0.1836) the operator measured for the
// 2026-07-17 06:00 UTC bar.
func TestOHLCDustFloor_ServedSeriesReproducesTheWick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	stored := ohlcDustPair{base: usdcIssuer, quote: "native"}
	seed(t, db, ctx, stored, []seedTrade{
		{off: 1 * time.Second, base: "2", quote: "15", usd: "0.00000027"},
		{off: 10 * time.Second, base: "10000000", quote: "54875000", usd: "1000"},
		{off: 20 * time.Second, base: "10000000", quote: "54885000", usd: "1000"},
		{off: 30 * time.Second, base: "10000000", quote: "54200000", usd: "1000"},
		{off: 40 * time.Second, base: "10000000", quote: "54500000", usd: "1000"},
		{off: 50 * time.Second, base: "10000000", quote: "10000", usd: "0.000001"},
	}, t0)
	refreshPriceCAGGs(t, db, ctx)

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// Requested orientation XLM/USDC — the stored rows are USDC/XLM, so every
	// price is inverted and high↔low swap. This is the production path.
	xlmUSDC, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}

	bars, err := store.OHLCSeries(ctx, xlmUSDC, timescale.HistoryGranularity("1h"),
		t0.Add(-time.Hour), t0.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != 1 {
		t.Fatalf("OHLCSeries returned %d bars, want 1 (the seeded hour)", len(bars))
	}
	b := bars[0]

	// The headline: 0.1333333333 was the served low. It must now be 0.1822.
	const wantLow = 0.18220005 // 1/5.4885
	if got := mustFloat(t, b.Low); !closeTo(got, wantLow, 1e-6) {
		t.Errorf("served low = %s, want ~%.8f (1/5.4885, the real market low).\n"+
			"0.1333333333 here is the B11-F1 dust wick (1/7.5, a 2↔15-stroop crumb)", b.Low, wantLow)
	}
	const wantHigh = 0.18450184 // 1/5.42
	if got := mustFloat(t, b.High); !closeTo(got, wantHigh, 1e-6) {
		t.Errorf("served high = %s, want ~%.8f (1/5.42); 1000 here is the 0.001 dust print inverted", b.High, wantHigh)
	}
	const wantOpen = 0.18223234 // 1/5.4875
	if got := mustFloat(t, b.Open); !closeTo(got, wantOpen, 1e-6) {
		t.Errorf("served open = %s, want ~%.8f (1/5.4875) — the first NON-dust trade", b.Open, wantOpen)
	}
	const wantClose = 0.18348624 // 1/5.45
	if got := mustFloat(t, b.Close); !closeTo(got, wantClose, 1e-6) {
		t.Errorf("served close = %s, want ~%.8f (1/5.45) — the last NON-dust trade", b.Close, wantClose)
	}
	if b.TradeCount != 6 {
		t.Errorf("served trade_count = %d, want 6 (dust still counts as a trade)", b.TradeCount)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────

// seedTrade is one row to write into `trades`: `off` from the bucket start,
// raw stroop amounts as NUMERIC text, and usd_volume ("" → SQL NULL).
type seedTrade struct {
	off   time.Duration
	base  string
	quote string
	usd   string
}

// seedNonce keeps every seeded trade's primary key distinct across pairs and
// tests within a run.
var seedNonce int

// seed inserts trades directly so usd_volume is exactly what the scenario
// needs (the resolver-driven InsertTrade path derives it instead).
func seed(t *testing.T, db *sql.DB, ctx context.Context, p ohlcDustPair, trades []seedTrade, t0 time.Time) {
	t.Helper()
	for _, tr := range trades {
		seedNonce++
		var usd any
		if tr.usd != "" {
			usd = tr.usd
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO trades
			    (source, ledger, tx_hash, op_index, ts,
			     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
			VALUES ('sdex', $1, $2, 0, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric)`,
			50_000_000+seedNonce,
			fmt.Sprintf("%064x", seedNonce),
			t0.Add(tr.off),
			p.base, p.quote, tr.base, tr.quote, usd,
		); err != nil {
			t.Fatalf("seed trade (%s/%s, +%s): %v", p.base, p.quote, tr.off, err)
		}
	}
}

func refreshPriceCAGGs(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	for _, grain := range dustFloorGrains {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('prices_"+grain+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh prices_%s: %v", grain, err)
		}
	}
}

// caggRow is the subset of a prices_<grain> row these tests assert on.
// Extremes are nullable so a missing COALESCE fallback surfaces as an
// explicit NULL failure rather than a scan error.
type caggRow struct {
	high, low, first, last sql.NullString
	volume, volumeUSD      sql.NullString
	tradeCount             int64
}

func readCAGG(t *testing.T, db *sql.DB, ctx context.Context, grain string, p ohlcDustPair, t0 time.Time) caggRow {
	t.Helper()
	var got caggRow
	// A single seeded instant lands in exactly one bucket per grain, so
	// bucket <= t0 < bucket + grain: select the row covering t0.
	err := db.QueryRowContext(ctx, `
		SELECT high_price::text, low_price::text, first_price::text, last_price::text,
		       volume::text, volume_usd::text, trade_count
		  FROM prices_`+grain+`
		 WHERE base_asset = $1 AND quote_asset = $2
		   AND bucket <= $3
		 ORDER BY bucket DESC
		 LIMIT 1`,
		p.base, p.quote, t0,
	).Scan(&got.high, &got.low, &got.first, &got.last, &got.volume, &got.volumeUSD, &got.tradeCount)
	if err != nil {
		t.Fatalf("read prices_%s for %s/%s: %v", grain, p.base, p.quote, err)
	}
	return got
}

// assertNumeric compares a NUMERIC::text column against an expected decimal
// literal by value (so "5.4200" == "5.42"), failing loudly on NULL.
func assertNumeric(t *testing.T, col string, got sql.NullString, want string) {
	t.Helper()
	if !got.Valid {
		t.Errorf("%s = NULL, want %s — the COALESCE fallback is missing", col, want)
		return
	}
	g := mustFloat(t, got.String)
	w := mustFloat(t, want)
	if !closeTo(g, w, 1e-9*max(1, absFloat(w))) {
		t.Errorf("%s = %s, want %s", col, got.String, want)
	}
}

func closeTo(got, want, tol float64) bool { return absFloat(got-want) <= tol }

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
