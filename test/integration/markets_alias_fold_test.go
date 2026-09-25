//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDistinctPairsAndPools_FoldAliasSpellings pins GH-1098: /v1/markets
// and /v1/pools grouped their canon CTE by orientation but not by alias
// spelling, so a market traded on one venue under `crypto:XLM` and on
// another under `native` (or its SAC wrapper) surfaced as two directory
// rows with split 24h volume instead of one.
//
// Seeds the SAME XLM/USDC pair under two different alias spellings —
// `crypto:XLM` (the CEX-recorded form) and the XLM SAC C-address (the
// Soroban-recorded form) — on two different sources, then asserts
// DistinctPairsExt (cross-source /v1/markets) and AllPools (per-source
// /v1/pools) each return exactly ONE row for the pair, with volume and
// trade count SUMMED across both spellings rather than split.
func TestDistinctPairsAndPools_FoldAliasSpellings(t *testing.T) {
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
		usdc      = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		xlmSAC    = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
		cryptoXLM = "crypto:XLM"
	)
	now := time.Now().UTC()
	// A CEX venue keys XLM under crypto:XLM: 2 trades, $10 each.
	for i := range 2 {
		seedGrainTrade(t, ctx, db, 600+i, "cex-a", now.Add(-20*time.Minute), cryptoXLM, usdc, "10", "1", "10")
	}
	// A Soroban venue keys the SAME asset under its SAC: 1 trade, $7.
	seedGrainTrade(t, ctx, db, 610, "soroswap", now.Add(-10*time.Minute), xlmSAC, usdc, "10", "1", "7")

	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh %s: %v", stmt, err)
		}
	}

	t.Run("DistinctPairsExt collapses both alias spellings into one row", func(t *testing.T) {
		markets, _, err := store.DistinctPairsExt(ctx, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("DistinctPairsExt: %v", err)
		}
		matches := 0
		var count int64
		for _, m := range markets {
			b, q := m.Pair.Base.String(), m.Pair.Quote.String()
			isXLMLeg := b == "native" || b == cryptoXLM || b == xlmSAC ||
				q == "native" || q == cryptoXLM || q == xlmSAC
			isUSDCLeg := b == usdc || q == usdc
			if isXLMLeg && isUSDCLeg {
				matches++
				count = m.TradeCount24h
			}
		}
		if matches != 1 {
			t.Fatalf("XLM/USDC surfaced as %d directory rows across alias spellings, want 1 "+
				"(GH-1098: alias spellings must fold before orienting); rows: %+v", matches, markets)
		}
		if count != 3 {
			t.Errorf("folded row's count_24h = %d, want 3 (2 crypto:XLM trades + 1 SAC trade summed)", count)
		}
	})

	t.Run("AllPools collapses both alias spellings into one row per source", func(t *testing.T) {
		pools, _, err := store.AllPools(ctx, timescale.PoolsFilter{Asset: "native"}, "", 100, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatalf("AllPools: %v", err)
		}
		bySource := map[string]int{}
		for _, p := range pools {
			bySource[p.Source]++
		}
		for _, src := range []string{"cex-a", "soroswap"} {
			if bySource[src] != 1 {
				t.Errorf("source %s returned %d pool rows for the pair, want 1", src, bySource[src])
			}
		}
	})
}
