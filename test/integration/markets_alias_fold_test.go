//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMarketsAndPoolsDropSelfFoldedPair seeds native ↔ XLM-SAC swaps in
// both leg orders. Both legs fold to `native`, a pair canonical.NewPair
// refuses: the listings must drop it in SQL rather than fail the whole
// /v1/pools request or skip-and-count it on every /v1/markets read.
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

// TestMarketsAndPoolsFoldSACSpelling executes the /v1/markets and
// /v1/pools reads against real TimescaleDB. SDEX keys XLM as `native`
// and Soroban venues key it as its SAC, so a listing that groups on the
// stored spelling serves one market as two rows, each with part of the
// 24h volume, and ranks it below markets it outsizes.
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
