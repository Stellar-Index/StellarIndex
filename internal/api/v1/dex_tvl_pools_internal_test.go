package v1

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Pool ids for the branch fixture. Real deployed contract ids reused as
// opaque well-formed strkeys, one per branch so a failure names the
// branch.
const (
	tvlPoolPriced    = tvlTestPairA      // both legs valued: served price + declared peg
	tvlPoolGated     = tvlTestPairB      // one leg the trust gate withholds
	tvlPoolNoMarket  = tvlTestAqPool     // one leg with no served price
	tvlPoolMalformed = tvlTestPhxPool    // a malformed token + a missing reserve
	tvlPoolEmpty     = tvlTestPhxBadPool // both legs zero: valued at $0, no price consulted
	// tvlTestNoMarketToken is a valid strkey the stub pricer has no rate
	// for and the gate does not withhold.
	tvlTestNoMarketToken = tvlTestCometPool
	// tvlTestWithheldToken is a valid strkey the stub gate withholds. It
	// has to be WELL-FORMED: the shared fixture's tvlTestUnpriced is
	// unpriceable because it fails strkey validation, which is the
	// malformed_token branch, not the withheld one.
	tvlTestWithheldToken = tvlTestPhxPool
)

// tvlBranchSources drives every per-leg branch through soroswap (whose
// reader carries raw *big.Int legs, so nil and negative reserves are
// expressible), the aquarius unresolved-position branch, and the
// phoenix undecodable-pool branch.
func tvlBranchSources() (DEXTVLSources, *stubTVLGate) {
	gate := &stubTVLGate{withhold: map[string]bool{tvlTestWithheldToken: true}}
	return DEXTVLSources{
		SoroswapPairs: stubTVLPairsReader{pairs: []timescale.SoroswapPair{
			{PairStrkey: tvlPoolPriced},
			{PairStrkey: tvlPoolGated},
			{PairStrkey: tvlPoolNoMarket},
			{PairStrkey: tvlPoolMalformed},
			{PairStrkey: tvlPoolEmpty},
		}},
		SoroswapReserves: stubTVLReserveReader{states: map[string]clickhouse.SoroswapPairState{
			// 20 XLM ($10 at rate 0.5) + 10.5 pegged USDC ($10.50).
			tvlPoolPriced: {
				Pair:   tvlPoolPriced,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(200_000_000),
				Token1: tvlTestUSDCSAC, Reserve1: big.NewInt(105_000_000),
				Ledger: 10,
			},
			// A withheld token + 10 XLM ($5).
			tvlPoolGated: {
				Pair:   tvlPoolGated,
				Token0: tvlTestWithheldToken, Reserve0: big.NewInt(999),
				Token1: canonical.XLMSacContractID, Reserve1: big.NewInt(100_000_000),
				Ledger: 30,
			},
			// A token with no served price + 4 XLM ($2).
			tvlPoolNoMarket: {
				Pair:   tvlPoolNoMarket,
				Token0: tvlTestNoMarketToken, Reserve0: big.NewInt(7),
				Token1: canonical.XLMSacContractID, Reserve1: big.NewInt(40_000_000),
				Ledger: 20,
			},
			// A malformed token and a leg whose reserve was never captured.
			tvlPoolMalformed: {
				Pair:   tvlPoolMalformed,
				Token0: "not-a-strkey", Reserve0: big.NewInt(1),
				Token1: canonical.XLMSacContractID, Reserve1: nil,
				Ledger: 5,
			},
			// Both sides empty.
			tvlPoolEmpty: {
				Pair:   tvlPoolEmpty,
				Token0: canonical.XLMSacContractID, Reserve0: big.NewInt(0),
				Token1: tvlTestUSDCSAC, Reserve1: big.NewInt(0),
				Ledger: 1,
			},
		}},
		AquariusReserves: &stubAquariusReserveReader{pools: []timescale.AquariusPoolReserve{{
			ContractID: tvlTestAqPool,
			Ledger:     tvlTestLedgerAquarius,
			Legs: []timescale.AquariusReserveLeg{
				{TokenIndex: 0, Token: canonical.XLMSacContractID, Reserve: canonical.NewAmount(big.NewInt(10_000_000))},
				{TokenIndex: 1, Token: "", Reserve: canonical.NewAmount(big.NewInt(42))},
			},
		}}},
		PhoenixPools:    []string{tvlTestPhxBadPool},
		PhoenixReserves: stubPhoenixReserveReader{undecodable: []string{tvlTestPhxBadPool}},
		Pricer:          stubTVLPricer{rates: map[string]string{"native": "0.5"}},
		PegInfo:         stubTVLPegInfo{pegged: map[string]int{tvlTestUSDCSAC: 7}},
		Gate:            gate,
	}, gate
}

func refreshedBranchCache(t *testing.T) *DEXTVLCache {
	t.Helper()
	src, _ := tvlBranchSources()
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c
}

func poolByID(t *testing.T, pools []DEXTVLPoolView, id string) DEXTVLPoolView {
	t.Helper()
	for _, p := range pools {
		if p.Pool == id {
			return p
		}
	}
	t.Fatalf("pool %s missing from %+v", id, pools)
	return DEXTVLPoolView{}
}

// TestDEXTVLPools_EveryLegBranchIsLabelled drives each way a leg can be
// valued or excluded and asserts the wire says which (#338): a valued
// leg carries asset + basis + usd and no exclusion; an excluded leg
// carries the reason and NO usd — never a silent "0.00".
func TestDEXTVLPools_EveryLegBranchIsLabelled(t *testing.T) {
	c := refreshedBranchCache(t)
	ss, ok := c.Protocol("soroswap")
	if !ok {
		t.Fatal("soroswap missing")
	}

	cases := []struct {
		name    string
		pool    string
		leg     int
		want    DEXTVLLegView
		priced  bool
		poolUSD string
	}{
		{"served price", tvlPoolPriced, 0, DEXTVLLegView{Token: canonical.XLMSacContractID, Reserve: "200000000", Asset: "native", Basis: DEXTVLBasisServedUSDPrice, USD: "10.00"}, true, "20.50"},
		{"declared peg", tvlPoolPriced, 1, DEXTVLLegView{Token: tvlTestUSDCSAC, Reserve: "105000000", Asset: tvlTestUSDCSAC, Basis: DEXTVLBasisDeclaredUSDPeg, USD: "10.50"}, true, "20.50"},
		{"withheld", tvlPoolGated, 0, DEXTVLLegView{Token: tvlTestWithheldToken, Reserve: "999", Asset: tvlTestWithheldToken, Excluded: DEXTVLLegWithheld}, false, "5.00"},
		{"no served price", tvlPoolNoMarket, 0, DEXTVLLegView{Token: tvlTestNoMarketToken, Reserve: "7", Asset: tvlTestNoMarketToken, Excluded: DEXTVLLegNoServedPrice}, false, "2.00"},
		{"malformed token", tvlPoolMalformed, 0, DEXTVLLegView{Token: "not-a-strkey", Reserve: "1", Excluded: DEXTVLLegMalformedToken}, false, "0.00"},
		{"missing reserve", tvlPoolMalformed, 1, DEXTVLLegView{Token: canonical.XLMSacContractID, Excluded: DEXTVLLegInvalidReserve}, false, "0.00"},
		{"empty reserve", tvlPoolEmpty, 0, DEXTVLLegView{Token: canonical.XLMSacContractID, Reserve: "0", Asset: "native", Basis: DEXTVLBasisEmptyReserve, USD: "0.00"}, true, "0.00"},
		{"empty pegged reserve", tvlPoolEmpty, 1, DEXTVLLegView{Token: tvlTestUSDCSAC, Reserve: "0", Asset: tvlTestUSDCSAC, Basis: DEXTVLBasisEmptyReserve, USD: "0.00"}, true, "0.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := poolByID(t, ss.Pools, tc.pool)
			if got := p.Legs[tc.leg]; got != tc.want {
				t.Errorf("leg = %+v, want %+v", got, tc.want)
			}
			if p.Priced != tc.priced || p.TVLUSD != tc.poolUSD {
				t.Errorf("pool = priced %v tvl %q, want priced %v tvl %q", p.Priced, p.TVLUSD, tc.priced, tc.poolUSD)
			}
			if p.Excluded != "" {
				t.Errorf("a decoded pool carries no pool-level exclusion, got %q", p.Excluded)
			}
		})
	}

	// Every leg is EITHER valued (basis + usd) OR excluded — never both,
	// never neither.
	for _, p := range ss.Pools {
		for _, leg := range p.Legs {
			valued := leg.USD != "" && leg.Basis != ""
			if valued == (leg.Excluded != "") {
				t.Errorf("pool %s leg %+v must be exactly one of valued or excluded", p.Pool, leg)
			}
		}
	}
	// The protocol view is the accumulator's, so the counts must agree
	// with the pools published beside it.
	if ss.TVL.PoolsTotal != 5 || ss.TVL.PoolsPriced != 2 || ss.TVL.UnpricedPools != 3 {
		t.Errorf("soroswap pools = %d/%d priced/%d unpriced, want 5/2/3", ss.TVL.PoolsTotal, ss.TVL.PoolsPriced, ss.TVL.UnpricedPools)
	}
	if ss.TVL.TVLUSD != "27.50" {
		t.Errorf("soroswap tvl_usd = %q, want 27.50 (20.50 + 5.00 + 2.00 + 0.00 + 0.00)", ss.TVL.TVLUSD)
	}
	if ss.TVL.AsOfLedger != 30 {
		t.Errorf("soroswap as_of_ledger = %d, want the max pool ledger 30", ss.TVL.AsOfLedger)
	}

	aq, _ := c.Protocol("aquarius")
	unresolved := poolByID(t, aq.Pools, tvlTestAqPool).Legs[1]
	if want := (DEXTVLLegView{Reserve: "42", Excluded: DEXTVLLegUnresolvedToken}); unresolved != want {
		t.Errorf("aquarius unresolved leg = %+v, want %+v", unresolved, want)
	}

	phx, _ := c.Protocol("phoenix")
	bad := poolByID(t, phx.Pools, tvlTestPhxBadPool)
	if bad.Excluded != DEXTVLPoolUndecodable || bad.TVLUSD != "0.00" || bad.Priced || bad.Legs == nil || len(bad.Legs) != 0 {
		t.Errorf("undecodable pool = %+v, want excluded=undecodable_storage, 0.00, unpriced, legs []", bad)
	}
	if phx.TVL.PoolsTotal != 1 || phx.TVL.UnpricedPools != 1 {
		t.Errorf("phoenix pools = %+v, want the undecodable pool counted total + unpriced", phx.TVL)
	}
}

// TestDEXTVLPools_ReconcileAtEveryLevel is the per-pool half of the
// acceptance criterion "a reconciliation test against pool reserves"
// (#338): on every protocol of both fixtures, the sum of the published
// leg usd strings is the pool's tvl_usd and the sum of the published
// pool tvl_usd strings is the protocol's — byte-for-byte, the property
// a consumer of the drill-down actually checks.
func TestDEXTVLPools_ReconcileAtEveryLevel(t *testing.T) {
	branch, _ := tvlBranchSources()
	for name, src := range map[string]DEXTVLSources{"shared": tvlTestSources(), "branch": branch} {
		t.Run(name, func(t *testing.T) {
			c := NewDEXTVLCache(src)
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			snap, _ := c.Snapshot()
			for protocol := range snap {
				p, ok := c.Protocol(protocol)
				if !ok {
					t.Fatalf("%s: Protocol() missing", protocol)
				}
				if len(p.Pools) != p.TVL.PoolsTotal {
					t.Errorf("%s: %d pools published, pools_total says %d", protocol, len(p.Pools), p.TVL.PoolsTotal)
				}
				protocolSum := new(big.Rat)
				for _, pool := range p.Pools {
					poolSum := new(big.Rat)
					for _, leg := range pool.Legs {
						if leg.USD == "" {
							continue
						}
						usd, ok := new(big.Rat).SetString(leg.USD)
						if !ok || strings.Count(leg.USD, ".") != 1 || len(leg.USD)-strings.Index(leg.USD, ".") != 3 {
							t.Fatalf("%s %s leg usd %q is not a two-place decimal", protocol, pool.Pool, leg.USD)
						}
						poolSum.Add(poolSum, usd)
					}
					if got := poolSum.FloatString(2); got != pool.TVLUSD {
						t.Errorf("%s %s: legs sum to %q, pool publishes %q", protocol, pool.Pool, got, pool.TVLUSD)
					}
					pv, _ := new(big.Rat).SetString(pool.TVLUSD)
					protocolSum.Add(protocolSum, pv)
				}
				if got := protocolSum.FloatString(2); got != p.TVL.TVLUSD {
					t.Errorf("%s: pools sum to %q, protocol publishes %q", protocol, got, p.TVL.TVLUSD)
				}
			}
		})
	}
}

// TestDEXTVLPools_OrderedLargestFirst — the drill-down is deterministic
// across refreshes: pools by published value descending, ties by id.
func TestDEXTVLPools_OrderedLargestFirst(t *testing.T) {
	c := refreshedBranchCache(t)
	ss, _ := c.Protocol("soroswap")
	var order []string
	for _, p := range ss.Pools {
		order = append(order, p.Pool)
	}
	want := []string{tvlPoolPriced, tvlPoolGated, tvlPoolNoMarket}
	// The two "0.00" pools tie and fall back to id order.
	if tvlPoolMalformed < tvlPoolEmpty {
		want = append(want, tvlPoolMalformed, tvlPoolEmpty)
	} else {
		want = append(want, tvlPoolEmpty, tvlPoolMalformed)
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("pool order = %v, want %v", order, want)
	}
}

// TestDEXTVLCache_ProtocolCarriesPoolsAcrossAFailedRefresh — a carried
// figure travels with the pools it was summed from and is LABELLED
// carried, so the drill-down shows what the headline refused rather
// than an empty list beside a non-zero number; a recovered refresh
// clears the label.
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

// TestDEXTVLNotDerivedReason — the 404 on a known protocol says the
// same thing tvl_total.excluded says where a standing exclusion names
// it, and points at that list otherwise.
func TestDEXTVLNotDerivedReason(t *testing.T) {
	if got := dexTVLNotDerivedReason("sdex"); !strings.Contains(got, "order book") {
		t.Errorf("sdex reason = %q, want the standing order-book exclusion", got)
	}
	if got := dexTVLNotDerivedReason("blend"); !strings.Contains(got, "lending") {
		t.Errorf("blend reason = %q, want the standing lending exclusion", got)
	}
	if got := dexTVLNotDerivedReason("soroswap"); !strings.Contains(got, "not wired") || !strings.Contains(got, "tvl_total.excluded") {
		t.Errorf("unwired reason = %q, want the generic not-wired statement pointing at tvl_total.excluded", got)
	}
}
