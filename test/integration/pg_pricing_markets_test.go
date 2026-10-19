//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// One market stored in both directions (sdex XLM/USDC and USDC/XLM, soroswap XLM/USDC only, aquarius USDC/XLM only) must
// read as ONE market with all four trades: asset markets count 1 (not 2), top_markets lists USDC once, sdex markets_24h
// is 1, and the pair breakdown keeps aquarius and all of sdex.
func TestMarketIdentity_BothOrientationsAreOneMarket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	native := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := canonical.NewPair(native, usdc)
	usdcXLM, _ := canonical.NewPair(usdc, native)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	for _, tr := range []canonical.Trade{
		mkIntegrationTrade("sdex", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		mkIntegrationTrade("sdex", 2, ts, usdcXLM, 200_000_000, 400_000_000),
		mkIntegrationTrade("soroswap", 3, ts, xlmUSDC, 600_000_000, 300_000_000),
		mkIntegrationTrade("aquarius", 4, ts, usdcXLM, 100_000_000, 200_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Fixture sanity: both stored orientations really are in prices_1m.
	var dirs int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT (base_asset, quote_asset)) FROM prices_1m
		 WHERE base_asset IN ('native', $1) AND quote_asset IN ('native', $1)`,
		usdc.String()).Scan(&dirs); err != nil {
		t.Fatal(err)
	}
	if dirs != 2 {
		t.Fatalf("fixture: prices_1m holds %d orientations of XLM/USDC, want 2", dirs)
	}

	n, err := store.GetAssetMarketsCount(ctx, native.String())
	if err != nil {
		t.Fatalf("GetAssetMarketsCount: %v", err)
	}
	if n != 1 {
		t.Errorf("GetAssetMarketsCount(native) = %d, want 1 (one market, two stored orientations)", n)
	}

	assertOneTopMarket(t, ctx, store, native.String(), usdc.String())
	assertPairSourceStatsBothOrientations(t, ctx, store, native, usdc)

	assetRows, err := store.AssetSourceStats(ctx, canonical.AssetAliasStrings(native))
	if err != nil {
		t.Fatalf("AssetSourceStats: %v", err)
	}
	for _, r := range assetRows {
		if r.MarketsCount24h != 1 {
			t.Errorf("AssetSourceStats(native) %s markets_24h = %d, want 1", r.Source, r.MarketsCount24h)
		}
	}

	row, err := store.GetNativeAssetRow(ctx)
	if err != nil {
		t.Fatalf("GetNativeAssetRow: %v", err)
	}
	if !row.ObservationCountUnmeasured {
		t.Errorf("GetNativeAssetRow must flag observation_count unmeasured, got count %d", row.ObservationCount)
	}
}

func assertOneTopMarket(t *testing.T, ctx context.Context, store *timescale.Store, asset, counterparty string) {
	t.Helper()
	tops, err := store.GetAssetTopMarkets(ctx, asset, 5)
	if err != nil {
		t.Fatalf("GetAssetTopMarkets: %v", err)
	}
	if len(tops) != 1 {
		t.Fatalf("GetAssetTopMarkets(%s) = %d markets, want 1: %+v", asset, len(tops), tops)
	}
	m := tops[0]
	if m.Counterparty != counterparty || m.Side != "base" {
		t.Errorf("top market = %+v, want counterparty %s on side base (XLM is the canonical base)", m, counterparty)
	}
	if m.TradeCount24h != 4 {
		t.Errorf("top market trade_count_24h = %d, want 4 (both orientations)", m.TradeCount24h)
	}
}

func assertPairSourceStatsBothOrientations(t *testing.T, ctx context.Context, store *timescale.Store, base, quote canonical.Asset) {
	t.Helper()
	rows, err := store.PairSourceStats(ctx, canonical.AssetAliasStrings(base), canonical.AssetAliasStrings(quote))
	if err != nil {
		t.Fatalf("PairSourceStats: %v", err)
	}
	want := map[string]int64{"sdex": 2, "soroswap": 1, "aquarius": 1}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.Source] = r.TradeCount24h
		if r.MarketsCount24h != 1 {
			t.Errorf("PairSourceStats %s markets_24h = %d, want 1", r.Source, r.MarketsCount24h)
		}
	}
	for src, n := range want {
		if got[src] != n {
			t.Errorf("PairSourceStats %s trades_24h = %d, want %d (all rows: %v)", src, got[src], n, got)
		}
	}
}

// Per-source breakdown orders by USD volume, underivable volume last, on a fixture where trade-count order disagrees:
// sdex out-trades soroswap at a fraction of its volume, and aquarius trades most but has no USD path.
func TestSourceStats_OrderedByVolume(t *testing.T) {
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
	native := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	eurc, err := canonical.NewClassicAsset("EURC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := canonical.NewPair(native, usdc)
	usdcEURC, _ := canonical.NewPair(usdc, eurc)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	trades := []canonical.Trade{mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 10_000_000_000, 5_000_000_000)}
	for i := range 3 {
		trades = append(trades, mkIntegrationTrade("sdex", 10+i, ts, xlmUSDC, 10_000_000, 5_000_000))
	}
	for i := range 5 {
		trades = append(trades, mkIntegrationTrade("aquarius", 20+i, ts, usdcEURC, 10_000_000, 9_000_000))
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}
	// The breakdowns sum the usd_volume stamped at trade time (0.5 USD/XLM
	// here); aquarius's USDC/EURC legs stay unstamped, so unpriced.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = quote_amount / 1e7 WHERE source IN ('soroswap', 'sdex')`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}

	pairRows, err := store.PairSourceStats(ctx, canonical.AssetAliasStrings(native), canonical.AssetAliasStrings(usdc))
	if err != nil {
		t.Fatalf("PairSourceStats: %v", err)
	}
	assertSourceOrder(t, "PairSourceStats(XLM/USDC)", pairRows, "soroswap", "sdex")

	assetRows, err := store.AssetSourceStats(ctx, canonical.AssetAliasStrings(usdc))
	if err != nil {
		t.Fatalf("AssetSourceStats: %v", err)
	}
	assertSourceOrder(t, "AssetSourceStats(USDC)", assetRows, "soroswap", "sdex", "aquarius")
	if last := assetRows[2]; last.VolumeUSD24h.Valid || last.UnpricedTrades24h != 5 {
		t.Errorf("aquarius volume = %v, unpriced = %d; want NULL, 5", last.VolumeUSD24h, last.UnpricedTrades24h)
	}
}

func assertSourceOrder(t *testing.T, name string, rows []timescale.SourceStats, want ...string) {
	t.Helper()
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.Source+"="+r.VolumeUSD24h.String)
	}
	if len(rows) != len(want) {
		t.Fatalf("%s returned %d sources, want %d: %v", name, len(rows), len(want), got)
	}
	for i, src := range want {
		if rows[i].Source != src {
			t.Fatalf("%s order = %v, want %v (USD volume desc, underivable last)", name, got, want)
		}
	}
}

// Native <-> XLM-SAC swaps in both leg orders fold both legs to `native`, a pair canonical.NewPair refuses: the listings
// must drop it in SQL rather than fail the whole /v1/pools request or skip-and-count it on every /v1/markets read.
func TestMarketsAndPoolsDropSelfFoldedPair(t *testing.T) {
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

	const (
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	now := time.Now().UTC()
	seedGrainTrade(t, ctx, db, 900, "soroswap", now.Add(-5*time.Minute), "native", xlmSAC, "10", "10", "1")
	seedGrainTrade(t, ctx, db, 901, "soroswap", now.Add(-4*time.Minute), xlmSAC, "native", "10", "10", "1")
	seedGrainTrade(t, ctx, db, 902, "soroswap", now.Add(-3*time.Minute), xlmSAC, usdc, "10", "1", "7")
	for _, v := range []string{"prices_1m", "prices_1d", "pools_per_source_1h"} {
		if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('`+v+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", v, err)
		}
	}

	assertNoSelfPair := func(t *testing.T, label string, markets []timescale.Market) {
		t.Helper()
		n := 0
		for _, m := range markets {
			if m.Pair.Base.String() == m.Pair.Quote.String() {
				t.Errorf("%s: self-pair row %s", label, m.Pair.Base)
			}
			if m.Pair.Base.String() == "native" || m.Pair.Quote.String() == "native" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d XLM rows, want only XLM/USDC", label, n)
		}
	}

	for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderVolume24hDesc, timescale.MarketsOrderPair} {
		skipped := testutil.ToFloat64(obs.MarketsSkippedRowsTotal)

		pools, _, err := store.AllPools(ctx, timescale.PoolsFilter{}, "", 100, order)
		if err != nil {
			t.Fatalf("AllPools(order %v): %v", order, err)
		}
		if len(pools) != 1 || pools[0].Pair.Base.String() == pools[0].Pair.Quote.String() {
			t.Errorf("AllPools(order %v) = %d rows, want only soroswap XLM/USDC", order, len(pools))
		}

		bySource, _, err := store.SourceMarkets(ctx, "soroswap", "", 100, order)
		if err != nil {
			t.Fatalf("SourceMarkets(order %v): %v", order, err)
		}
		assertNoSelfPair(t, "SourceMarkets(soroswap)", bySource)

		all, _, err := store.DistinctPairsExt(ctx, "", 100, order)
		if err != nil {
			t.Fatalf("DistinctPairsExt(order %v): %v", order, err)
		}
		assertNoSelfPair(t, "DistinctPairsExt", all)

		if got := testutil.ToFloat64(obs.MarketsSkippedRowsTotal) - skipped; got != 0 {
			t.Errorf("order %v: %v listing rows skipped, want 0 (self-pair reached the scan)", order, got)
		}
	}
}

// /v1/markets and /v1/pools on real TimescaleDB. SDEX keys XLM as `native` and Soroban venues as its SAC, so grouping on
// the stored spelling serves one market as two rows, each with part of the 24h volume, ranked below markets it outsizes.
func TestMarketsAndPoolsFoldSACSpelling(t *testing.T) {
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

	const (
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	now := time.Now().UTC()
	seedGrainTrade(t, ctx, db, 800, "sdex", now.Add(-30*time.Minute), "native", usdc, "10", "1", "10")
	seedGrainTrade(t, ctx, db, 801, "sdex", now.Add(-25*time.Minute), usdc, "native", "2", "20", "20")
	seedGrainTrade(t, ctx, db, 802, "soroswap", now.Add(-20*time.Minute), xlmSAC, usdc, "10", "1", "7")
	// One venue under both spellings: /v1/pools keys rows per venue, so
	// this is the per-source shape of the same split.
	seedGrainTrade(t, ctx, db, 803, "aquarius", now.Add(-15*time.Minute), xlmSAC, usdc, "10", "1", "5")
	seedGrainTrade(t, ctx, db, 804, "aquarius", now.Add(-10*time.Minute), "native", usdc, "10", "1", "3")

	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg (%s): %v", stmt, err)
		}
	}

	// assertOneMarket requires exactly one XLM/USDC row, spelled `native`,
	// carrying every venue's and every spelling's trades.
	assertOneMarket := func(t *testing.T, label string, markets []timescale.Market, count int64, vol float64) {
		t.Helper()
		n := 0
		for _, m := range markets {
			b, q := m.Pair.Base.String(), m.Pair.Quote.String()
			if b == xlmSAC || q == xlmSAC {
				t.Errorf("%s: row %s|%s still carries the SAC spelling", label, b, q)
			}
			if (b == "native" && q == usdc) || (b == usdc && q == "native") {
				n++
				if m.TradeCount24h != count || numeric(t, m.Volume24hUSD) != vol {
					t.Errorf("%s: XLM/USDC = (%d trades, $%v), want (%d, $%v)",
						label, m.TradeCount24h, numeric(t, m.Volume24hUSD), count, vol)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d XLM/USDC rows, want 1 (got %d rows total)", label, n, len(markets))
		}
	}

	for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderVolume24hDesc, timescale.MarketsOrderPair} {
		all, _, err := store.DistinctPairsExt(ctx, "", 100, order)
		if err != nil {
			t.Fatalf("DistinctPairsExt(order %v): %v", order, err)
		}
		assertOneMarket(t, "DistinctPairsExt", all, 5, 45)

		byAsset, _, err := store.AssetMarkets(ctx, "native", "", 100, order)
		if err != nil {
			t.Fatalf("AssetMarkets(order %v): %v", order, err)
		}
		assertOneMarket(t, "AssetMarkets(native)", byAsset, 5, 45)

		bySource, _, err := store.SourceMarkets(ctx, "aquarius", "", 100, order)
		if err != nil {
			t.Fatalf("SourceMarkets(order %v): %v", order, err)
		}
		assertOneMarket(t, "SourceMarkets(aquarius)", bySource, 2, 8)

		pools, _, err := store.AllPools(ctx, timescale.PoolsFilter{}, "", 100, order)
		if err != nil {
			t.Fatalf("AllPools(order %v): %v", order, err)
		}
		type fig struct {
			count int64
			vol   float64
		}
		got := map[string]fig{}
		for _, p := range pools {
			if b, q := p.Pair.Base.String(), p.Pair.Quote.String(); b == xlmSAC || q == xlmSAC {
				t.Errorf("AllPools: %s row %s|%s still carries the SAC spelling", p.Source, b, q)
			}
			if _, dup := got[p.Source]; dup {
				t.Errorf("AllPools(order %v): %s returned twice — alias spellings were not folded", order, p.Source)
			}
			got[p.Source] = fig{p.TradeCount24h, numeric(t, p.Volume24hUSD)}
		}
		want := map[string]fig{"sdex": {2, 30}, "soroswap": {1, 7}, "aquarius": {2, 8}}
		for src, w := range want {
			if got[src] != w {
				t.Errorf("AllPools(order %v): %s = %+v, want %+v", order, src, got[src], w)
			}
		}
	}

	// ?include=inception on the folded row must see history recorded
	// only under the SAC spelling.
	early := now.Add(-72 * time.Hour)
	seedGrainTrade(t, ctx, db, 805, "soroswap", early, xlmSAC, usdc, "10", "1", "1")
	// A vendor daily close the day before is not a trade and must not pull the anchor earlier.
	seedGrainTrade(t, ctx, db, 806, "coingecko_daily", now.Add(-96*time.Hour), xlmSAC, usdc, "10000000", "20000", "0.002")
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1d: %v", err)
	}
	firsts, err := store.FirstTradeBatch(ctx, [][2]string{{"native", usdc}})
	if err != nil {
		t.Fatalf("FirstTradeBatch: %v", err)
	}
	wantDay := early.Truncate(24 * time.Hour)
	if got, ok := firsts["native|"+usdc]; !ok || !got.Equal(wantDay) {
		t.Errorf("FirstTradeBatch(native|USDC) = %v (present %v), want %v", got, ok, wantDay)
	}
}

// An XLM market keyed `crypto:XLM` (as every CEX feed writes it) MUST surface for ?asset=native and vice-versa:
// distinctPairsCommon must not use a scalar `base_asset = $5 OR quote_asset = $5`, which undercounts. The filter binds
// the full alias set with ANY-membership on each leg.
func TestAPI_MarketsAssetFilter_XLMAliasComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	cryptoXLM, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	// The market lives ONLY under the crypto:XLM form (the CEX spelling).
	cexPair, _ := canonical.NewPair(cryptoXLM, usdc)

	t0 := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	for i, tr := range []canonical.Trade{
		mkAPITrade(1, t0.Add(0*time.Minute), cexPair, 1_000_000_000, 12_000_000),
		mkAPITrade(2, t0.Add(5*time.Minute), cexPair, 1_000_000_000, 12_100_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}

	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	srv := v1.New(v1.Options{Markets: apiMarketsAdapter{s: store}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Query the OTHER alias form; a scalar filter returns 0 rows (crypto:XLM omitted).
	var env struct {
		Data []v1.Market `json:"data"`
	}
	getJSON(t, ts.URL+"/v1/markets?asset=native", &env)

	foundXLMLeg := false
	for _, m := range env.Data {
		if m.Base == "crypto:XLM" || m.Quote == "crypto:XLM" {
			foundXLMLeg = true
		}
	}
	if !foundXLMLeg {
		t.Fatalf("?asset=native did not surface the crypto:XLM-keyed market "+
			"(alias-incomplete filter); got %d rows: %+v", len(env.Data), env.Data)
	}
}

// Two properties of the /v1/markets directory read, on real TimescaleDB and CAGGs in r1's state (prices_1d materialized
// only to the PREVIOUS UTC midnight; 6h end_offset + materialized_only, migrations/0147): 1. per-source figures are that
// SOURCE's.
func TestMarketsListingGrain(t *testing.T) {
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

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)
	// Within minutes of UTC midnight, now-2m is yesterday; "today" prints
	// must stay inside today's bucket.
	today := func(ago time.Duration) time.Time {
		if at := now.Add(-ago); !at.Before(dayStart) {
			return at
		}
		return dayStart
	}

	// Pair A: native/USDC on two venues in the same minutes, plus one older print per venue (3 days back: inside the 14d
	// recency window, outside the 24h figures window) so a materialized prices_1d bucket lists both venues. Without them the
	// pair-wide read returns no row and a cross-source sum is indistinguishable from an absent pair.
	seedGrainTrade(t, ctx, db, 1, "sdex", now.Add(-72*time.Hour), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 2, "soroswap", now.Add(-72*time.Hour), "native", usdc, "1", "0.5", "1")
	for i := range 6 {
		seedGrainTrade(t, ctx, db, 10+i, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "100")
	}
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 20+i, "soroswap", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "5")
	}

	// ── pair B: traded yesterday at 10, again today at 20 ─────────────
	seedGrainTrade(t, ctx, db, 30, "sdex", dayStart.Add(-6*time.Hour), "crypto:ETH", "fiat:USD", "1", "10", "10")
	seedGrainTrade(t, ctx, db, 31, "sdex", today(time.Minute), "crypto:ETH", "fiat:USD", "1", "20", "20")

	// ── pair C: first trade is TODAY ──────────────────────────────────
	seedGrainTrade(t, ctx, db, 40, "sdex", today(2*time.Minute), "crypto:SOL", "fiat:USD", "1", "7", "7")

	// prices_1m and pools_per_source_1h are materialized whole (end_offsets 30 s and 5 min); prices_1d only to the previous
	// UTC midnight, the state its 6h end_offset leaves it in for every pair that traded today.
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg (%s): %v", stmt, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, $1::timestamptz)`, dayStart,
	); err != nil {
		t.Fatalf("refresh prices_1d up to %s: %v", dayStart, err)
	}

	t.Run("source figures are that source's own", func(t *testing.T) {
		markets, _, err := store.SourceMarkets(ctx, "soroswap", "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("SourceMarkets: %v", err)
		}
		row := findGrainMarket(t, markets, "native", usdc)
		if row.TradeCount24h != 2 {
			t.Errorf("SourceMarkets(soroswap).trade_count_24h = %d, want 2 (soroswap's own prints; sdex printed 6 more into the same minutes)",
				row.TradeCount24h)
		}
		if got := numeric(t, row.Volume24hUSD); got != 10 {
			t.Errorf("SourceMarkets(soroswap).volume_24h_usd = %v, want 10 (soroswap's own USD volume; the pair's cross-venue total is 610)", got)
		}

		// The same venue's same pair, from the surface that always had
		// the per-source grain. These two disagreed by orders of
		// magnitude; they are now the same CTE.
		pools, _, err := store.AllPools(ctx,
			timescale.PoolsFilter{Sources: []string{"soroswap"}}, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AllPools: %v", err)
		}
		if len(pools) != 1 {
			t.Fatalf("AllPools(soroswap) returned %d rows, want 1", len(pools))
		}
		if pools[0].TradeCount24h != row.TradeCount24h {
			t.Errorf("/v1/markets?source=soroswap trade_count_24h = %d but /v1/pools?source=soroswap = %d — the two surfaces must agree",
				row.TradeCount24h, pools[0].TradeCount24h)
		}
		if numeric(t, pools[0].Volume24hUSD) != numeric(t, row.Volume24hUSD) {
			t.Errorf("/v1/markets?source=soroswap volume_24h_usd = %v but /v1/pools?source=soroswap = %v — the two surfaces must agree",
				numeric(t, row.Volume24hUSD), numeric(t, pools[0].Volume24hUSD))
		}

		// The zero-value filter means "every venue", not "no venue".
		all, _, err := store.AllPools(ctx, timescale.PoolsFilter{}, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AllPools(PoolsFilter{}): %v", err)
		}
		seen := map[string]bool{}
		for _, p := range all {
			seen[p.Source] = true
		}
		if !seen["soroswap"] || !seen["sdex"] {
			t.Errorf("AllPools(PoolsFilter{}) sources = %v, want both soroswap and sdex (unset Sources is no filter)", seen)
		}
	})

	t.Run("last_price is today's price, not yesterday's close", func(t *testing.T) {
		markets, _, err := store.DistinctPairsExt(ctx, "", 500, timescale.MarketsOrderPair)
		if err != nil {
			t.Fatalf("DistinctPairsExt: %v", err)
		}
		row := findGrainMarket(t, markets, "crypto:ETH", "fiat:USD")
		if got := numeric(t, row.LastPrice); got != 20 {
			t.Errorf("last_price = %v, want 20 (today's print; 10 is yesterday's prices_1d close)", got)
		}
	})

	t.Run("a market whose first trade is today is listed", func(t *testing.T) {
		markets, _, err := store.DistinctPairsExt(ctx, "", 500, timescale.MarketsOrderPair)
		if err != nil {
			t.Fatalf("DistinctPairsExt: %v", err)
		}
		row := findGrainMarket(t, markets, "crypto:SOL", "fiat:USD")
		if got := numeric(t, row.LastPrice); got != 7 {
			t.Errorf("last_price = %v, want 7", got)
		}
		if !row.BucketCloseAt.Equal(dayStart) {
			t.Errorf("bucket_close_at = %s, want %s (the day bucket it last traded in)",
				row.BucketCloseAt, dayStart)
		}
	})
}

// seedGrainTrade writes one trade straight to the hypertable so the
// fixture can set usd_volume (the pricing pipeline's output) without
// running the pipeline.
func seedGrainTrade(t *testing.T, ctx context.Context, db *sql.DB, nonce int, source string, ts time.Time, base, quote, baseAmt, quoteAmt, usdVolume string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades
		    (source, ledger, tx_hash, op_index, ts,
		     base_asset, quote_asset, base_amount, quote_amount, usd_volume)
		VALUES ($1, $2, $3, 0, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric)`,
		source, 70_000_000+nonce, fmt.Sprintf("%064x", 700_000+nonce), ts,
		base, quote, baseAmt, quoteAmt, usdVolume,
	); err != nil {
		t.Fatalf("seed trade %d (%s): %v", nonce, source, err)
	}
}

// findGrainMarket locates one pair in a listing regardless of which
// orientation the canonical collapse picked.
func findGrainMarket(t *testing.T, markets []timescale.Market, base, quote string) timescale.Market {
	t.Helper()
	for _, m := range markets {
		b, q := m.Pair.Base.String(), m.Pair.Quote.String()
		if (b == base && q == quote) || (b == quote && q == base) {
			return m
		}
	}
	got := make([]string, 0, len(markets))
	for _, m := range markets {
		got = append(got, m.Pair.Base.String()+"|"+m.Pair.Quote.String())
	}
	t.Fatalf("pair %s|%s is missing from the listing; got %v", base, quote, got)
	return timescale.Market{}
}

// numeric parses a wire-shaped numeric string for comparison. nil is
// reported as 0 so a missing figure fails the same assertion a wrong
// one does.
func numeric(t *testing.T, s *string) float64 {
	t.Helper()
	if s == nil {
		return 0
	}
	v, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		t.Fatalf("un-parseable numeric %q: %v", *s, err)
	}
	return v
}

// Pair-direction canonicalization must reach VWAPsForPair1m, PairMarket and OHLCSeries: each combines BOTH stored
// directions into the requested orientation, inverting flipped rows. SDEX stores XLM/USDC and USDC/XLM separately, so a
// per-direction read misses a flipped-only bucket.
func TestOrientStragglers_CombineBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := canonical.NewPair(canonical.NativeAsset(), usdc) // requested orientation
	usdcXLM, _ := canonical.NewPair(usdc, canonical.NativeAsset()) // the flipped storage direction

	// Anchor two 1-minute buckets ~2h back so both are closed + inside 24h.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	// Bucket A stores the requested direction at price 0.5 USDC/XLM.
	// Bucket B stores ONLY the flipped direction at price 2.0 XLM/USDC —
	// which is the SAME market at 0.5 USDC/XLM once inverted.
	trades := []canonical.Trade{
		mkAPITrade(1, t0, xlmUSDC, 1_000_000, 500_000),                      // price 0.5
		mkAPITrade(2, t0.Add(2*time.Minute), usdcXLM, 1_000_000, 2_000_000), // price 2.0 → 0.5 inverted
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	from := t0.Add(-time.Minute)
	to := t0.Add(3 * time.Minute)

	// ── VWAPsForPair1m ─────────────────────────────────────────────
	vwaps, err := store.VWAPsForPair1m(ctx, xlmUSDC, from, to)
	if err != nil {
		t.Fatalf("VWAPsForPair1m: %v", err)
	}
	if len(vwaps) != 2 {
		t.Fatalf("VWAPsForPair1m returned %d buckets, want 2 (both directions combined): %v", len(vwaps), vwaps)
	}
	for i, v := range vwaps {
		if v < 0.49 || v > 0.51 {
			t.Errorf("VWAPsForPair1m[%d] = %v, want ~0.5 (flipped bucket inverted)", i, v)
		}
	}

	// ── PairMarket ─────────────────────────────────────────────────
	m, ok, err := store.PairMarket(ctx, canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("PairMarket: %v", err)
	}
	if !ok {
		t.Fatal("PairMarket ok=false, want true")
	}
	if m.TradeCount24h != 2 {
		t.Errorf("PairMarket TradeCount24h = %d, want 2 (both directions)", m.TradeCount24h)
	}
	if m.LastPrice == nil {
		t.Fatal("PairMarket LastPrice = nil, want the inverted flipped-bucket price")
	}
	if lp := mustFloat(t, *m.LastPrice); lp < 0.49 || lp > 0.51 {
		t.Errorf("PairMarket LastPrice = %s, want ~0.5 (latest bucket is flipped, inverted)", *m.LastPrice)
	}

	// ── OHLCSeries ─────────────────────────────────────────────────
	bars, err := store.OHLCSeries(ctx, xlmUSDC, timescale.HistoryGranularity("1m"), from, to, 0)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("OHLCSeries returned %d bars, want 2 (both directions combined): %+v", len(bars), bars)
	}
	// The second bar is the flipped-only bucket — its OHLC must be inverted.
	b := bars[1]
	for name, s := range map[string]string{"open": b.Open, "high": b.High, "low": b.Low, "close": b.Close} {
		if px := mustFloat(t, s); px < 0.49 || px > 0.51 {
			t.Errorf("OHLCSeries flipped bar %s = %s, want ~0.5 (inverted from 2.0)", name, s)
		}
	}
	if b.TradeCount != 1 {
		t.Errorf("OHLCSeries flipped bar TradeCount = %d, want 1", b.TradeCount)
	}

	// ── TimedVWAPsForPair1m (anomaly baseline) ─────────────────────
	timed, err := store.TimedVWAPsForPair1m(ctx, xlmUSDC, from, to)
	if err != nil {
		t.Fatalf("TimedVWAPsForPair1m: %v", err)
	}
	if len(timed) != 2 {
		t.Fatalf("TimedVWAPsForPair1m returned %d points, want 2 (both directions)", len(timed))
	}
	for i, tv := range timed {
		if tv.VWAP < 0.49 || tv.VWAP > 0.51 {
			t.Errorf("TimedVWAPsForPair1m[%d].VWAP = %v, want ~0.5", i, tv.VWAP)
		}
	}

	// OHLCSeriesReBucketed (5m fold): both source buckets combine and invert first, then fold into the 5m grid. Assert on
	// totals so the check is independent of which 5m bucket each minute lands in.
	rb, err := store.OHLCSeriesReBucketed(ctx, xlmUSDC, timescale.HistoryGranularity("1m"), "5 minutes", from, to, 0)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	if len(rb) == 0 {
		t.Fatal("OHLCSeriesReBucketed returned no bars, want the flipped bucket combined in")
	}
	var rbTrades int64
	for _, bar := range rb {
		rbTrades += bar.TradeCount
		if px := mustFloat(t, bar.Close); px < 0.49 || px > 0.51 {
			t.Errorf("OHLCSeriesReBucketed bar Close = %s, want ~0.5 (inverted)", bar.Close)
		}
	}
	if rbTrades != 2 {
		t.Errorf("OHLCSeriesReBucketed total TradeCount = %d, want 2 (both directions)", rbTrades)
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("ParseFloat(%q): %v", s, err)
	}
	return f
}

const usdcSACForm = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

func openOrientationStore(t *testing.T, ctx context.Context, dsn string) (*timescale.Store, *sql.DB) {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, store.DB()
}

func refreshPrices1m(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
}

// soleTopMarket returns the asset's only top market, failing otherwise.
func soleTopMarket(t *testing.T, ctx context.Context, store *timescale.Store, assetID string) timescale.AssetTopMarket {
	t.Helper()
	tops, err := store.GetAssetTopMarkets(ctx, assetID, 5)
	if err != nil {
		t.Fatalf("GetAssetTopMarkets(%s): %v", assetID, err)
	}
	if len(tops) != 1 {
		t.Fatalf("GetAssetTopMarkets(%s) = %+v, want exactly one market", assetID, tops)
	}
	return tops[0]
}

// A market traded through a declared USDC SAC must orient like
// native/USDC in SQL as it does in canonical.Orient: USDC is the quote.
func TestAssetTopMarkets_DeclaredStablecoinSACIsQuote(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase,
		map[string]string{usdcSACForm: "USDC:" + realIssuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	seedGrainTrade(t, ctx, db, 1, "soroswap", time.Now().UTC().Add(-30*time.Minute),
		canonical.XLMSacContractID, usdcSACForm, "10", "1", "1")
	refreshPrices1m(t, ctx, db)

	if _, quote, _ := canonical.Orient(canonical.XLMSacContractID, usdcSACForm); quote != usdcSACForm {
		t.Fatalf("precondition: canonical.Orient quote = %s, want the USDC SAC", quote)
	}
	m := soleTopMarket(t, ctx, store, "USDC-"+realIssuer)
	if m.Side != "quote" || m.Counterparty != canonical.XLMSacContractID {
		t.Errorf("USDC top market = %+v, want side quote against the XLM SAC", m)
	}
}

// On a database whose default collation is not byte order, a rank tie
// must still break the way canonical.Orient breaks it.
func TestAssetTopMarkets_TieBreakIsByteOrderUnderICUCollation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE icu_en_us TEMPLATE template0
		LOCALE_PROVIDER icu ICU_LOCALE 'en-US' LOCALE 'C'`); err != nil {
		t.Fatalf("create ICU database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/icu_en_us"
	icuDSN := u.String()
	icu, err := sql.Open("pgx", icuDSN)
	if err != nil {
		t.Fatalf("open ICU database: %v", err)
	}
	t.Cleanup(func() { _ = icu.Close() })
	if _, err := icu.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		t.Fatalf("enable timescaledb: %v", err)
	}
	var localeGreater bool
	if err := icu.QueryRowContext(ctx, `SELECT 'yX'::text > 'ZZ'::text`).Scan(&localeGreater); err != nil {
		t.Fatalf("collation probe: %v", err)
	}
	if localeGreater {
		t.Fatalf("instrument: ICU database compares 'yX' > 'ZZ' in byte order; the tie would not be exercised")
	}

	applyMigrations(t, icuDSN)
	store, db := openOrientationStore(t, ctx, icuDSN)
	const lower = "yXLM-" + realIssuer
	const upper = "ZZZ-" + realIssuer
	seedGrainTrade(t, ctx, db, 1, "sdex", time.Now().UTC().Add(-30*time.Minute), upper, lower, "1", "1", "1")
	refreshPrices1m(t, ctx, db)

	if _, quote, _ := canonical.Orient(upper, lower); quote != lower {
		t.Fatalf("precondition: canonical.Orient quote = %s, want %s", quote, lower)
	}
	if m := soleTopMarket(t, ctx, store, lower); m.Side != "quote" {
		t.Errorf("%s top market = %+v, want side quote as canonical.Orient decides", lower, m)
	}
}

// A Soroban-native token whose own contract address carries a scam-class
// directory tag ranks in the flagged tier, like a flagged issuer's asset.
func TestListAssets_FlaggedContractTokenRanksFlaggedTier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	canonical.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	const flagged = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	const clean = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	for _, id := range []string{flagged, clean} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO discovered_assets
			    (contract_id, first_seen_at, first_seen_ledger, first_seen_event,
			     last_seen_at, last_seen_ledger, event_count)
			VALUES ($1, now(), 1, 'transfer', now(), 2, 5)`, id); err != nil {
			t.Fatalf("seed discovered_assets %s: %v", id, err)
		}
		seedRawVolume(t, ctx, db, id, "5000")
	}
	seedDirectoryTags(t, ctx, db, flagged, "Example Token", []string{"malicious"})

	tiers := map[string]int{}
	for _, r := range listRankOrder(t, ctx, store, timescale.AssetsOrderObservationCountDesc, 50) {
		if r.RankTier != nil {
			tiers[r.AssetID] = *r.RankTier
		}
	}
	if tiers[flagged] != 2 {
		t.Errorf("flagged contract token rank tier = %d (present=%v), want 2", tiers[flagged], tiers)
	}
	if tier, ok := tiers[clean]; !ok || tier != 0 {
		t.Errorf("unlisted contract token rank tier = %d (present=%v), want 0", tier, ok)
	}
}

// quoteRankSQL must rank the off-chain `crypto:XLM` as XLM (rank 2) like "native" and the SAC, not as an ordinary token
// (rank 1). Stored as (base=crypto:XRP, quote=crypto:XLM): a rank tie would break on string comparison and swap which
// leg is base and quote.
func TestDistinctPairsRanksCryptoXLMAsQuote(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xrp := canonical.Asset{Type: canonical.AssetCrypto, Code: "XRP"}
	xlm := canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"}
	pair, err := canonical.NewPair(xrp, xlm)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	tr := mkAPITrade(1, now.Add(-5*time.Minute), pair, 1_000_000, 500_000)
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	rows, _, err := store.DistinctPairs(ctx, "", 50)
	if err != nil {
		t.Fatalf("DistinctPairs: %v", err)
	}
	// The listing folds XLM spellings, so the quote may come back
	// as any of XLM's alias forms; what this test pins is the orientation.
	xlmForms := map[string]bool{}
	for _, a := range canonical.AssetAliases(xlm) {
		xlmForms[a.String()] = true
	}
	var found bool
	for _, m := range rows {
		if m.Pair.Base.String() != xrp.String() && m.Pair.Quote.String() != xrp.String() {
			continue
		}
		found = true
		if m.Pair.Base.String() != xrp.String() || !xlmForms[m.Pair.Quote.String()] {
			t.Errorf("canonical pair = (%s, %s), want (%s, an XLM form of %s) — XLM must rank as quote",
				m.Pair.Base.String(), m.Pair.Quote.String(), xrp.String(), xlm.String())
		}
	}
	if !found {
		t.Fatalf("DistinctPairs did not return the seeded XRP/XLM market: %+v", rows)
	}
}

// Point-in-time substance SQL on real TimescaleDB, then the thin-market gate end to end. /v1/price/at and
// /v1/price/changes serve a past bucket but the gate measured the trailing 24h ending NOW. DEEP/native (deep 400 days
// ago, no trades since) must not lose its history.
func TestPairMarketSubstanceAt_MeasuresTheMarketAtTheInstant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	deep, err := canonical.NewClassicAsset("DEEP", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := canonical.NewClassicAsset("SEED", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	deepXLM, _ := canonical.NewPair(deep, canonical.NativeAsset())
	xlmDeep, _ := canonical.NewPair(canonical.NativeAsset(), deep) // the flipped stored direction
	seedXLM, _ := canonical.NewPair(seed, canonical.NativeAsset())

	now := time.Now().UTC().Truncate(time.Minute)
	then := now.Add(-400 * 24 * time.Hour).Truncate(time.Hour)

	var trades []canonical.Trade
	nonce := 0
	add := func(ts time.Time, pair canonical.Pair) {
		nonce++
		trades = append(trades, mkAPITrade(nonce, ts, pair, 1_000_000, 500_000))
	}
	// DEEP, historical: 24 trades, one every 20 min from then−9h to
	// then−1h20m, alternating stored direction → 8 distinct hour buckets
	// (then−9h … then−2h), span 7h.
	for i := 0; i < 24; i++ {
		pair := deepXLM
		if i%2 == 1 {
			pair = xlmDeep
		}
		add(then.Add(-9*time.Hour+time.Duration(i)*20*time.Minute), pair)
	}
	add(then.Add(-25*time.Hour), deepXLM)  // decoy: one bucket BEFORE the window opens
	add(then.Add(30*time.Minute), deepXLM) // decoy: the bucket still OPEN at the instant
	add(then.Add(-3*time.Hour), seedXLM)   // SEED, historical: a single burst
	for i := 0; i < 24; i++ {              // SEED, today: 24 minutes, now−10h … now−2h20m
		add(now.Add(-10*time.Hour+time.Duration(i)*20*time.Minute), seedXLM)
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	// Stamp the dollar leg directly: what is under test is the window,
	// not the insert-time valuation rules.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 8.57 WHERE source = 'integ-api' AND base_asset = $1 AND ts < $2::timestamptz`,
		seed.String(), now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("stamp seed usd_volume: %v", err)
	}
	for _, view := range []string{"prices_1m", "prices_1h"} {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('`+view+`', NULL, NULL)`); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}

	day := 24 * time.Hour
	measure := func(pair canonical.Pair, asOf time.Time, g timescale.HistoryGranularity) timescale.MarketSubstance {
		t.Helper()
		sub, err := store.PairMarketSubstanceAt(ctx, canonical.AssetAliases(pair.Base), canonical.AssetAliases(pair.Quote), asOf, day, g)
		if err != nil {
			t.Fatalf("PairMarketSubstanceAt(%s, %s, %s): %v", pair, asOf, g, err)
		}
		return sub
	}
	wantSub := func(name string, got timescale.MarketSubstance, volume float64, buckets, span int64) {
		t.Helper()
		if v := mustFloat(t, got.VolumeUSD); v != volume || got.Buckets != buckets || got.SpanSeconds != span {
			t.Errorf("%s: got {volume %s, buckets %d, span %ds}, want {volume %v, buckets %d, span %ds}",
				name, got.VolumeUSD, got.Buckets, got.SpanSeconds, volume, buckets, span)
		}
	}

	// ── the SQL: the window sits at the instant ────────────────────────
	wantSub("DEEP at then, hour grain — both directions, decoys excluded",
		measure(deepXLM, then, timescale.Granularity1h), 2400, 8, 7*3600)
	wantSub("DEEP at then, asked in the flipped orientation",
		measure(xlmDeep, then, timescale.Granularity1h), 2400, 8, 7*3600)
	wantSub("SEED at then, hour grain — the single burst",
		measure(seedXLM, then, timescale.Granularity1h), 8.57, 1, 0)
	wantSub("SEED at now−1h, minute grain — today's market",
		measure(seedXLM, now.Add(-time.Hour), timescale.Granularity1m), 2400, 24, 460*60)
	// Moving the instant moves the window: at now−5h only the trades up
	// to now−5h−1m had closed (i ≤ 14 → 15 minutes, span 280 min).
	wantSub("SEED at now−5h, minute grain — the window moved with the instant",
		measure(seedXLM, now.Add(-5*time.Hour), timescale.Granularity1m), 1500, 15, 280*60)
	wantSub("DEEP at now, minute grain — dormant today",
		measure(deepXLM, now, timescale.Granularity1m), 0, 0, 0)

	// The trailing reader agrees with the point-in-time one AT now — the
	// new query is the old one with the window moved, nothing else.
	live, err := store.PairMarketSubstance(ctx, canonical.AssetAliases(seedXLM.Base), canonical.AssetAliases(seedXLM.Quote), day)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	wantSub("SEED trailing-from-now", live, 2400, 24, 460*60)

	// ── the gate, end to end over the real store ───────────────────────
	gate := pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{})
	if gate.Allowed(ctx, deep, canonical.NativeAsset(), "test") {
		t.Error("fixture: DEEP must be below the LIVE floor (no trades for 400 days)")
	}
	if !gate.AllowedAt(ctx, deep, canonical.NativeAsset(), then, "test") {
		t.Error("DEEP at `then` was withheld: its market cleared every leg of the floor at that " +
			"instant, and being dormant today is not a reason to refuse its history")
	}
	if !gate.Allowed(ctx, seed, canonical.NativeAsset(), "test") {
		t.Error("fixture: SEED must clear the LIVE floor today")
	}
	if gate.AllowedAt(ctx, seed, canonical.NativeAsset(), then, "test") {
		t.Error("SEED at `then` was served: its market at that instant was one $8.57 burst, and " +
			"being thick today is not a reason to publish the seeded price")
	}
	if !gate.AllowedAt(ctx, seed, canonical.NativeAsset(), now.Add(-time.Hour), "test") {
		t.Error("SEED at now−1h was withheld though its recent market clears the minute floor")
	}
}

// Served values of the /v1/pairs single-pair summary across the bounds the query uses: both stored orientations fold
// into ONE row (count_24h sums them); count_24h counts only the trailing 24h though the pair is active across the 14-day
// recency window.
func TestPairMarket_BothDirectionsAndWindowBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	fwd, err := canonical.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := canonical.NewPair(usdc, xlm)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	// newest is the last trade inside the recency window.
	newest := now.Add(-90 * time.Second)

	seed := []struct {
		nonce int
		ts    time.Time
		pair  canonical.Pair
	}{
		// Inside 24h — 4 forward, 2 flipped. count_24h must be 6.
		{1, newest, fwd},
		{2, now.Add(-2 * time.Hour), fwd},
		{3, now.Add(-9 * time.Hour), fwd},
		{4, now.Add(-23 * time.Hour), fwd},
		{5, now.Add(-30 * time.Minute), rev},
		{6, now.Add(-20 * time.Hour), rev},
		// Inside the 14d recency window but OUTSIDE 24h — these keep the
		// pair visible but must NOT be counted.
		{7, now.Add(-2 * 24 * time.Hour), fwd},
		{8, now.Add(-5 * 24 * time.Hour), fwd},
		{9, now.Add(-13 * 24 * time.Hour), rev},
		// Outside the 14d recency window entirely.
		{10, now.Add(-20 * 24 * time.Hour), fwd},
	}
	for _, s := range seed {
		if err := store.InsertTrade(ctx, mkAPITrade(s.nonce, s.ts, s.pair, 1_000_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", s.nonce, err)
		}
	}

	m, found, err := store.PairMarket(ctx, xlm, usdc)
	if err != nil {
		t.Fatalf("PairMarket: %v", err)
	}
	if !found {
		t.Fatal("PairMarket reported the pair as untraded; 9 trades sit inside " +
			"MarketsRecencyWindow across both orientations")
	}
	if got := m.LastTradeAt.UTC(); !got.Equal(newest) {
		t.Errorf("last_trade_at = %s, want %s (the exact newest trade inside the "+
			"recency window, second-precision — /v1/pairs does NOT round to a CAGG "+
			"bucket the way /v1/markets does)", got, newest)
	}
	if m.TradeCount24h != 6 {
		t.Errorf("count_24h = %d, want 6 (4 forward + 2 flipped inside 24h). A "+
			"count of 4 means the flipped orientation was dropped; a count of 9 "+
			"means the 24-hour bound was widened to the recency window",
			m.TradeCount24h)
	}
	if m.Pair.Base.String() != xlm.String() || m.Pair.Quote.String() != usdc.String() {
		t.Errorf("pair = %s/%s, want the REQUESTED orientation %s/%s",
			m.Pair.Base, m.Pair.Quote, xlm, usdc)
	}

	// The reciprocal request folds the same two orientations and reports
	// the same activity, echoed in ITS requested orientation.
	rm, rfound, err := store.PairMarket(ctx, usdc, xlm)
	if err != nil {
		t.Fatalf("PairMarket reciprocal: %v", err)
	}
	if !rfound || rm.TradeCount24h != 6 || !rm.LastTradeAt.UTC().Equal(newest) {
		t.Errorf("reciprocal orientation disagrees: found=%v count_24h=%d last=%s; "+
			"want true/6/%s — both directions must fold the same way whichever way "+
			"round the caller asks", rfound, rm.TradeCount24h, rm.LastTradeAt.UTC(), newest)
	}

	// A pair whose ONLY trade is older than MarketsRecencyWindow stays invisible: the 14-day bound on the last_trade_at
	// probes is the recency gate; dropping it to 24h would hide every pair idle for a day.
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	dormant, err := canonical.NewPair(aqua, usdc)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTrade(ctx, mkAPITrade(11, now.Add(-20*24*time.Hour), dormant, 1_000_000_000, 12_000_000)); err != nil {
		t.Fatalf("InsertTrade dormant: %v", err)
	}
	if _, ok, err := store.PairMarket(ctx, aqua, usdc); err != nil {
		t.Fatalf("PairMarket dormant: %v", err)
	} else if ok {
		t.Error("a pair whose only trade predates MarketsRecencyWindow was reported " +
			"as traded; /v1/pairs must stay consistent with DistinctPairs")
	}

	// And a pair idle for 3 days but active inside the window IS visible,
	// with count_24h = 0 rather than a miss.
	idle, err := canonical.NewPair(usdc, aqua)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTrade(ctx, mkAPITrade(12, now.Add(-3*24*time.Hour), idle, 1_000_000_000, 12_000_000)); err != nil {
		t.Fatalf("InsertTrade idle: %v", err)
	}
	im, ifound, err := store.PairMarket(ctx, usdc, aqua)
	if err != nil {
		t.Fatalf("PairMarket idle: %v", err)
	}
	if !ifound {
		t.Fatal("a pair active 3 days ago is inside MarketsRecencyWindow and must " +
			"still be served")
	}
	if im.TradeCount24h != 0 {
		t.Errorf("idle pair count_24h = %d, want 0", im.TradeCount24h)
	}
}

// Seeds two markets into prices_1m: TWO/native trading both ways in one closed minute, and FLIP/native stored ONLY as
// (native, FLIP). Every trade prices the non-native leg at 0.5 native with $100 of USD volume.
type orientationFixture struct {
	store              *timescale.Store
	twoXLM, flipXLM    canonical.Pair
	twoMinute, flipMin time.Time
}

func seedOrientationFixture(ctx context.Context, t *testing.T) orientationFixture {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	two, err := canonical.NewClassicAsset("TWO", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	flip, err := canonical.NewClassicAsset("FLIP", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	f := orientationFixture{store: store}
	f.twoXLM, _ = canonical.NewPair(two, canonical.NativeAsset())
	xlmTwo, _ := canonical.NewPair(canonical.NativeAsset(), two)
	f.flipXLM, _ = canonical.NewPair(flip, canonical.NativeAsset())
	xlmFlip, _ := canonical.NewPair(canonical.NativeAsset(), flip)

	now := time.Now().UTC().Truncate(time.Minute)
	f.twoMinute = now.Add(-30 * time.Minute)
	f.flipMin = now.Add(-20 * time.Minute)
	for _, tr := range []canonical.Trade{
		mkAPITrade(1, f.twoMinute.Add(5*time.Second), f.twoXLM, 1_000_000, 500_000),
		mkAPITrade(2, f.twoMinute.Add(10*time.Second), xlmTwo, 500_000, 1_000_000),
		mkAPITrade(3, f.flipMin.Add(5*time.Second), xlmFlip, 500_000, 1_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 100 WHERE source = 'integ-api'`); err != nil {
		t.Fatalf("stamp usd_volume: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}
	return f
}

// The six pair readers whose orientation fold moved from one OR to two UNION ALL arms must still read both stored
// directions: a flipped-only market is served, a two-sided one is counted once per row, an absent pair is a clean miss.
func TestPairReadersFoldBothOrientationsAsUnion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := seedOrientationFixture(ctx, t)
	near := func(name, got string, want float64) {
		t.Helper()
		if v := mustFloat(t, got); v < want-1e-9 || v > want+1e-9 {
			t.Errorf("%s = %s, want %v", name, got, want)
		}
	}

	latest, err := f.store.LatestClosedVWAP1mForPair(ctx, f.flipXLM)
	if err != nil {
		t.Fatalf("LatestClosedVWAP1mForPair(flipped-only): %v", err)
	}
	near("latest VWAP, flipped-only market", latest.VWAP, 0.5)

	at, err := f.store.ClosedVWAPAtOrBefore(ctx, f.flipXLM, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("ClosedVWAPAtOrBefore(flipped-only): %v", err)
	}
	near("VWAP at-or-before, flipped-only market", at.VWAP, 0.5)
	if !at.Bucket.Equal(f.flipMin) {
		t.Errorf("at-or-before bucket = %s, want %s", at.Bucket, f.flipMin)
	}

	pts, err := f.store.TimedVWAPs1mForChangeSummary(ctx, f.twoXLM, f.twoMinute.Add(-time.Hour), f.twoMinute.Add(time.Minute))
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("change-summary points = %d, want 1 (both directions of one bucket fold to one point)", len(pts))
	}
	near("change-summary VWAP, two-sided bucket", pts[0].Value, 0.5)

	sub, err := f.store.PairMarketSubstance(ctx, canonical.AssetAliases(f.twoXLM.Base), canonical.AssetAliases(f.twoXLM.Quote), 24*time.Hour)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	if mustFloat(t, sub.VolumeUSD) != 200 || sub.Buckets != 1 {
		t.Errorf("substance = {%s, %d buckets}, want {200, 1}: both directions, one bucket", sub.VolumeUSD, sub.Buckets)
	}
	subAt, err := f.store.PairMarketSubstanceAt(ctx, canonical.AssetAliases(f.flipXLM.Base), canonical.AssetAliases(f.flipXLM.Quote),
		time.Now().UTC(), 24*time.Hour, timescale.Granularity1m)
	if err != nil {
		t.Fatalf("PairMarketSubstanceAt: %v", err)
	}
	if mustFloat(t, subAt.VolumeUSD) != 100 || subAt.Buckets != 1 {
		t.Errorf("substance-at = {%s, %d buckets}, want {100, 1} from the flipped row", subAt.VolumeUSD, subAt.Buckets)
	}

	absent, _ := canonical.NewPair(f.twoXLM.Base, f.flipXLM.Base)
	if _, err := f.store.LatestClosedVWAP1mForPair(ctx, absent); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("absent pair: err = %v, want sql.ErrNoRows", err)
	}
}

// The /v1/markets sparkline read must match the listing it decorates. The handler keys the batch by the listing's
// CANONICAL rows; FLIP/native is stored only as (native, FLIP), so reading the stored key alone drew 24 zero bars beside
// a $100 volume_24h_usd. All trades sit in the last hour, so the 24 hourly bars must sum to the headline.
func TestMarketsSparklineMatchesListingVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := seedOrientationFixture(ctx, t)

	xlmSAC, err := canonical.NewSorobanAsset(canonical.XLMSacContractID)
	if err != nil {
		t.Fatal(err)
	}
	flipSAC, _ := canonical.NewPair(f.flipXLM.Base, xlmSAC)
	sacTrade := mkAPITrade(4, f.flipMin.Add(15*time.Second), flipSAC, 1_000_000, 500_000)
	if err := f.store.InsertTrade(ctx, sacTrade); err != nil {
		t.Fatalf("InsertTrade(SAC leg): %v", err)
	}
	if _, err := f.store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 50 WHERE tx_hash = $1`, sacTrade.TxHash); err != nil {
		t.Fatalf("stamp SAC-leg usd_volume: %v", err)
	}
	if _, err := f.store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	rows, _, err := f.store.DistinctPairs(ctx, "", 50)
	if err != nil {
		t.Fatalf("DistinctPairs: %v", err)
	}
	rat := func(s string) *big.Rat {
		t.Helper()
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("not a decimal: %q", s)
		}
		return r
	}
	want := map[string]*big.Rat{}
	var pairs [][2]string
	for _, m := range rows {
		if m.Volume24hUSD == nil {
			continue
		}
		key := m.Pair.Base.String() + "|" + m.Pair.Quote.String()
		want[key] = rat(*m.Volume24hUSD)
		pairs = append(pairs, [2]string{m.Pair.Base.String(), m.Pair.Quote.String()})
	}
	flipKey := f.flipXLM.Base.String() + "|" + f.flipXLM.Quote.String()
	twoKey := f.twoXLM.Base.String() + "|" + f.twoXLM.Quote.String()
	if want[flipKey] == nil || want[flipKey].Cmp(big.NewRat(150, 1)) != 0 ||
		want[twoKey] == nil || want[twoKey].Cmp(big.NewRat(200, 1)) != 0 {
		t.Fatalf("fixture: listing volumes = %v, want %s=150 and %s=200", want, flipKey, twoKey)
	}

	hist, err := f.store.GetPairsVolumeHistory24hBatch(ctx, pairs)
	if err != nil {
		t.Fatalf("GetPairsVolumeHistory24hBatch: %v", err)
	}
	for key, headline := range want {
		series := hist[key]
		if len(series) != 24 {
			t.Errorf("%s: %d bars, want 24", key, len(series))
			continue
		}
		sum := new(big.Rat)
		for _, p := range series {
			sum.Add(sum, rat(p.VolumeUSD))
		}
		if sum.Cmp(headline) != 0 {
			t.Errorf("%s: sparkline sums to %s, listing volume_24h_usd is %s",
				key, sum.FloatString(7), headline.FloatString(7))
		}
	}
}

// A bucket fed only by a vendor daily close is not a trade: the point-in-time VWAP walks past it on every rung and
// serves the nearest fill-derived bucket instead, or nothing.
func TestClosedVWAPAtOrBefore_SkipsVendorOnlyBuckets(t *testing.T) {
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

	usdcAsset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdcAsset)
	if err != nil {
		t.Fatal(err)
	}
	refresh := func() {
		t.Helper()
		for _, v := range []string{"prices_1m", "prices_15m", "prices_1h", "prices_4h", "prices_1d"} {
			if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('`+v+`', NULL, NULL)`); err != nil {
				t.Fatalf("refresh %s: %v", v, err)
			}
		}
	}

	now := time.Now().UTC().Truncate(time.Minute)
	asOf := now.Add(-29 * time.Hour)
	vendorTS := now.Add(-30 * time.Hour)
	seedGrainTrade(t, ctx, db, 807, "coingecko_daily", vendorTS, pair.Base.String(), pair.Quote.String(), "10000000", "20000", "0.002")
	refresh()
	if _, err := store.ClosedVWAPAtOrBefore(ctx, pair, asOf, 2*time.Hour); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ClosedVWAPAtOrBefore over a vendor-only bucket: err = %v, want sql.ErrNoRows", err)
	}

	venueTS := now.Add(-33 * time.Hour)
	seedGrainTrade(t, ctx, db, 808, "sdex", venueTS, pair.Base.String(), pair.Quote.String(), "10", "5", "1")
	refresh()
	at, err := store.ClosedVWAPAtOrBefore(ctx, pair, asOf, 6*time.Hour)
	if err != nil {
		t.Fatalf("ClosedVWAPAtOrBefore with a fill 4h earlier: %v", err)
	}
	if !at.Bucket.Equal(venueTS) {
		t.Errorf("bucket = %s, want the fill minute %s, not the vendor close at %s", at.Bucket, venueTS, vendorTS)
	}
	got := new(big.Rat)
	if _, ok := got.SetString(at.VWAP); !ok || got.Cmp(big.NewRat(1, 2)) != 0 {
		t.Errorf("VWAP = %q, want 0.5 from the fill, not the vendor close", at.VWAP)
	}
}

func TestTradesInRange_SkipsVendorRows(t *testing.T) {
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

	usdcAsset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdcAsset)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	seedGrainTrade(t, ctx, db, 809, "coingecko_daily", now.Add(-30*time.Hour), pair.Base.String(), pair.Quote.String(), "10000000", "20000", "0.002")
	seedGrainTrade(t, ctx, db, 810, "sdex", now.Add(-31*time.Hour), pair.Base.String(), pair.Quote.String(), "10", "5", "1")

	trades, err := store.TradesInRange(ctx, pair, now.Add(-32*time.Hour), now.Add(-29*time.Hour), 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(trades) != 1 || trades[0].Source != "sdex" {
		t.Fatalf("trades = %+v, want only the sdex fill; a vendor daily close is not a trade", trades)
	}
}

// TestPoolsLastPriceTieIsDeterministic: two spellings of one market folded into a single pool can close
// their buckets in the same ledger, at the same ts. The tie breaks on the higher close whatever order the
// rows were written in, so the served last_price does not flip between queries.
func TestPoolsLastPriceTieIsDeterministic(t *testing.T) {
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

	const (
		usdc   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	)
	ts := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	// Same ts, both write orders: the native spelling's 0.2 is the higher close, so it wins.
	seedGrainTrade(t, ctx, db, 900, "aquarius", ts, "native", usdc, "10", "2", "2")
	seedGrainTrade(t, ctx, db, 901, "aquarius", ts, xlmSAC, usdc, "10", "1", "1")
	seedGrainTrade(t, ctx, db, 902, "phoenix", ts, xlmSAC, usdc, "10", "1", "1")
	seedGrainTrade(t, ctx, db, 903, "phoenix", ts, "native", usdc, "10", "2", "2")
	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh pools_per_source_1h: %v", err)
	}

	pools, _, err := store.AllPools(ctx, timescale.PoolsFilter{}, "", 100, timescale.MarketsOrderPair)
	if err != nil {
		t.Fatalf("AllPools: %v", err)
	}
	seen := 0
	for _, p := range pools {
		if p.Source != "aquarius" && p.Source != "phoenix" {
			continue
		}
		seen++
		if p.LastPrice == nil || numeric(t, p.LastPrice) != 0.2 {
			t.Errorf("%s last_price = %v, want 0.2 (the higher tied close)", p.Source, p.LastPrice)
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 folded pools in %d rows", seen, len(pools))
	}
}
