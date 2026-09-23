//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMarketsListingFoldsAliasSpellings executes GH-1098 against real
// TimescaleDB: XLM/USDC traded on SDEX (native / USDC-GA5Z…) and on a
// Soroban venue (the USDC SAC / the XLM SAC, stored in the reversed
// orientation) is ONE market. The directory must list it once with the
// summed 24h volume and trade count, oriented XLM/USDC with the Soroban
// price inverted; /v1/pools must label the Soroban pool with the same
// canonical legs; the row's sparkline and first-trade enrichment must
// read every spelling; the network strip must count it once.
func TestMarketsListingFoldsAliasSpellings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		usdc       = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		usdcSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		xlmSAC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
		usdcConfig = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	reg, err := canonical.NewAliasRegistry(map[string]string{usdcSAC: usdcConfig})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	now := time.Now().UTC()
	// SDEX: 3 × $100 at 0.5 USDC per XLM, half an hour ago.
	for i := range 3 {
		seedGrainTrade(t, ctx, db, 100+i, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "100")
	}
	// Soroban: 2 × $5 stored as USDC-SAC/XLM-SAC at 4 XLM per USDC, the
	// newest print — so the canonical last_price is its inverse, 0.25.
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 200+i, "soroswap", now.Add(-5*time.Minute), usdcSAC, xlmSAC, "1", "4", "5")
	}
	// A second Soroban venue in the SDEX orientation: 1 × $7, older than
	// the soroswap prints so it does not set last_price.
	seedGrainTrade(t, ctx, db, 300, "aquarius", now.Add(-10*time.Minute), xlmSAC, usdcSAC, "1", "0.5", "7")
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg (%s): %v", stmt, err)
		}
	}

	t.Run("directory lists the market once with summed figures", func(t *testing.T) {
		for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderVolume24hDesc, timescale.MarketsOrderPair} {
			markets, _, err := store.DistinctPairsExt(ctx, "", 500, order)
			if err != nil {
				t.Fatalf("DistinctPairsExt(%d): %v", order, err)
			}
			if len(markets) != 1 {
				got := make([]string, 0, len(markets))
				for _, m := range markets {
					got = append(got, m.Pair.Base.String()+"|"+m.Pair.Quote.String())
				}
				t.Fatalf("order %d: %d directory rows %v, want 1 (native|%s)", order, len(markets), got, usdc)
			}
			m := markets[0]
			if m.Pair.Base.String() != "native" || m.Pair.Quote.String() != usdc {
				t.Errorf("order %d: pair = %s|%s, want native|%s", order, m.Pair.Base, m.Pair.Quote, usdc)
			}
			if m.TradeCount24h != 6 {
				t.Errorf("order %d: trade_count_24h = %d, want 6 (3 sdex + 2 soroswap + 1 aquarius)", order, m.TradeCount24h)
			}
			if got := numeric(t, m.Volume24hUSD); got != 317 {
				t.Errorf("order %d: volume_24h_usd = %v, want 317 (300 sdex + 10 soroswap + 7 aquarius)", order, got)
			}
			if got := numeric(t, m.LastPrice); got != 0.25 {
				t.Errorf("order %d: last_price = %v, want 0.25 (the newest print, 4 XLM/USDC, inverted)", order, got)
			}
		}
	})

	t.Run("asset filter on the SAC spelling returns the folded row", func(t *testing.T) {
		markets, _, err := store.AssetMarkets(ctx, xlmSAC, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AssetMarkets: %v", err)
		}
		if len(markets) != 1 || numeric(t, markets[0].Volume24hUSD) != 317 {
			t.Fatalf("AssetMarkets(%s) = %+v, want one native|USDC row carrying 317", xlmSAC, markets)
		}
	})

	t.Run("soroban pool carries the canonical legs", func(t *testing.T) {
		pools, _, err := store.AllPools(ctx, timescale.PoolsFilter{Sources: []string{"soroswap"}}, "", 100, timescale.MarketsOrderPair)
		if err != nil {
			t.Fatalf("AllPools: %v", err)
		}
		if len(pools) != 1 {
			t.Fatalf("AllPools(soroswap) = %d rows, want 1", len(pools))
		}
		p := pools[0]
		if p.Pair.Base.String() != "native" || p.Pair.Quote.String() != usdc {
			t.Errorf("soroswap pool = %s|%s, want native|%s (same key as the sdex pool)", p.Pair.Base, p.Pair.Quote, usdc)
		}
		if got := numeric(t, p.LastPrice); got != 0.25 {
			t.Errorf("soroswap pool last_price = %v, want 0.25", got)
		}
		markets, _, err := store.SourceMarkets(ctx, "soroswap", "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("SourceMarkets: %v", err)
		}
		if len(markets) != 1 || markets[0].Pair.Base.String() != "native" || numeric(t, markets[0].Volume24hUSD) != 10 {
			t.Errorf("SourceMarkets(soroswap) = %+v, want one native|USDC row carrying 10", markets)
		}
	})

	t.Run("row enrichment reads every spelling of the folded row", func(t *testing.T) {
		pairs := [][2]string{{"native", usdc}}
		hist, err := store.GetPairsVolumeHistory24hBatch(ctx, pairs)
		if err != nil {
			t.Fatalf("GetPairsVolumeHistory24hBatch: %v", err)
		}
		var sum float64
		for _, pt := range hist["native|"+usdc] {
			v := pt.VolumeUSD
			sum += numeric(t, &v)
		}
		// SDEX 300 + the SAC-spelled aquarius 7. The reversed soroswap
		// prints are an orientation gap the sparkline does not fold (a
		// separate defect), so they are not part of this figure.
		if sum != 307 {
			t.Errorf("sparkline sum = %v, want 307 (300 sdex + 7 aquarius under the SAC spelling)", sum)
		}
		firsts, err := store.FirstTradeBatch(ctx, pairs)
		if err != nil {
			t.Fatalf("FirstTradeBatch: %v", err)
		}
		if _, ok := firsts["native|"+usdc]; !ok {
			t.Errorf("FirstTradeBatch has no entry for the folded row: %v", firsts)
		}
	})

	t.Run("network strip counts the market once", func(t *testing.T) {
		st, err := store.GetNetworkStats(ctx)
		if err != nil {
			t.Fatalf("GetNetworkStats: %v", err)
		}
		if st.MarketsCount24h != 1 {
			t.Errorf("markets_count_24h = %d, want 1", st.MarketsCount24h)
		}
	})
}
