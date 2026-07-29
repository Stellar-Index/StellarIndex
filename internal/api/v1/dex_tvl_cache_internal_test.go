package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Valid C-strkeys for test tokens (the XLM SAC is the real pubnet one;
// the others are real deployed contract ids reused purely as opaque
// well-formed strkeys).
const (
	tvlTestUSDCSAC  = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	tvlTestPairA    = "CDGHOS7DDZ7DB24J7TGDJCRF3ZJ7FA4K6NBSMVUVRPXV3N2CD3BM4JHZ"
	tvlTestPairB    = "CDP3XWJ4ZN222LKYBMWIY22MMPYWCFHUUDLGVKPXCHTFPXTJATRWSAJK"
	tvlTestUnpriced = "CAAV3AE3VKD2P4TY7LWTQMMJHIJ4WOCZ5ANCIJPC3NRSERQVXYHNCCQW"
	tvlTestAqPool   = "CBQDHNBFBZYE4MECPHNQCLM7F5FRZ4R7HZWQZXAK7NZYYUR3ILWSKDMV"
)

type stubTVLPairsReader struct {
	pairs []timescale.SoroswapPair
	err   error
}

func (s stubTVLPairsReader) LoadSoroswapPairRegistry(context.Context) ([]timescale.SoroswapPair, error) {
	return s.pairs, s.err
}

type stubTVLReserveReader struct {
	states map[string]clickhouse.SoroswapPairState
	err    error
}

func (s stubTVLReserveReader) SoroswapPairReserves(_ context.Context, _ []string) (map[string]clickhouse.SoroswapPairState, error) {
	return s.states, s.err
}

type stubAquariusReserveReader struct {
	pools []timescale.AquariusPoolReserve
	err   error
}

func (s *stubAquariusReserveReader) LatestAquariusReserves(context.Context, int) ([]timescale.AquariusPoolReserve, error) {
	return s.pools, s.err
}

// stubTVLPricer prices `native` and any token listed in rates; every
// other asset is unpriceable.
type stubTVLPricer struct {
	rates map[string]string // canonical asset String() → raw-ratio rate
}

func (s stubTVLPricer) USDPriceAt(_ context.Context, asset canonical.Asset, _ time.Time) (string, bool, error) {
	r, ok := s.rates[asset.String()]
	return r, ok, nil
}

type stubTVLPegInfo struct{ pegged map[string]int }

func (s stubTVLPegInfo) QuoteUSDPegInfo(asset canonical.Asset) (int, bool) {
	d, ok := s.pegged[asset.String()]
	return d, ok
}

func tvlTestSources() DEXTVLSources {
	return DEXTVLSources{
		SoroswapPairs: stubTVLPairsReader{pairs: []timescale.SoroswapPair{
			{PairStrkey: tvlTestPairA}, {PairStrkey: tvlTestPairB},
		}},
		SoroswapReserves: stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{
			// 20 XLM-SAC (raw 2e8 at the anchor's 1e7 scale → ×0.5 = $10)
			// + 10.5 USDC-SAC (pegged at 7 decimals → $10.50).
			tvlTestPairA: {
				Pair:   tvlTestPairA,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(200_000_000),
				Token1: tvlTestUSDCSAC, Reserve1: big.NewInt(105_000_000),
			},
			// One unpriceable leg + 10 XLM-SAC → $5 lower-bound
			// contribution, pool counted unpriced.
			tvlTestPairB: {
				Pair:   tvlTestPairB,
				Token0: tvlTestUnpriced, Reserve0: big.NewInt(999),
				Token1: canonical.XLMSacContractID, Reserve1: big.NewInt(100_000_000),
			},
		}},
		AquariusReserves: &stubAquariusReserveReader{pools: []timescale.AquariusPoolReserve{{
			ContractID: tvlTestAqPool,
			ObservedAt: time.Now(),
			Legs: []timescale.AquariusReserveLeg{
				{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(10_000_000))},
				{TokenIndex: 1, Token: "", Reserve: canonical.NewAmount(big.NewInt(42))},
			},
		}}},
		Pricer:  stubTVLPricer{rates: map[string]string{"native": "0.5"}},
		PegInfo: stubTVLPegInfo{pegged: map[string]int{tvlTestUSDCSAC: 7}},
	}
}

func TestDEXTVLCache_ColdStartServesEmpty(t *testing.T) {
	c := NewDEXTVLCache(DEXTVLSources{})
	snap, at := c.Snapshot()
	if snap != nil || !at.IsZero() {
		t.Fatalf("cold snapshot = %v @ %v, want nil @ zero", snap, at)
	}
}

func TestDEXTVLCache_RefreshComputesProtocolTVL(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, at := c.Snapshot()
	if at.IsZero() {
		t.Fatal("fetchedAt still zero after Refresh")
	}

	ss, ok := snap["soroswap"]
	if !ok {
		t.Fatal("soroswap missing from snapshot")
	}
	// $10 (XLM) + $10.50 (USDC peg) + $5 (pair B priced leg) = $25.50.
	if ss.TVLUSD != "25.50" {
		t.Errorf("soroswap TVLUSD = %q, want 25.50", ss.TVLUSD)
	}
	if ss.PoolsTotal != 2 || ss.PoolsPriced != 1 || ss.UnpricedPools != 1 {
		t.Errorf("soroswap pools = total %d priced %d unpriced %d, want 2/1/1",
			ss.PoolsTotal, ss.PoolsPriced, ss.UnpricedPools)
	}
	if ss.AsOf == "" || ss.Basis == "" {
		t.Error("soroswap AsOf/Basis must be populated")
	}

	aq, ok := snap["aquarius"]
	if !ok {
		t.Fatal("aquarius missing from snapshot")
	}
	// 1 XLM-SAC leg → $0.50; the address-less leg is unpriceable.
	if aq.TVLUSD != "0.50" {
		t.Errorf("aquarius TVLUSD = %q, want 0.50", aq.TVLUSD)
	}
	if aq.PoolsTotal != 1 || aq.PoolsPriced != 0 || aq.UnpricedPools != 1 {
		t.Errorf("aquarius pools = total %d priced %d unpriced %d, want 1/0/1",
			aq.PoolsTotal, aq.PoolsPriced, aq.UnpricedPools)
	}
}

func TestDEXTVLCache_PerProtocolErrorKeepsPreviousEntry(t *testing.T) {
	src := tvlTestSources()
	aq := src.AquariusReserves.(*stubAquariusReserveReader)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	before, _ := c.Snapshot()

	aq.err = errors.New("boom")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("second Refresh should surface the aquarius error")
	}
	after, _ := c.Snapshot()
	if after["aquarius"] != before["aquarius"] {
		t.Errorf("aquarius entry should be carried over on error: %+v vs %+v",
			after["aquarius"], before["aquarius"])
	}
	if _, ok := after["soroswap"]; !ok {
		t.Error("soroswap should still refresh when aquarius errors")
	}
}

func TestDEXTVLCache_MissingReadersOmitProtocols(t *testing.T) {
	c := NewDEXTVLCache(DEXTVLSources{})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with no readers: %v", err)
	}
	snap, at := c.Snapshot()
	if len(snap) != 0 {
		t.Errorf("snapshot = %v, want empty", snap)
	}
	if at.IsZero() {
		t.Error("fetchedAt should be stamped even when nothing is wired")
	}
}

func TestTVLValuer_LegEdgeCases(t *testing.T) {
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{"native": "2"}}, nil, time.Now())
	ctx := context.Background()

	if usd, ok := v.legUSD(ctx, canonical.XLMSacContractID, big.NewInt(0)); !ok || usd.Sign() != 0 {
		t.Errorf("zero reserve should price as exactly 0 (ok=%v usd=%v)", ok, usd)
	}
	if _, ok := v.legUSD(ctx, canonical.XLMSacContractID, big.NewInt(-1)); ok {
		t.Error("negative reserve must be unpriceable")
	}
	if _, ok := v.legUSD(ctx, canonical.XLMSacContractID, nil); ok {
		t.Error("nil reserve must be unpriceable")
	}
	if _, ok := v.legUSD(ctx, "not-a-strkey", big.NewInt(1)); ok {
		t.Error("malformed token must be unpriceable")
	}
	// i128-scale reserve: 2^80 raw units at rate 2 — exact big math,
	// never a float or int64 (ADR-0003).
	huge := new(big.Int).Lsh(big.NewInt(1), 80)
	usd, ok := v.legUSD(ctx, canonical.XLMSacContractID, huge)
	if !ok {
		t.Fatal("huge reserve should price")
	}
	want := new(big.Rat).SetFrac(new(big.Int).Mul(huge, big.NewInt(2)), big.NewInt(10_000_000))
	if usd.Cmp(want) != 0 {
		t.Errorf("huge reserve usd = %s, want %s", usd.FloatString(4), want.FloatString(4))
	}
}
