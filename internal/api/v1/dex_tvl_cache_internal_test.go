package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
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
	// Real curated pool ids reused as opaque well-formed strkeys.
	tvlTestPhxPool    = "CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX"
	tvlTestPhxBadPool = "CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH"
	tvlTestCometPool  = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"
)

// Per-pool last-change ledgers on the shared fixture, unequal and interleaved across protocols, so a snapshot's as_of_ledger is
// right only if it takes the MAX over that protocol's own pools, never a first/last/any-pool ledger.
const (
	tvlTestLedgerPairA    uint32 = 63_000_001
	tvlTestLedgerPairB    uint32 = 63_000_050
	tvlTestLedgerAquarius uint32 = 62_900_000
	tvlTestLedgerPhoenix  uint32 = 63_100_000
	tvlTestLedgerComet    uint32 = 62_000_000
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

type stubPhoenixReserveReader struct {
	states      map[string]clickhouse.PhoenixPoolState
	undecodable []string
	err         error
}

func (s stubPhoenixReserveReader) PhoenixPoolReserves(_ context.Context, _ []string) (map[string]clickhouse.PhoenixPoolState, []string, error) {
	return s.states, s.undecodable, s.err
}

type stubCometReserveReader struct {
	states      map[string]clickhouse.CometPoolState
	undecodable []string
	err         error
}

func (s stubCometReserveReader) CometPoolReserves(_ context.Context, _ []string) (map[string]clickhouse.CometPoolState, []string, error) {
	return s.states, s.undecodable, s.err
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
				Ledger: tvlTestLedgerPairA,
			},
			// One unpriceable leg + 10 XLM-SAC → $5 lower-bound
			// contribution, pool counted unpriced.
			tvlTestPairB: {
				Pair:   tvlTestPairB,
				Token0: tvlTestUnpriced, Reserve0: big.NewInt(999),
				Token1: canonical.XLMSacContractID, Reserve1: big.NewInt(100_000_000),
				Ledger: tvlTestLedgerPairB,
			},
		}},
		AquariusReserves: &stubAquariusReserveReader{pools: []timescale.AquariusPoolReserve{{
			ContractID: tvlTestAqPool,
			ObservedAt: time.Now(),
			Ledger:     tvlTestLedgerAquarius,
			Legs: []timescale.AquariusReserveLeg{
				{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(10_000_000))},
				{TokenIndex: 1, Token: "", Reserve: canonical.NewAmount(big.NewInt(42))},
			},
		}}},
		PhoenixPools: []string{tvlTestPhxPool, tvlTestPhxBadPool},
		PhoenixReserves: stubPhoenixReserveReader{
			states: map[string]clickhouse.PhoenixPoolState{
				// 20 XLM-SAC ($10 at rate 0.5) + 10.5 USDC-SAC peg
				// ($10.50) → $20.50, fully priced.
				tvlTestPhxPool: {
					Pool:   tvlTestPhxPool,
					TokenA: canonical.XLMSacContractID, ReserveA: big.NewInt(200_000_000),
					TokenB: tvlTestUSDCSAC, ReserveB: big.NewInt(105_000_000),
					Ledger: tvlTestLedgerPhoenix,
				},
			},
			// A pool whose captured storage shape the reader refused
			// to decode: contributes 0, counted total + unpriced.
			undecodable: []string{tvlTestPhxBadPool},
		},
		CometPools: []string{tvlTestCometPool},
		CometReserves: stubCometReserveReader{
			states: map[string]clickhouse.CometPoolState{
				// 10 XLM-SAC ($5) + 2.1 USDC-SAC peg ($2.10) → $7.10.
				tvlTestCometPool: {
					Pool: tvlTestCometPool,
					Legs: []clickhouse.CometPoolLeg{
						{Token: canonical.XLMSacContractID, Balance: big.NewInt(100_000_000)},
						{Token: tvlTestUSDCSAC, Balance: big.NewInt(21_000_000)},
					},
					Ledger: tvlTestLedgerComet,
				},
			},
		},
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

	phx, ok := snap["phoenix"]
	if !ok {
		t.Fatal("phoenix missing from snapshot")
	}
	// $10 (XLM) + $10.50 (USDC peg); the undecodable pool contributes
	// exactly 0 and is counted total + unpriced (lower bound, never a
	// guess).
	if phx.TVLUSD != "20.50" {
		t.Errorf("phoenix TVLUSD = %q, want 20.50", phx.TVLUSD)
	}
	if phx.PoolsTotal != 2 || phx.PoolsPriced != 1 || phx.UnpricedPools != 1 {
		t.Errorf("phoenix pools = total %d priced %d unpriced %d, want 2/1/1",
			phx.PoolsTotal, phx.PoolsPriced, phx.UnpricedPools)
	}
	if phx.AsOf == "" || phx.Basis == "" {
		t.Error("phoenix AsOf/Basis must be populated")
	}

	cm, ok := snap["comet"]
	if !ok {
		t.Fatal("comet missing from snapshot")
	}
	// $5 (XLM) + $2.10 (USDC peg), fully priced.
	if cm.TVLUSD != "7.10" {
		t.Errorf("comet TVLUSD = %q, want 7.10", cm.TVLUSD)
	}
	if cm.PoolsTotal != 1 || cm.PoolsPriced != 1 || cm.UnpricedPools != 0 {
		t.Errorf("comet pools = total %d priced %d unpriced %d, want 1/1/0",
			cm.PoolsTotal, cm.PoolsPriced, cm.UnpricedPools)
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

// TestDEXTVLCache_RefreshObservesMetrics pins the paired counter+histogram instrumentation: an all-ok refresh records
// outcome="ok", one with a failing protocol records "error". Uses obstest since per-label histogram children are unreachable via
// testutil.
func TestDEXTVLCache_RefreshObservesMetrics(t *testing.T) {
	okBefore := obstest.HistogramSampleCount(t, obs.DEXTVLRefreshDurationSeconds, "outcome", "ok")
	errBefore := obstest.HistogramSampleCount(t, obs.DEXTVLRefreshDurationSeconds, "outcome", "error")

	src := tvlTestSources()
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := obstest.HistogramSampleCount(t, obs.DEXTVLRefreshDurationSeconds, "outcome", "ok"); got != okBefore+1 {
		t.Errorf("ok observations = %d, want %d", got, okBefore+1)
	}

	src.AquariusReserves.(*stubAquariusReserveReader).err = errors.New("boom")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh with failing protocol should error")
	}
	if got := obstest.HistogramSampleCount(t, obs.DEXTVLRefreshDurationSeconds, "outcome", "error"); got != errBefore+1 {
		t.Errorf("error observations = %d, want %d", got, errBefore+1)
	}
}

// TestDEXTVLCache_PriceReadErrorCarriesThePreviousFigureForward: every fixture protocol holds XLM, so a failed XLM read carries
// all four forward at their previous figures and surfaces as a refresh error, not soroswap at $10.50 (pegged leg only) instead
// of $25.50.
func TestDEXTVLCache_PriceReadErrorCarriesThePreviousFigureForward(t *testing.T) {
	src, pricer, _ := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("healthy Refresh: %v", err)
	}
	before, _ := c.Snapshot()
	if got := before["soroswap"].TVLUSD; got != "25.50" {
		t.Fatalf("fixture drifted: healthy soroswap TVL = %q, want 25.50", got)
	}

	pricer.arm("native", errTVLPriceRead)
	err := c.Refresh(context.Background())
	// Not Fatal: the figures below are the harm, and they are reported
	// whether or not the error surfaced.
	var errText string
	if err == nil {
		t.Error("Refresh returned nil: a failed price read was admitted as a successful refresh")
	} else {
		errText = err.Error()
		if !errors.Is(err, errTVLPriceRead) {
			t.Errorf("Refresh error does not wrap the read failure: %v", err)
		}
	}

	after, _ := c.Snapshot()
	for _, name := range []string{"soroswap", "aquarius", "phoenix", "comet"} {
		if after[name] != before[name] {
			t.Errorf("%s: want the previous figure carried forward %+v, got %+v",
				name, before[name], after[name])
		}
		if !strings.Contains(errText, name) {
			t.Errorf("Refresh error does not name %s: %q", name, errText)
		}
		if snap, ok := c.Protocol(name); !ok || !snap.CarriedForward {
			t.Errorf("%s: published as fresh (CarriedForward=false) on a failed price read", name)
		}
	}
	if got := after["soroswap"].TVLUSD; got != "25.50" {
		t.Errorf("soroswap TVL = %q after a failed XLM price read, want the carried 25.50", got)
	}

	// The failure is memoised for the refresh like any other verdict:
	// four protocols asked about XLM, the failing store was asked once.
	if n := pricer.callsFor("native"); n != 1 {
		t.Errorf("failing price read issued %d times in one refresh, want 1", n)
	}

	// And it does not outlive the refresh that saw it.
	pricer.arm("native", nil)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("recovered Refresh: %v", err)
	}
	recovered, _ := c.Snapshot()
	if got := recovered["soroswap"].TVLUSD; got != "25.50" {
		t.Errorf("recovered soroswap TVL = %q, want 25.50", got)
	}
}

// TestDEXTVLCache_PriceReadErrorIsScopedToTheProtocolsThatHoldTheToken: only soroswap's pair B holds the no-market token, so
// only soroswap carries forward; the other three never asked the failing question and publish this cycle's figure.
func TestDEXTVLCache_PriceReadErrorIsScopedToTheProtocolsThatHoldTheToken(t *testing.T) {
	src, pricer, noMarketID := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("healthy Refresh: %v", err)
	}
	if n := pricer.callsFor(noMarketID); n == 0 {
		t.Fatalf("fixture drifted: the pricer was never asked about %s, so a fault armed on it could not fire", noMarketID)
	}

	pricer.arm(noMarketID, errTVLPriceRead)
	err := c.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh returned nil for a failed price read")
	}
	if !strings.Contains(err.Error(), "soroswap") {
		t.Errorf("error does not name soroswap: %v", err)
	}
	if snap, ok := c.Protocol("soroswap"); !ok || !snap.CarriedForward {
		t.Error("soroswap: published as fresh on a failed price read")
	}
	for _, name := range []string{"aquarius", "phoenix", "comet"} {
		if strings.Contains(err.Error(), name) {
			t.Errorf("%s holds no failing token but was failed: %v", name, err)
		}
		if snap, ok := c.Protocol(name); !ok || snap.CarriedForward {
			t.Errorf("%s: carried forward though it never read the failing token", name)
		}
	}
}

// TestDEXTVLCache_UnpricedTokenIsStillNotAnError: a no-market token answers (ok=false, nil error), a fact about the token, so
// the refresh succeeds and the leg reads no_served_price rather than failing the protocol.
func TestDEXTVLCache_UnpricedTokenIsStillNotAnError(t *testing.T) {
	src, _, _ := priceReadErrorSources(t)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with an unpriced (not erroring) token: %v", err)
	}
	snap, ok := c.Protocol("soroswap")
	if !ok {
		t.Fatal("no soroswap snapshot")
	}
	var found bool
	for _, pool := range snap.Pools {
		for _, leg := range pool.Legs {
			if leg.Token != tvlTestNoMarketToken {
				continue
			}
			found = true
			if leg.Excluded != DEXTVLLegNoServedPrice {
				t.Errorf("unpriced leg excluded = %q, want %q", leg.Excluded, DEXTVLLegNoServedPrice)
			}
		}
	}
	if !found {
		t.Fatal("fixture drifted: no leg holds tvlTestNoMarketToken")
	}
}

// TestDEXTVLCache_GatedTokenContributesNoValue: tvlValuer.rateFor must not value a
// directory-flagged or substance-less token from the USD resolver alone, whose sole floor is
// $0.01 of quote notional; one self-traded minute would otherwise value it at its own VWAP into
// the protocol headline with the pool counted fully priced, so even the "≥" lower-bound hatching
// would hide the gap.
//
// The fixture is the pool shape TestDEXTVLCache_RefreshComputesProtocolTVL asserts on ($10 XLM +
// $10.50 pegged USDC = $20.50, 1/1/0), with the USDC leg withheld. Both halves are pinned: the
// money (the gated leg's $10.50 leaves tvl_usd) and the honesty (the pool moves from priced to
// unpriced, so the surface renders a lower bound).
func TestDEXTVLCache_GatedTokenContributesNoValue(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{
		// The pool's USDC-SAC leg: a token the serving guards refuse.
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": true,
	}}
	src := tvlTestSources()
	src.Gate = gate

	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()

	phx, ok := snap["phoenix"]
	if !ok {
		t.Fatal("phoenix missing from snapshot")
	}
	// $10 from the XLM leg only — the withheld USDC leg's $10.50 is
	// gone, and it must NOT have been re-derived through the declared
	// USD peg either (the peg shortcut is downstream of the gate).
	if phx.TVLUSD != "10.00" {
		t.Errorf("phoenix TVLUSD = %q, want 10.00 (was 20.50 with the gated leg valued)", phx.TVLUSD)
	}
	// The undecodable pool still counts 1 total + 1 unpriced; the gated
	// pool moves from priced → unpriced. Losing value silently while
	// still claiming pools_priced=1 would be the worse bug.
	if phx.PoolsTotal != 2 || phx.PoolsPriced != 0 || phx.UnpricedPools != 2 {
		t.Errorf("phoenix pools = total %d priced %d unpriced %d, want 2/0/2",
			phx.PoolsTotal, phx.PoolsPriced, phx.UnpricedPools)
	}
	if !strings.Contains(phx.Basis, "serving trust gates withhold") {
		t.Errorf("basis = %q, want it to state that gated legs count unpriced", phx.Basis)
	}

	// Comet holds the same two tokens: $5 XLM + $2.10 gated USDC.
	cm, ok := snap["comet"]
	if !ok {
		t.Fatal("comet missing from snapshot")
	}
	if cm.TVLUSD != "5.00" {
		t.Errorf("comet TVLUSD = %q, want 5.00 (was 7.10)", cm.TVLUSD)
	}
	if cm.PoolsTotal != 1 || cm.PoolsPriced != 0 || cm.UnpricedPools != 1 {
		t.Errorf("comet pools = total %d priced %d unpriced %d, want 1/0/1",
			cm.PoolsTotal, cm.PoolsPriced, cm.UnpricedPools)
	}

	// Soroswap: pair A loses its $10.50 USDC leg (priced → unpriced),
	// pair B was already unpriced. $10 + $5 = $15.
	ss := snap["soroswap"]
	if ss.TVLUSD != "15.00" {
		t.Errorf("soroswap TVLUSD = %q, want 15.00 (was 25.50)", ss.TVLUSD)
	}
	if ss.PoolsPriced != 0 || ss.UnpricedPools != 2 {
		t.Errorf("soroswap priced %d unpriced %d, want 0/2", ss.PoolsPriced, ss.UnpricedPools)
	}
}

// TestDEXTVLCache_NoGateKeepsTodaysFigures pins the nil direction: with [pricing_guard] disabled no gate is wired, legs are
// valued unscreened, and Basis must not claim a screen that did not run. Reachable in production: buildDEXTVLValueGate returns a
// nil interface.
func TestDEXTVLCache_NoGateKeepsTodaysFigures(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	if got := snap["phoenix"].TVLUSD; got != "20.50" {
		t.Errorf("ungated phoenix TVLUSD = %q, want the unchanged 20.50", got)
	}
	if strings.Contains(snap["phoenix"].Basis, "serving trust gates withhold") {
		t.Errorf("basis = %q, must not claim a gate that is not wired", snap["phoenix"].Basis)
	}
}

// TestDEXTVLCache_GatedAquariusLegCountsUnpriced covers the protocol whose leg loop skips address-less legs before valuation:
// the gate must apply on the arm that does resolve a token.
func TestDEXTVLCache_GatedAquariusLegCountsUnpriced(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{tvlTestUSDCSAC: true}}
	src := tvlTestSources()
	src.Gate = gate
	// One pool: 1 XLM-SAC ($0.50 at rate 0.5) + 10.5 gated USDC-SAC
	// (which the declared peg would otherwise value at $10.50).
	src.AquariusReserves = &stubAquariusReserveReader{pools: []timescale.AquariusPoolReserve{{
		ContractID: tvlTestAqPool,
		ObservedAt: time.Now(),
		Legs: []timescale.AquariusReserveLeg{
			{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(10_000_000))},
			{TokenIndex: 1, Token: tvlTestUSDCSAC, Reserve: canonical.NewAmount(big.NewInt(105_000_000))},
		},
	}}}
	// Drop the other protocols so the assertion is about aquarius only.
	src.SoroswapReserves = stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{}}
	src.PhoenixReserves = stubPhoenixReserveReader{states: map[string]clickhouse.PhoenixPoolState{}}
	src.CometReserves = stubCometReserveReader{states: map[string]clickhouse.CometPoolState{}}

	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	aq := snap["aquarius"]
	if aq.TVLUSD != "0.50" {
		t.Errorf("aquarius TVLUSD = %q, want 0.50 — the XLM leg only, with the gated"+
			" USDC leg's $10.50 excluded", aq.TVLUSD)
	}
	if aq.PoolsTotal != 1 || aq.PoolsPriced != 0 || aq.UnpricedPools != 1 {
		t.Errorf("aquarius pools = total %d priced %d unpriced %d, want 1/0/1",
			aq.PoolsTotal, aq.PoolsPriced, aq.UnpricedPools)
	}
}

// TestDEXTVLCache_BasisNamesOnlyTheScreensThatRan: Basis is a public claim, rendered verbatim by the explorer's TVL tooltip,
// about which trust screens ran. With disable_substance_gate = true it must not mention the substance floor.
func TestDEXTVLCache_BasisNamesOnlyTheScreensThatRan(t *testing.T) {
	const (
		bothScreens = "; unpriced legs contribute 0, and a leg whose asset the serving trust gates " +
			"withhold (directory-flagged issuer, or a market below the substance floor) " +
			"is counted unpriced rather than valued"
		scamOnly = "; unpriced legs contribute 0, and a leg whose asset the serving trust gates " +
			"withhold (directory-flagged issuer) is counted unpriced rather than valued"
		noScreens = "; unpriced legs contribute 0"
	)
	for _, tc := range []struct {
		name    string
		screens []string
		want    string
	}{
		// The r1 default: both guards wired. Byte-identical to the
		// original sentence — the wire shape must not
		// move for the deployment whose claim was true.
		{"both guards wired", nil, bothScreens},
		// disable_substance_gate = true: the scam directory still
		// withholds, the substance floor does not exist.
		{"substance gate disabled", []string{TVLScreenScamDirectory}, scamOnly},
		// A gate that withholds nothing claims nothing.
		{"no screen wired", []string{}, noScreens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tvlTestSources()
			src.Gate = &stubTVLGate{screens: tc.screens}
			c := NewDEXTVLCache(src)
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			snap, _ := c.Snapshot()
			phx, ok := snap["phoenix"]
			if !ok {
				t.Fatal("phoenix missing from snapshot")
			}
			if !strings.HasSuffix(phx.Basis, tc.want) {
				t.Errorf("basis tail =\n\t%q\nwant\n\t%q", phx.Basis, tc.want)
			}
			if len(tc.screens) == 1 && strings.Contains(phx.Basis, "substance floor") {
				t.Errorf("basis claims the substance floor screen with only the scam gate wired: %q", phx.Basis)
			}
		})
	}
}

// TestDEXTVLCache_SelfListedTokenIsNotValued: anyone can pair any token, so a self-listed token's reserves and VWAP are
// creator-authored. Without the identity screen its reserve is valued into tvl_usd with no lower-bound marker.
// A verified-catalogue asset reached through an undeclared SAC must still be valued.
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

// TestDEXTVLCache_IdentityScreenFailsClosedWithoutCatalogue pins the nil direction: with no catalogue only native XLM and a
// declared USD peg are identified, so a missing wire shrinks the figure, never inflates it.
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

// TestDEXTVLCache_ProtocolCarriesPoolsAcrossAFailedRefresh — a carried figure travels with the
// pools it was summed from and is labelled carried, so the drill-down never shows an empty list
// beside a non-zero number; a recovered refresh clears the label.
func TestDEXTVLCache_ProtocolCarriesPoolsAcrossAFailedRefresh(t *testing.T) {
	src, _ := tvlBranchSources()
	aq := src.AquariusReserves.(*stubAquariusReserveReader)
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	before, _ := c.Protocol("aquarius")
	if before.CarriedForward {
		t.Fatal("a freshly computed protocol is not carried")
	}

	aq.err = errors.New("lake unavailable")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("second Refresh should surface the aquarius error")
	}
	carried, ok := c.Protocol("aquarius")
	if !ok || !carried.CarriedForward {
		t.Fatalf("carried aquarius = %+v ok=%v, want the previous entry labelled carried", carried, ok)
	}
	if !reflect.DeepEqual(carried.Pools, before.Pools) || carried.TVL != before.TVL {
		t.Errorf("carried entry must be the previous cycle's figure AND pools: %+v vs %+v", carried, before)
	}
	if ss, _ := c.Protocol("soroswap"); ss.CarriedForward {
		t.Error("a protocol that refreshed this cycle must not be labelled carried")
	}

	aq.err = nil
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("third Refresh: %v", err)
	}
	if after, _ := c.Protocol("aquarius"); after.CarriedForward {
		t.Error("a recovered refresh clears the carried label")
	}
}

// TestDEXTVLCache_ProtocolColdAndUnknown — no entry before the first
// refresh, and none for a name the snapshot never held.
func TestDEXTVLCache_ProtocolColdAndUnknown(t *testing.T) {
	c := NewDEXTVLCache(DEXTVLSources{})
	if _, ok := c.Protocol("soroswap"); ok {
		t.Error("cold cache must report no entry")
	}
	c = refreshedBranchCache(t)
	if _, ok := c.Protocol("sdex"); ok {
		t.Error("sdex has no derivation and must report no entry")
	}
}

// TestDEXTVLCache_FirstCycleReadFailureIsNamedNotVanished: a protocol whose first reserve read fails has no previous figure
// to carry and must still appear as an excluded entry with lower_bound set, not vanish from tvl_total and the drill-down.
func TestDEXTVLCache_FirstCycleReadFailureIsNamedNotVanished(t *testing.T) {
	src := tvlTestSources()
	src.SoroswapPairs = stubTVLPairsReader{err: errors.New("registry down")}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh returned nil for a failed reserve read")
	}
	if _, ok := c.Protocol("soroswap"); ok {
		t.Fatal("soroswap published a figure on a first-cycle read failure")
	}
	total := c.Total()
	ex, ok := tvlExclusionFor(total, "soroswap")
	if !ok {
		t.Fatal("tvl_total.excluded does not name soroswap, whose read failed this cycle")
	}
	if !strings.Contains(ex.Reason, "read failed") {
		t.Errorf("soroswap exclusion reason = %q, want it to say the read failed", ex.Reason)
	}
	if !total.LowerBound {
		t.Error("tvl_total.lower_bound = false while a derived protocol is missing from the sum")
	}
	status, detail := tvlNotDerivedDetail(t, c, "soroswap")
	if status != http.StatusNotFound || !strings.Contains(detail, "read failed") || strings.Contains(detail, "not wired") {
		t.Errorf("GET soroswap/tvl = %d %q, want 404 saying the read failed this cycle", status, detail)
	}
}

// TestDEXTVLCache_AquariusBasisStatesItsIdentityLimits pins that Basis says aquarius token identities are positional
// recovery over a table with no transaction order, since aquarius carries most of the headline.
func TestDEXTVLCache_AquariusBasisStatesItsIdentityLimits(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, ok := c.Protocol("aquarius")
	if !ok {
		t.Fatal("aquarius missing")
	}
	for _, want := range []string{"recovered by position", "either one's post-state"} {
		if !strings.Contains(snap.TVL.Basis, want) {
			t.Errorf("aquarius basis = %q, want it to state %q", snap.TVL.Basis, want)
		}
	}
}

// TestDEXTVLCache_EmptyReserveReadIsUnavailableNotZero: a reader returning no pools must not publish tvl_usd "0.00" as a
// fresh exact figure admitted to the headline; the reader contract forbids "zero TVL".
func TestDEXTVLCache_EmptyReserveReadIsUnavailableNotZero(t *testing.T) {
	src := tvlTestSources()
	src.PhoenixReserves = stubPhoenixReserveReader{states: map[string]clickhouse.PhoenixPoolState{}}
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v (an empty read is not a read failure)", err)
	}
	if snap, ok := c.Protocol("phoenix"); ok {
		t.Fatalf("phoenix published %q with %d pools from an empty read, want no figure",
			snap.TVL.TVLUSD, snap.TVL.PoolsTotal)
	}
	total := c.Total()
	if total == nil {
		t.Fatal("tvl_total absent")
	}
	for _, name := range total.Protocols {
		if name == "phoenix" {
			t.Error("tvl_total sums phoenix from an empty read")
		}
	}
	ex, ok := tvlExclusionFor(total, "phoenix")
	if !ok || !strings.Contains(ex.Reason, "no pools") {
		t.Errorf("phoenix exclusion = %+v (found %v), want a reason naming the empty read", ex, ok)
	}
	if !total.LowerBound {
		t.Error("tvl_total.lower_bound = false while phoenix is missing from the sum")
	}
	status, detail := tvlNotDerivedDetail(t, c, "phoenix")
	if status != http.StatusNotFound || !strings.Contains(detail, "no pools") {
		t.Errorf("GET phoenix/tvl = %d %q, want 404 naming the empty read", status, detail)
	}
}

// A USD price read can fail in two ways that mean opposite things. "Nobody prices this token" is
// a fact about the token: the leg is excluded as no_served_price and the figure is a lower bound.
// "The price read ERRORED" is a fact about this refresh: the token is as priced as a minute ago
// and we just could not ask.
//
// If rateFor folded both into the first, the error would be memoised as "unpriceable" for the
// whole refresh, every protocol's refresh would SUCCEED, and Refresh's carry-forward (which only
// runs when a protocol refresh returns an error) could not fire. One transient Postgres error on
// the XLM rate would silently drop every XLM leg from the published DEX TVL and admit the
// shrunken total as fresh.

// erroringTVLPricer is stubTVLPricer with a fault that can be armed per
// asset between refreshes, and a per-asset call count.
type erroringTVLPricer struct {
	mu     sync.Mutex
	rates  map[string]string
	failOn map[string]error
	calls  map[string]int
}

func (p *erroringTVLPricer) USDPriceAt(_ context.Context, asset canonical.Asset, _ time.Time) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := asset.String()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[id]++
	if err := p.failOn[id]; err != nil {
		return "", false, err
	}
	r, ok := p.rates[id]
	return r, ok, nil
}

func (p *erroringTVLPricer) arm(id string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failOn = map[string]error{}
	if err != nil {
		p.failOn[id] = err
	}
	p.calls = map[string]int{}
}

func (p *erroringTVLPricer) callsFor(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

var errTVLPriceRead = errors.New("pq: canceling statement due to statement timeout")

// priceReadErrorSources is the shared fixture with soroswap pair B's first leg swapped for a well-formed token nobody prices;
// tvlTestUnpriced fails strkey validation first, so a price-read fault armed on it would never fire.
func priceReadErrorSources(t *testing.T) (DEXTVLSources, *erroringTVLPricer, string) {
	t.Helper()
	src := tvlTestSources()
	reserves, ok := src.SoroswapReserves.(stubTVLReserveReader)
	if !ok {
		t.Fatalf("fixture drifted: SoroswapReserves is %T", src.SoroswapReserves)
	}
	pairB := reserves.states[tvlTestPairB]
	pairB.Token0 = tvlTestNoMarketToken
	reserves.states[tvlTestPairB] = pairB

	asset, ok := tvlAssetForToken(tvlTestNoMarketToken)
	if !ok {
		t.Fatalf("fixture drifted: %s is not a well-formed token", tvlTestNoMarketToken)
	}
	pricer := &erroringTVLPricer{rates: map[string]string{"native": "0.5"}}
	src.Pricer = pricer
	src.Verified = tvlTestCatalogue(tvlTestNoMarketToken)
	return src, pricer, asset.String()
}
