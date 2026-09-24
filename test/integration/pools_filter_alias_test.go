//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPoolsFilterFoldsOrientationAndAliases executes the /v1/pools
// filters against the real pools_per_source_1h CAGG. SDEX stores XLM/USDC
// and USDC/XLM trades as separate rows, and Soroban DEXes key XLM under
// its SAC, so a filter applied before the orientation fold, or bound as
// one literal spelling, drops real volume from the pair page and the
// asset Liquidity tab while still presenting the figures as totals.
func TestPoolsFilterFoldsOrientationAndAliases(t *testing.T) {
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
	// sdex: 3 trades stored XLM-first ($10 each), 2 stored USDC-first ($20 each).
	for i := range 3 {
		seedGrainTrade(t, ctx, db, 500+i, "sdex", now.Add(-30*time.Minute), "native", usdc, "10", "1", "10")
	}
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 510+i, "sdex", now.Add(-20*time.Minute), usdc, "native", "2", "20", "20")
	}
	// soroswap keys XLM under its SAC.
	seedGrainTrade(t, ctx, db, 520, "soroswap", now.Add(-10*time.Minute), xlmSAC, usdc, "10", "1", "7")
	// A pool touching neither asset, which every filter must exclude.
	seedGrainTrade(t, ctx, db, 530, "sdex", now.Add(-10*time.Minute), "crypto:ETH", "fiat:USD", "1", "20", "20")

	if _, err := db.ExecContext(ctx, `CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh pools_per_source_1h: %v", err)
	}

	type want struct {
		count int64
		vol   float64
	}
	both := map[string]want{"sdex": {5, 70}, "soroswap": {1, 7}}
	sources := []string{"sdex", "soroswap"}
	for name, tc := range map[string]struct {
		filter timescale.PoolsFilter
		want   map[string]want
	}{
		"pair in canonical order":   {timescale.PoolsFilter{Sources: sources, Base: "native", Quote: usdc}, both},
		"pair in reversed order":    {timescale.PoolsFilter{Sources: sources, Base: usdc, Quote: "native"}, both},
		"asset=native":              {timescale.PoolsFilter{Sources: sources, Asset: "native"}, both},
		"asset=SAC":                 {timescale.PoolsFilter{Sources: sources, Asset: xlmSAC}, both},
		"base-only canonical side":  {timescale.PoolsFilter{Sources: sources, Base: "native"}, both},
		"quote-only canonical side": {timescale.PoolsFilter{Sources: sources, Quote: usdc}, both},
		"base-only non-canonical":   {timescale.PoolsFilter{Sources: sources, Base: usdc}, map[string]want{}},
	} {
		for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderVolume24hDesc, timescale.MarketsOrderPair} {
			pools, _, err := store.AllPools(ctx, tc.filter, "", 100, order)
			if err != nil {
				t.Fatalf("%s (order %v): AllPools: %v", name, order, err)
			}
			got := make(map[string]want, len(pools))
			for _, p := range pools {
				if p.Pair.Base.String() == "crypto:ETH" {
					t.Errorf("%s: unrelated ETH/USD pool leaked through the filter", name)
				}
				if _, dup := got[p.Source]; dup {
					t.Errorf("%s: %s returned twice — orientations were not folded", name, p.Source)
				}
				got[p.Source] = want{p.TradeCount24h, numeric(t, p.Volume24hUSD)}
			}
			if len(got) != len(tc.want) {
				t.Errorf("%s (order %v): got venues %v, want %v", name, order, got, tc.want)
			}
			for src, w := range tc.want {
				if got[src] != w {
					t.Errorf("%s (order %v): %s = %+v, want %+v (both orientations, every alias form)",
						name, order, src, got[src], w)
				}
			}
		}
	}
}
