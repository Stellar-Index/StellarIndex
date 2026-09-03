package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// tvlReconcileLeg is one reserve leg as the FIXTURE declares it, so the
// reconciliation below re-derives the expected total from the reserve
// inputs rather than restating the production accumulator's arithmetic.
type tvlReconcileLeg struct {
	protocol string
	asset    string // canonical asset String(), or "" for an unpriceable leg
	raw      int64  // reserve in the token's base units
}

// tvlReconcileFixture mirrors tvlTestSources()'s pool reserves exactly:
// every leg of every pool of every protocol, in base units. This is the
// "per-pool data we already store" side of the reconciliation (#338) —
// edit tvlTestSources and this must be edited in lockstep, which is the
// whole point of keeping it a separate, independently-summed statement
// of the same reserves.
var tvlReconcileFixture = []tvlReconcileLeg{
	// soroswap pair A: 20 XLM + 10.5 USDC.
	{"soroswap", "native", 200_000_000},
	{"soroswap", tvlTestUSDCSAC, 105_000_000},
	// soroswap pair B: an unpriceable token + 10 XLM.
	{"soroswap", "", 999},
	{"soroswap", "native", 100_000_000},
	// aquarius: 1 XLM + one leg whose token address never resolved.
	{"aquarius", "native", 10_000_000},
	{"aquarius", "", 42},
	// phoenix: 20 XLM + 10.5 USDC (the undecodable pool contributes 0).
	{"phoenix", "native", 200_000_000},
	{"phoenix", tvlTestUSDCSAC, 105_000_000},
	// comet: 10 XLM + 2.1 USDC.
	{"comet", "native", 100_000_000},
	{"comet", tvlTestUSDCSAC, 21_000_000},
}

// tvlReconcileExpected sums tvlReconcileFixture per protocol using the
// two documented valuation identities and NOTHING from dex_tvl_cache.go:
//
//   - a declared USD peg (USDC here, 7 decimals) is worth exactly
//     $1 × raw / 10^decimals;
//   - anything else is raw × rate / 10^7, the anchor-scale identity in
//     tvlValuer.legUSD's contract.
//
// An unpriceable leg contributes exactly zero (the lower-bound rule).
func tvlReconcileExpected() (map[string]*big.Rat, *big.Rat) {
	perProtocol := map[string]*big.Rat{}
	grand := new(big.Rat)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(7), nil)
	xlmRate := big.NewRat(1, 2) // stubTVLPricer's "native": "0.5"

	for _, leg := range tvlReconcileFixture {
		if perProtocol[leg.protocol] == nil {
			perProtocol[leg.protocol] = new(big.Rat)
		}
		var usd *big.Rat
		switch leg.asset {
		case "":
			continue
		case tvlTestUSDCSAC:
			usd = new(big.Rat).SetFrac(big.NewInt(leg.raw), scale)
		default:
			usd = new(big.Rat).SetFrac(big.NewInt(leg.raw), scale)
			usd.Mul(usd, xlmRate)
		}
		perProtocol[leg.protocol].Add(perProtocol[leg.protocol], usd)
		grand.Add(grand, usd)
	}
	return perProtocol, grand
}

// TestDEXTVLTotal_ReconcilesAgainstPoolReserves is the acceptance
// criterion "a reconciliation test against pool reserves" (#338). It
// re-derives every protocol's figure AND the headline total straight
// from the fixture's reserve legs, then asserts three separate things:
//
//  1. each published per-protocol tvl_usd equals its reserves;
//  2. the published total equals the reserves of every included pool;
//  3. the published total is byte-identical to the sum of the published
//     PARTS — the property a consumer actually checks, and the one that
//     would break the moment the total were computed from the unrounded
//     rationals instead of from what we serve.
func TestDEXTVLTotal_ReconcilesAgainstPoolReserves(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()
	perProtocol, grand := tvlReconcileExpected()

	for name, want := range perProtocol {
		part, ok := snap[name]
		if !ok {
			t.Fatalf("%s missing from snapshot", name)
		}
		if got := part.TVLUSD; got != want.FloatString(2) {
			t.Errorf("%s tvl_usd = %q, reserves reconcile to %q", name, got, want.FloatString(2))
		}
	}

	total := c.Total()
	if total == nil {
		t.Fatal("Total() is nil after a successful refresh")
	}
	if total.TVLUSD != grand.FloatString(2) {
		t.Errorf("total tvl_usd = %q, pool reserves reconcile to %q", total.TVLUSD, grand.FloatString(2))
	}
	// 25.50 + 0.50 + 20.50 + 7.10 — pinned literally so a fixture edit
	// that moves BOTH sides of the reconciliation still trips here.
	if total.TVLUSD != "53.60" {
		t.Errorf("total tvl_usd = %q, want 53.60", total.TVLUSD)
	}

	sumOfParts := new(big.Rat)
	for _, name := range total.Protocols {
		part, ok := new(big.Rat).SetString(snap[name].TVLUSD)
		if !ok {
			t.Fatalf("published %s tvl_usd %q does not parse", name, snap[name].TVLUSD)
		}
		sumOfParts.Add(sumOfParts, part)
	}
	if got := sumOfParts.FloatString(2); got != total.TVLUSD {
		t.Errorf("sum of published parts = %q, published total = %q — a consumer adding up the "+
			"rows must land exactly on the headline", got, total.TVLUSD)
	}

	if len(total.Protocols) != 4 {
		t.Errorf("total.Protocols = %v, want all four AMMs", total.Protocols)
	}
	if total.PoolsTotal != 6 || total.PoolsPriced != 3 || total.UnpricedPools != 3 {
		t.Errorf("total pools = %d/%d priced, %d unpriced; want 6/3/3",
			total.PoolsTotal, total.PoolsPriced, total.UnpricedPools)
	}
	if !total.LowerBound {
		t.Error("total.LowerBound must be true while any included protocol has unpriced pools")
	}
	if total.Basis == "" {
		t.Error("total.Basis must be populated")
	}
}

// TestDEXTVLSnapshot_StampsTheChainHighWater pins as_of_ledger — the
// acceptance criterion "per-protocol TVL + total, each with as-of
// ledger". Before this the cache discarded every reader's ledger and
// the only stamp was the refresh wall-clock, which says nothing about
// the chain position the reserves were read at.
func TestDEXTVLSnapshot_StampsTheChainHighWater(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap, _ := c.Snapshot()

	for name, want := range map[string]uint32{
		"soroswap": tvlTestLedgerPairB, // max over pairA/pairB, not first-seen
		"aquarius": tvlTestLedgerAquarius,
		"phoenix":  tvlTestLedgerPhoenix,
		"comet":    tvlTestLedgerComet,
	} {
		if got := snap[name].AsOfLedger; got != want {
			t.Errorf("%s as_of_ledger = %d, want %d", name, got, want)
		}
	}

	total := c.Total()
	if total == nil {
		t.Fatal("Total() is nil after a successful refresh")
	}
	if total.AsOfLedger != tvlTestLedgerPhoenix {
		t.Errorf("total as_of_ledger = %d, want the global high-water %d",
			total.AsOfLedger, tvlTestLedgerPhoenix)
	}
}

// TestDEXTVLTotal_RefusesACarriedForwardProtocol is the divergence
// check (#338): when a protocol's reserve read fails, Refresh carries
// its PREVIOUS figure forward, and that figure cannot honestly be
// published under this refresh's as_of. The total must drop it, name
// it, and shrink — not absorb it and serve a total whose as_of no
// component honours.
func TestDEXTVLTotal_RefusesACarriedForwardProtocol(t *testing.T) {
	divergentBefore := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent"))
	src := tvlTestSources()
	c := NewDEXTVLCache(src)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if got := c.Total().TVLUSD; got != "53.60" {
		t.Fatalf("baseline total = %q, want 53.60", got)
	}

	// Break aquarius' reader; its entry is carried forward verbatim.
	src.AquariusReserves.(*stubAquariusReserveReader).err = errors.New("lake unavailable")
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("second Refresh should report the aquarius read failure")
	}

	snap, _ := c.Snapshot()
	if _, ok := snap["aquarius"]; !ok {
		t.Fatal("aquarius should still be in the snapshot (carried forward)")
	}
	total := c.Total()
	if total == nil {
		t.Fatal("Total() is nil after a degraded refresh")
	}
	// 53.60 - 0.50 (aquarius) = 53.10: the total SHRANK by exactly the
	// refused part rather than republishing a stale one.
	if total.TVLUSD != "53.10" {
		t.Errorf("total tvl_usd = %q, want 53.10 (aquarius refused)", total.TVLUSD)
	}
	for _, name := range total.Protocols {
		if name == "aquarius" {
			t.Error("aquarius must not be named as a contributor to the total")
		}
	}
	var found bool
	for _, ex := range total.Excluded {
		if ex.Subject == "aquarius" {
			found = true
			if ex.Reason != dexTVLRefusedStale {
				t.Errorf("aquarius exclusion reason = %q, want the stale reason", ex.Reason)
			}
		}
	}
	if !found {
		t.Errorf("aquarius must be named in total.Excluded; got %+v", total.Excluded)
	}
	if !total.LowerBound {
		t.Error("a total with a refused component is a lower bound")
	}
	if got, want := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent")), divergentBefore+1; got != want {
		t.Errorf("dex_tvl_reconcile_total{divergent} = %v, want %v", got, want)
	}
}

// TestDEXTVLTotal_RefusesUnbalancedPoolAccounting drives the remaining
// two refusals directly: a part whose pool counts do not balance (its
// coverage claim, and therefore its lower-bound story, is unprovable)
// and a part whose published figure is not a usable decimal.
func TestDEXTVLTotal_RefusesUnbalancedPoolAccounting(t *testing.T) {
	at := time.Date(2026, 9, 3, 4, 30, 32, 0, time.UTC)
	stamp := at.Format(time.RFC3339)
	total := reconcileDEXTVLTotal(map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00", PoolsTotal: 2, PoolsPriced: 1, UnpricedPools: 1, AsOf: stamp, AsOfLedger: 7},
		// 3 != 1 + 1 — a pool is counted in neither column.
		"aquarius": {TVLUSD: "99.00", PoolsTotal: 3, PoolsPriced: 1, UnpricedPools: 1, AsOf: stamp, AsOfLedger: 9},
		// Not a decimal at all.
		"phoenix": {TVLUSD: "n/a", PoolsTotal: 1, PoolsPriced: 1, AsOf: stamp, AsOfLedger: 11},
	}, at, nil)

	if total.TVLUSD != "10.00" {
		t.Errorf("total tvl_usd = %q, want 10.00 (only soroswap admitted)", total.TVLUSD)
	}
	if len(total.Protocols) != 1 || total.Protocols[0] != "soroswap" {
		t.Errorf("total.Protocols = %v, want [soroswap]", total.Protocols)
	}
	if total.AsOfLedger != 7 {
		t.Errorf("total as_of_ledger = %d, want 7 — refused parts must not stamp it", total.AsOfLedger)
	}
	reasons := map[string]string{}
	for _, ex := range total.Excluded {
		reasons[ex.Subject] = ex.Reason
	}
	if reasons["aquarius"] != dexTVLRefusedPoolAccounting {
		t.Errorf("aquarius reason = %q, want the pool-accounting reason", reasons["aquarius"])
	}
	if reasons["phoenix"] != dexTVLRefusedUnparseable {
		t.Errorf("phoenix reason = %q, want the unparseable reason", reasons["phoenix"])
	}
}

// TestDEXTVLTotal_AlwaysPublishesTheScopeExclusions: the total is not a
// whole-network figure, and the surfaces it omits are named on every
// response rather than left to be inferred from an absence.
func TestDEXTVLTotal_AlwaysPublishesTheScopeExclusions(t *testing.T) {
	c := NewDEXTVLCache(tvlTestSources())
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	subjects := map[string]bool{}
	for _, ex := range c.Total().Excluded {
		if ex.Reason == "" {
			t.Errorf("exclusion %q carries no reason", ex.Subject)
		}
		subjects[ex.Subject] = true
	}
	for _, want := range []string{"classic liquidity pools", "sdex", "blend", "sorocredit", "defindex"} {
		if !subjects[want] {
			t.Errorf("total.Excluded must name %q; got %v", want, subjects)
		}
	}
}

// TestDEXTVLTotal_ColdStartHasNoTotal — before the first refresh there
// is nothing to total, and the field stays absent rather than serving
// a fabricated $0 headline.
func TestDEXTVLTotal_ColdStartHasNoTotal(t *testing.T) {
	if got := NewDEXTVLCache(DEXTVLSources{}).Total(); got != nil {
		t.Errorf("cold Total() = %+v, want nil", got)
	}
	if got := reconcileDEXTVLTotal(map[string]ProtocolTVLView{}, time.Now(), nil); got != nil {
		t.Errorf("empty-snapshot total = %+v, want nil", got)
	}
}

// TestDEXTVLTotal_AllRefusedPublishesNoTotal — when reconciliation admits
// NOTHING, the total must be absent, not "0.00".
//
// This is the shape the sibling refusal tests do not reach: they each
// refuse one protocol out of several, so a real number always survives.
// Refuse every one and the summing loop lands on an empty *big.Rat,
// which renders as a perfectly plausible "$0.00" — the single reading
// that is definitely wrong, since the per-protocol figures sitting right
// beside it on the same response are non-zero. `tvl_total` is a pointer
// with `omitempty` precisely so it can be absent; a consumer that reads
// tvl_usd without also reading basis and excluded then gets silence
// rather than a fabricated zero.
func TestDEXTVLTotal_AllRefusedPublishesNoTotal(t *testing.T) {
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339)

	// Two protocols, both admissible on their own...
	snapshot := map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00", AsOf: stamp, PoolsTotal: 2, PoolsPriced: 2},
		"phoenix":  {TVLUSD: "5.00", AsOf: stamp, PoolsTotal: 1, PoolsPriced: 1},
	}
	if got := reconcileDEXTVLTotal(snapshot, at, nil); got == nil || got.TVLUSD != "15.00" {
		t.Fatalf("baseline total = %+v, want 15.00 — the fixture itself must be admissible", got)
	}

	// ...now carry BOTH forward, so every part is refused.
	divergentBefore := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent"))
	total := reconcileDEXTVLTotal(snapshot, at, []string{"soroswap", "phoenix"})
	if total != nil {
		t.Fatalf("total = %+v, want nil — an all-refused cycle must publish NO total, "+
			"never a $%s that reads as a real figure of zero", total, total.TVLUSD)
	}
	if got := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent")); got != divergentBefore+1 {
		t.Errorf("divergent counter = %v, want %v — an omitted total is still a divergence "+
			"and must be visible to an operator, not silently absent", got, divergentBefore+1)
	}
}
