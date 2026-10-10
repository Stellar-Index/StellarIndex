//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricelesscoverage"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
