package v1

import (
	"context"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDEXTVLPools_MoneyRoundsOnceAtTheLeaf pins the reconciliation
// contract the per-pool drill-down (#338) rests on: each valued leg is
// published to the cent, and every figure above it is the EXACT sum of
// the published figures beneath. Two legs worth $0.004 each are
// therefore two published "0.00" legs, a "0.00" pool and a "0.00"
// protocol — not a "0.01" protocol that no set of published pool rows
// could ever add up to.
//
// Written against the public cache surface only, so it runs unchanged
// against the tree before the drill-down existed, where the protocol
// figure was the unrounded rational sum rendered once and read "0.01".
func TestDEXTVLPools_MoneyRoundsOnceAtTheLeaf(t *testing.T) {
	// 80,000 raw XLM-SAC units at the 1e7 anchor scale × $0.5 = $0.004.
	const subCent = 80_000
	c := NewDEXTVLCache(DEXTVLSources{
		SoroswapPairs: stubTVLPairsReader{pairs: []timescale.SoroswapPair{{PairStrkey: tvlTestPairA}}},
		SoroswapReserves: stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{
			tvlTestPairA: {
				Pair:   tvlTestPairA,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(subCent),
				Token1: canonical.XLMSacContractID, Reserve1: big.NewInt(subCent),
				Ledger: tvlTestLedgerPairA,
			},
		}},
		Pricer: stubTVLPricer{rates: map[string]string{"native": "0.5"}},
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	if got := snap["soroswap"].TVLUSD; got != "0.00" {
		t.Fatalf("soroswap tvl_usd = %q, want 0.00 — two sub-cent legs publish as two 0.00 legs, and the "+
			"protocol figure is the exact sum of what is published, not the rational 0.008 rendered once", got)
	}
	if snap["soroswap"].PoolsPriced != 1 || snap["soroswap"].UnpricedPools != 0 {
		t.Errorf("a pool of valued sub-cent legs is PRICED (worth less than a cent is not unpriceable): %+v", snap["soroswap"])
	}
}
