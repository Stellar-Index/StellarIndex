//go:build integration

package integration_test

import (
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestXLMLegVolume_TradeTimeNeverSpot pins the volume readers that used to
// value an unpriced XLM leg at today's XLM/USD: the per-source breakdowns
// exclude it and count it, and the MEV scan values it at the anchor of the
// trade's own minute, or not at all when that anchor is over an hour old.
// XLM trades at 0.2 (3h ago), 0.4 (30m ago) and 0.5 (5m ago); spot is 0.5.
func TestXLMLegVolume_TradeTimeNeverSpot(t *testing.T) {
	f := newAnchorFixture(t)
	const unit = 10_000_000
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	native := c.NativeAsset()
	f.trade("sdex", 3*time.Hour, native, usdc, 10*unit, 2*unit, "2")
	f.trade("sdex", 30*time.Minute, native, usdc, 10*unit, 4*unit, "4")
	f.trade("sdex", 5*time.Minute, native, usdc, 10*unit, 5*unit, "5")
	f.trade("sdex", 10*time.Minute, native, usdc, 10*unit, 5*unit, "")
	X := f.contract("X")
	f.trade("sdex", 29*time.Minute, native, X, 10*unit, 1*unit, "")
	fresh := uint32(50_000_000 + f.nonce)
	f.trade("sdex", 110*time.Minute, X, native, 1*unit, 10*unit, "")
	stale := uint32(50_000_000 + f.nonce)

	db := f.store.DB()
	for ledger, usd := range f.usd {
		if _, err := db.ExecContext(f.ctx, `UPDATE trades SET usd_volume = $1::numeric WHERE ledger = $2`, usd, ledger); err != nil {
			t.Fatalf("stamp usd_volume: %v", err)
		}
	}
	for _, q := range []string{
		`UPDATE trades SET taker = 'GTAKER'`,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('pools_per_source_1h', NULL, NULL)`,
	} {
		if _, err := db.ExecContext(f.ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	xlm := c.AssetAliasStrings(native)
	pair, err := f.store.PairSourceStats(f.ctx, xlm, []string{usdc.String()})
	if err != nil {
		t.Fatalf("PairSourceStats: %v", err)
	}
	if len(pair) != 1 || !ratEq(t, pair[0].VolumeUSD24h.String, "11") || pair[0].UnpricedTrades24h != 1 {
		t.Errorf("PairSourceStats = %+v, want sdex volume 11 (trade-time only) with 1 unpriced trade", pair)
	}
	asset, err := f.store.AssetSourceStats(f.ctx, xlm)
	if err != nil {
		t.Fatalf("AssetSourceStats: %v", err)
	}
	if len(asset) != 1 || !ratEq(t, asset[0].VolumeUSD24h.String, "11") || asset[0].UnpricedTrades24h != 3 {
		t.Errorf("AssetSourceStats = %+v, want sdex volume 11 (trade-time only) with 3 unpriced trades", asset)
	}

	// Spot would read 10 (20 XLM at 0.5); trade time is 10 XLM at 0.4, and the
	// stale leg is excluded.
	soroban, lowerBound, err := f.store.SorobanVolume24hUSDForAsset(f.ctx, X.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if !ratEq(t, soroban, "4") || !lowerBound {
		t.Errorf("SorobanVolume24hUSDForAsset(X) = %s lowerBound=%v, want 4 at trade time, flagged a lower bound", soroban, lowerBound)
	}

	pools, _, err := f.store.AllPools(f.ctx, timescale.PoolsFilter{Sources: []string{"sdex"}}, "", 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("AllPools: %v", err)
	}
	markets, _, err := f.store.SourceMarkets(f.ctx, "sdex", "", 100, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("SourceMarkets: %v", err)
	}
	for _, p := range pools {
		if p.Pair.Base.String() == "native" && p.Pair.Quote == usdc &&
			(p.Volume24hUSD == nil || !ratEq(t, *p.Volume24hUSD, "11") || !p.VolumeLowerBound) {
			t.Errorf("AllPools sdex XLM/USDC = %+v, want 11 (trade-time only, spot would add 5) flagged a lower bound", p)
		}
	}
	for _, m := range markets {
		if m.Pair.Base.String() == "native" && m.Pair.Quote == usdc &&
			(m.Volume24hUSD == nil || !ratEq(t, *m.Volume24hUSD, "11") || !m.VolumeLowerBound) {
			t.Errorf("SourceMarkets sdex XLM/USDC = %+v, want 11 flagged a lower bound", m)
		}
	}
	if len(pools) == 0 || len(markets) == 0 {
		t.Errorf("AllPools/SourceMarkets(sdex) returned %d/%d rows, want both non-empty", len(pools), len(markets))
	}

	trades, usd, err := f.store.TradesForArbScan(f.ctx, f.now.Add(-4*time.Hour), 0)
	if err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	got := map[uint32]string{}
	for i, tr := range trades {
		got[tr.Ledger] = usd[i]
	}
	if v, ok := got[fresh]; !ok || !ratEq(t, v, "4") {
		t.Errorf("fresh XLM leg notional = %q (present=%v), want 4: 10 XLM at its own minute's 0.4, not spot 0.5", v, ok)
	}
	if v, ok := got[stale]; !ok || v != "" {
		t.Errorf("stale XLM leg notional = %q (present=%v), want \"\": its newest anchor is 70 min before the trade", v, ok)
	}
}
