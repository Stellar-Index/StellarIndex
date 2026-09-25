// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tvlTestSelfListed is a well-formed Soroban token that no catalogue
// entry, SAC derivation or declared peg vouches for — the shape of a
// token anyone can deploy and pair on the Soroswap factory.
const tvlTestSelfListed = tvlTestPhxBadPool // a well-formed contract id reused as an opaque token

// tvlTestAquaSAC derives the AQUA SAC on the configured network from
// the catalogue's own classic entry, so the test cannot drift from the
// identity the valuer derives.
func tvlTestAquaSAC(t *testing.T) (sac string, classic canonical.Asset) {
	t.Helper()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("aqua asset: %v", err)
	}
	sac, err = aqua.SacContractID()
	if err != nil {
		t.Fatalf("aqua sac: %v", err)
	}
	return sac, aqua
}

// TestDEXTVLCache_SelfListedTokenIsNotValued is the #985 regression.
//
// The pool registry is fed by the factory, and the factory lets anyone
// pair any token, so a self-listed token's reserve magnitude AND its
// prices_1m VWAP are both authored by its creator. Before the identity
// screen, a served rate for such a token (one wash swap clears the
// resolver's one-cent floor, a few dollars of self-trading clears the
// substance floor) valued its whole reserve into tvl_usd with the pool
// counted PRICED — no lower-bound marker at all.
//
// A verified-catalogue asset reached through its SAC must still be
// valued even when the operator never declared that SAC as a wrapper,
// or the screen would silently drop legitimate liquidity.
func TestDEXTVLCache_SelfListedTokenIsNotValued(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	aquaSAC, _ := tvlTestAquaSAC(t)
	src := DEXTVLSources{
		SoroswapPairs: stubTVLPairsReader{pairs: []timescale.SoroswapPair{
			{PairStrkey: tvlTestPairA}, {PairStrkey: tvlTestPairB},
		}},
		SoroswapReserves: stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{
			// 20 XLM ($10 at 0.5) against 1e15 raw of the self-listed
			// token at a served rate of 1000 — $100 billion if valued.
			tvlTestPairA: {
				Pair:   tvlTestPairA,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(200_000_000),
				Token1: tvlTestSelfListed, Reserve1: big.NewInt(1_000_000_000_000_000),
				Ledger: tvlTestLedgerPairA,
			},
			// 10 XLM ($5) against 30 AQUA via its undeclared SAC ($3 at 0.1).
			tvlTestPairB: {
				Pair:   tvlTestPairB,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(100_000_000),
				Token1: aquaSAC, Reserve1: big.NewInt(300_000_000),
				Ledger: tvlTestLedgerPairB,
			},
		}},
		Pricer: stubTVLPricer{rates: map[string]string{
			"native":          "0.5",
			tvlTestSelfListed: "1000",
			aquaSAC:           "0.1",
		}},
		Verified: cat,
	}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, ok := c.Protocol("soroswap")
	if !ok {
		t.Fatal("soroswap missing from snapshot")
	}
	if snap.TVL.TVLUSD != "18.00" {
		t.Errorf("soroswap tvl_usd = %q, want 18.00 ($10 + $5 XLM + $3 AQUA; the self-listed leg contributes 0)", snap.TVL.TVLUSD)
	}
	if snap.TVL.PoolsTotal != 2 || snap.TVL.PoolsPriced != 1 || snap.TVL.UnpricedPools != 1 {
		t.Errorf("soroswap pools = %d/%d/%d, want 2 total, 1 priced, 1 unpriced",
			snap.TVL.PoolsTotal, snap.TVL.PoolsPriced, snap.TVL.UnpricedPools)
	}
	var selfListed *DEXTVLLegView
	for i := range snap.Pools {
		for j := range snap.Pools[i].Legs {
			if snap.Pools[i].Legs[j].Token == tvlTestSelfListed {
				selfListed = &snap.Pools[i].Legs[j]
			}
		}
	}
	if selfListed == nil {
		t.Fatal("self-listed leg missing from the drill-down")
	}
	if selfListed.Excluded != DEXTVLLegUnverifiedAsset || selfListed.USD != "" {
		t.Errorf("self-listed leg = excluded %q usd %q, want excluded %q and no usd",
			selfListed.Excluded, selfListed.USD, DEXTVLLegUnverifiedAsset)
	}
	if !strings.Contains(snap.TVL.Basis, "verified currency catalogue") {
		t.Errorf("basis = %q, want it to state the identity screen", snap.TVL.Basis)
	}
	total := c.Total()
	if total == nil || !total.LowerBound {
		t.Errorf("tvl_total lower_bound missing or false (total nil: %v), want true while a self-listed leg is unvalued", total == nil)
	}
}

// tvlTestCatalogue builds a verified-currency catalogue vouching for the
// given Soroban token contract ids, so a fixture that exercises a price
// tier with an arbitrary token first clears the identity screen.
func tvlTestCatalogue(contracts ...string) *currency.Catalogue {
	var b strings.Builder
	b.WriteString("verified_currencies:\n")
	for i, c := range contracts {
		fmt.Fprintf(&b, "  - ticker: TST%d\n    slug: tst%d\n    name: Test token %d\n    class: crypto\n"+
			"    networks:\n      - network: stellar\n        asset_id: %s\n", i, i, i, c)
	}
	cat, err := currency.LoadFromBytes([]byte(b.String()))
	if err != nil {
		panic(fmt.Sprintf("tvlTestCatalogue: %v", err))
	}
	return cat
}

// TestDEXTVLCache_IdentityScreenFailsClosedWithoutCatalogue pins the nil
// direction: with no catalogue wired only native XLM and a declared USD
// peg are identified, so a priced self-listed token still contributes
// nothing. A missing wire must shrink the figure, never inflate it.
func TestDEXTVLCache_IdentityScreenFailsClosedWithoutCatalogue(t *testing.T) {
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{
		"native": "0.5", tvlTestSelfListed: "1000",
	}}, stubTVLPegInfo{pegged: map[string]int{tvlTestUSDCSAC: 7}}, nil, time.Now())
	if got := v.value(context.Background(), tvlTestSelfListed, big.NewInt(10_000_000)); got.excluded != DEXTVLLegUnverifiedAsset {
		t.Errorf("self-listed leg excluded = %q, want %q", got.excluded, DEXTVLLegUnverifiedAsset)
	}
	if got := v.value(context.Background(), canonical.XLMSacContractID, big.NewInt(10_000_000)); got.usd == nil || got.usd.FloatString(2) != "0.50" {
		t.Errorf("native leg = %+v, want $0.50", got)
	}
	if got := v.value(context.Background(), tvlTestUSDCSAC, big.NewInt(10_000_000)); got.basis != DEXTVLBasisDeclaredUSDPeg {
		t.Errorf("declared peg leg = %+v, want valued at the declared peg", got)
	}
}
