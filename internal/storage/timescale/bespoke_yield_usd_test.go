package timescale

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
)

const (
	testUSDCStrategy  = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"
	testCETESStrategy = "CAZ3LLLKPWEOVK6K4G5NCQ2VXWABLFIPKKNMN5GLKMZKEN7JSKTEMIKN"
	testCETES         = "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	testCETESSAC      = "CAL6ER2TI6CTRAY6BFXWNWA7WTYXUXTQCHUBCIBU5O6KM3HJFG6Z6VXV"
)

// The fold is the union market's Σ vwap·vol / Σ vol across the classic and SAC spellings, and a day prices_1d holds ignores prices_1h.
func TestFoldDailyUSDPrices_WeightsSpellingsAndPrefersDaily(t *testing.T) {
	spellings := defindexPriceSpellings(defindexStrategyAssets)
	got := foldDailyUSDPrices([]usdPriceRow{
		{daily: true, day: "2026-11-10", asset: testCETES, vwap: rat("0.05"), volPriced: rat("3000")},
		{daily: true, day: "2026-11-10", asset: testCETESSAC, vwap: rat("0.06"), volPriced: rat("1000")},
		{daily: false, day: "2026-11-10", asset: testCETES, vwap: rat("9"), volPriced: rat("1")},
		{daily: false, day: "2026-11-11", asset: testCETESSAC, vwap: rat("0.07"), volPriced: rat("5")},
		{daily: true, day: "2026-11-10", asset: "SCAM-GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF", vwap: rat("100"), volPriced: rat("1")},
	}, spellings)
	if p := got[assetDay{testCETES, "2026-11-10"}]; p == nil || p.Cmp(rat("0.0525")) != 0 {
		t.Errorf("2026-11-10 CETES = %v, want 0.0525 (daily rows only, volume-weighted across spellings)", p)
	}
	if p := got[assetDay{testCETES, "2026-11-11"}]; p == nil || p.Cmp(rat("0.07")) != 0 {
		t.Errorf("2026-11-11 CETES = %v, want 0.07 from prices_1h while prices_1d lacks the day", p)
	}
	if len(got) != 2 {
		t.Errorf("got %d prices, want 2: a spelling outside the strategy assets must not be read", len(got))
	}
}

// A flipped (USDC, CETES) row's vwap is CETES per USDC: it adds vol USD for vwap·vol CETES, folded with the forward rows.
func TestFoldDailyUSDPrices_FoldsFlippedRows(t *testing.T) {
	got := foldDailyUSDPrices([]usdPriceRow{
		{daily: true, flipped: true, day: "2026-11-12", asset: testCETES, vwap: rat("20"), volPriced: rat("100")},
		{daily: true, day: "2026-11-12", asset: testCETESSAC, vwap: rat("0.04"), volPriced: rat("500")},
		{daily: true, flipped: true, day: "2026-11-13", asset: testCETESSAC, vwap: rat("25"), volPriced: rat("4")},
	}, defindexPriceSpellings(defindexStrategyAssets))
	if p := got[assetDay{testCETES, "2026-11-12"}]; p == nil || p.Cmp(rat("0.048")) != 0 {
		t.Errorf("2026-11-12 CETES = %v, want 0.048 = (100 + 20) USD / (2000 + 500) CETES", p)
	}
	if p := got[assetDay{testCETES, "2026-11-13"}]; p == nil || p.Cmp(rat("0.04")) != 0 {
		t.Errorf("2026-11-13 CETES = %v, want 0.04 = 1 / 25 from the flipped row alone", p)
	}
}

// The flipped arms read the on-chain USDC proxies only: fiat:USD volume is not on the asset's 7-decimal scale.
func TestDefindexDailyUSDPricesSQL_BothDirectionsOnChainFlips(t *testing.T) {
	if n := strings.Count(defindexDailyUSDPricesSQL, "UNION ALL"); n != 3 {
		t.Fatalf("got %d UNION ALL, want 4 arms (prices_1d, prices_1h) × (forward, flipped)", n)
	}
	if n := strings.Count(defindexDailyUSDPricesSQL, "AND base_asset NOT LIKE 'fiat:%'"); n != 2 {
		t.Errorf("got %d fiat exclusions, want one per flipped arm", n)
	}
	if n := strings.Count(defindexDailyUSDPricesSQL, "AND quote_asset = ANY($2::text[])"); n != 2 {
		t.Errorf("got %d flipped arms, want 2", n)
	}
}

// Invariant: a flow is valued at its own day's price of its own strategy's asset, exactly, or excluded and named; never guessed.
func TestValueStrategyFlows_PricesExactlyAndNamesExclusions(t *testing.T) {
	huge := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 70)) // above 2^63: must not narrow
	flows := []strategyFlowSum{
		{strategy: testUSDCStrategy, direction: "deposit", bucket: "2026-11-10", day: "2026-11-10", amount: huge, flows: 3},
		{strategy: testCETESStrategy, direction: "deposit", bucket: "2026-11-10", day: "2026-11-10", amount: rat("20000000"), flows: 1},
		{strategy: testCETESStrategy, direction: "deposit", bucket: "2026-11-11", day: "2026-11-11", amount: rat("50000000"), flows: 2},
		{strategy: "CUNMAPPEDXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", direction: "withdraw", bucket: "2026-11-10", day: "2026-11-10", amount: rat("1"), flows: 4},
	}
	prices := map[assetDay]*big.Rat{{testCETES, "2026-11-10"}: rat("0.05")}
	v := valueStrategyFlows(flows, defindexStrategyAssets, prices)

	wantUSDC := new(big.Rat).Mul(huge, tenToMinus(7))
	want := new(big.Rat).Add(wantUSDC, rat("0.1")) // 2 CETES × $0.05
	if got := v.total("deposit"); got.Cmp(want) != 0 {
		t.Errorf("deposit total = %s, want %s", got.FloatString(7), want.FloatString(7))
	}
	if _, ok := v.byDirBucket["deposit"]["2026-11-11"]; ok {
		t.Error("an unpriced day must leave no valued point, not a zero")
	}
	if c := v.lowerBoundClause("deposit"); c != "excludes 2 strategy flows it could not value in USD: CETES (no USD price that day)" {
		t.Errorf("deposit clause = %q", c)
	}
	if c := v.lowerBoundClause("withdraw"); !strings.Contains(c, "4 strategy flows it could not value in USD: strategy CUNMAPPED") {
		t.Errorf("withdraw clause = %q", c)
	}
	if v.total("withdraw").Sign() != 0 {
		t.Error("an unmapped strategy's flows must not be valued")
	}
}

func TestYieldVolumeKPIHint_NamesLowerBound(t *testing.T) {
	full := yieldUSD{unpriced: map[string]*unpricedTally{}}
	if h := yieldVolumeKPIHint("deposit", "deposits", full); strings.Contains(h, "LOWER BOUND") {
		t.Errorf("fully priced hint claims a lower bound: %q", h)
	}
	partial := yieldUSD{unpriced: map[string]*unpricedTally{"deposit": {flows: 1, assets: map[string]bool{"EURC": true}}}}
	if h := yieldVolumeKPIHint("deposit", "deposits", partial); !strings.HasPrefix(h, "LOWER BOUND:") || !strings.Contains(h, "EURC") {
		t.Errorf("partial hint = %q, want a LOWER BOUND naming EURC", h)
	}
	if h := yieldVolumeKPIHint("withdraw", "withdrawals", partial); strings.Contains(h, "LOWER BOUND") {
		t.Errorf("withdraw hint inherits the deposit exclusion: %q", h)
	}
	if c := full.seriesLowerBoundClause(); c != "" {
		t.Errorf("fully priced series clause = %q, want none", c)
	}
	if c := partial.seriesLowerBoundClause(); !strings.HasPrefix(c, "deposit excludes 1 strategy flows") || strings.Contains(c, "withdraw") {
		t.Errorf("partial series clause = %q, want the deposit exclusion only", c)
	}
}

// An unmapped strategy's flows drop out of the USD volume as unpriced, so the map must track the curated set exactly.
func TestDefindexStrategyAssets_CoverCuratedSet(t *testing.T) {
	if len(defindexStrategyAssets) != len(defindex.MainnetStrategies) {
		t.Fatalf("defindexStrategyAssets has %d entries, MainnetStrategies %d", len(defindexStrategyAssets), len(defindex.MainnetStrategies))
	}
	for _, s := range defindex.MainnetStrategies {
		a, ok := defindexStrategyAssets[s]
		if !ok {
			t.Errorf("strategy %s has no asset", s)
			continue
		}
		c, err := canonical.ParseAsset(a.Classic)
		if err != nil || (c.Type != canonical.AssetClassic && c.Type != canonical.AssetNative) {
			t.Errorf("strategy %s classic %q is not a classic or native asset (err %v)", s, a.Classic, err)
		}
		sac, err := canonical.ParseAsset(a.SAC)
		if err != nil || sac.Type != canonical.AssetSoroban {
			t.Errorf("strategy %s SAC %q is not a contract (err %v)", s, a.SAC, err)
		}
	}
}

// A SAC paired with the wrong classic id prices a strategy as some other asset.
func TestDefindexStrategyAssets_SACDerivesFromClassic(t *testing.T) {
	for s, a := range defindexStrategyAssets {
		c, err := canonical.ParseAsset(a.Classic)
		if err != nil {
			t.Fatalf("strategy %s: %v", s, err)
		}
		got, err := c.SacContractID()
		if err != nil {
			t.Fatalf("strategy %s: %v", s, err)
		}
		if got != a.SAC {
			t.Errorf("strategy %s: %s derives SAC %s, map says %s", s, a.Classic, got, a.SAC)
		}
	}
}
