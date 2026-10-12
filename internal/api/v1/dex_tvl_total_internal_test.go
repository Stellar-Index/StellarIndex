package v1

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

// tvlReconcileFixture mirrors tvlTestSources()'s pool reserves exactly: every leg of every pool
// of every protocol, in base units. It is the independently-summed per-pool side of the
// reconciliation, so editing tvlTestSources means editing this in lockstep.
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

// tvlReconcileExpected sums tvlReconcileFixture per protocol using only the documented identities, (not dex_tvl_cache.go):
// a declared USD peg is $1 x raw / 10^decimals; anything else is raw x rate / 10^7. An unpriceable leg contributes zero.
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

// TestDEXTVLTotal_ReconcilesAgainstPoolReserves re-derives every protocol's figure and the
// headline total straight from the fixture's reserve legs, then asserts:
//
//  1. each published per-protocol tvl_usd equals its reserves;
//  2. the published total equals the reserves of every included pool;
//  3. the published total is byte-identical to the sum of the published parts, the property a
//     consumer checks, which breaks if the total is computed from unrounded rationals instead of
//     from what is served.
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

// TestDEXTVLSnapshot_StampsTheChainHighWater pins as_of_ledger on per-protocol TVL and the total.
// The refresh wall-clock says nothing about the chain position the reserves were read at.
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

// TestDEXTVLTotal_RefusesACarriedForwardProtocol: when a protocol's reserve read fails, Refresh
// carries its previous figure forward, which cannot honestly be published under this refresh's
// as_of. The total must drop it, name it and shrink, not absorb it.
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

// TestDEXTVLTotal_RefusesUnbalancedPoolAccounting drives the remaining two refusals: a part whose pool counts do not balance
// (its coverage claim is unprovable) and a part whose published figure is not a usable decimal.
func TestDEXTVLTotal_RefusesUnbalancedPoolAccounting(t *testing.T) {
	at := time.Date(2026, 9, 3, 4, 30, 32, 0, time.UTC)
	stamp := at.Format(time.RFC3339)
	total := reconcileDEXTVLTotal(map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00", PoolsTotal: 2, PoolsPriced: 1, UnpricedPools: 1, AsOf: stamp, AsOfLedger: 7},
		// 3 != 1 + 1 — a pool is counted in neither column.
		"aquarius": {TVLUSD: "99.00", PoolsTotal: 3, PoolsPriced: 1, UnpricedPools: 1, AsOf: stamp, AsOfLedger: 9},
		// Not a decimal at all.
		"phoenix": {TVLUSD: "n/a", PoolsTotal: 1, PoolsPriced: 1, AsOf: stamp, AsOfLedger: 11},
	}, at, nil, nil)

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
	if got := reconcileDEXTVLTotal(map[string]ProtocolTVLView{}, time.Now(), nil, nil); got != nil {
		t.Errorf("empty-snapshot total = %+v, want nil", got)
	}
}

// TestDEXTVLTotal_AllRefusedPublishesNoTotal: when reconciliation admits nothing the total must be absent, not "0.00".
// An empty *big.Rat would render a plausible "$0.00"; tvl_total is an omitempty pointer so it can be silent instead.
func TestDEXTVLTotal_AllRefusedPublishesNoTotal(t *testing.T) {
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339)

	// Two protocols, both admissible on their own...
	snapshot := map[string]ProtocolTVLView{
		"soroswap": {TVLUSD: "10.00", AsOf: stamp, PoolsTotal: 2, PoolsPriced: 2},
		"phoenix":  {TVLUSD: "5.00", AsOf: stamp, PoolsTotal: 1, PoolsPriced: 1},
	}
	if got := reconcileDEXTVLTotal(snapshot, at, nil, nil); got == nil || got.TVLUSD != "15.00" {
		t.Fatalf("baseline total = %+v, want 15.00 — the fixture itself must be admissible", got)
	}

	// ...now carry BOTH forward, so every part is refused.
	divergentBefore := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent"))
	total := reconcileDEXTVLTotal(snapshot, at, []string{"soroswap", "phoenix"}, nil)
	if total != nil {
		t.Fatalf("total = %+v, want nil — an all-refused cycle must publish NO total, "+
			"never a $%s that reads as a real figure of zero", total, total.TVLUSD)
	}
	if got := testutil.ToFloat64(obs.DEXTVLReconcileTotal.WithLabelValues("divergent")); got != divergentBefore+1 {
		t.Errorf("divergent counter = %v, want %v — an omitted total is still a divergence "+
			"and must be visible to an operator, not silently absent", got, divergentBefore+1)
	}
}

// TestDEXTVLTotal_NoStaleIssueCitation guards that dex_tvl_total.go comments and the classic-liquidity-pools exclusion Reason
// do not cite the closed D4 DEX TVL tracking issue as still-open work.
func TestDEXTVLTotal_NoStaleIssueCitation(t *testing.T) {
	src, err := os.ReadFile("dex_tvl_total.go")
	if err != nil {
		t.Fatalf("read dex_tvl_total.go: %v", err)
	}
	if strings.Contains(string(src), "#338") {
		t.Fatal("dex_tvl_total.go still cites closed issue #338 (RLT-429)")
	}
}

// A pooled-liquidity protocol may be summed into the headline or left out, but never silently: an indexed AMM missing from
// tvl_total.excluded leaves readers unable to see it, and the drill-down would wrongly blame deployment wiring.
// Both surfaces read dexTVLScopeExclusions, so this guard makes the next pooled protocol declare itself.
func TestDEXTVLScope_PooledProtocolIsDerivedOrExplicitlyExcluded(t *testing.T) {
	t.Parallel()

	derived := map[string]bool{}
	for _, d := range NewDEXTVLCache(DEXTVLSources{}).derivations() {
		derived[d.name] = true
	}
	if len(derived) == 0 {
		t.Fatal("no TVL derivations enumerated — this guard needs re-aiming")
	}

	excluded := map[string]bool{}
	for _, ex := range dexTVLScopeExclusions {
		excluded[ex.Subject] = true
	}

	for _, p := range protocolRegistry {
		// Pooled-liquidity and vault/AUM categories only. Lending/oracle/bridge protocols are not
		// AMM-headline candidates, and those carrying a comparable figure (blend, sorocredit,
		// defindex, upshift) already have their own exclusion. "yield" is included because a
		// vault's AUM has the same double-count/no-current-state shape a reader might sum.
		if p.Category != "amm" && p.Category != "dex" && p.Category != "yield" {
			continue
		}
		if derived[p.Name] || excluded[p.Name] {
			continue
		}
		t.Errorf("protocol %q is category %q but is neither summed into tvl_total nor named in "+
			"dexTVLScopeExclusions: the headline omits it with no reason on the wire, and "+
			"GET /v1/protocols/%s/tvl blames the omission on deployment wiring. Add a scope "+
			"exclusion saying why, or wire a derivation.", p.Name, p.Category, p.Name)
	}
}

// tvlExclusionFor returns the headline's excluded entry for subject.
func tvlExclusionFor(total *DEXTVLTotalView, subject string) (DEXTVLExclusion, bool) {
	if total == nil {
		return DEXTVLExclusion{}, false
	}
	for _, ex := range total.Excluded {
		if ex.Subject == subject {
			return ex, true
		}
	}
	return DEXTVLExclusion{}, false
}

// tvlNotDerivedDetail GETs /v1/protocols/{name}/tvl and returns the
// status and problem detail.
func tvlNotDerivedDetail(t *testing.T, c *DEXTVLCache, name string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{DEXTVL: c}).Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/protocols/"+name+"/tvl", nil))
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, p.Detail
}

// stubTVLGate withholds exactly the canonical assets it is told to, keyed on Asset.String() so tests can pin the identity asked.
// screens mirrors the production adapter unless set, because the Basis sentence is composed from it.
type stubTVLGate struct {
	withhold map[string]bool
	asked    []string
	screens  []string
}

func (g *stubTVLGate) ValueWithheld(_ context.Context, asset canonical.Asset) bool {
	g.asked = append(g.asked, asset.String())
	return g.withhold[asset.String()]
}

func (g *stubTVLGate) Screens() []string {
	if g.screens == nil {
		return []string{TVLScreenScamDirectory, TVLScreenSubstanceFloor}
	}
	return g.screens
}

// TestTVLValuer_GateSeesTheCanonicalIdentity pins the SAC collapse: the scam directory is keyed by classic issuer G-address,
// so a configured classic/SAC wrapper must reach the gate as its classic twin, and native's SAC as `native`.
// Not parallel: the AliasRegistry is process-global.
func TestTVLValuer_GateSeesTheCanonicalIdentity(t *testing.T) {
	const (
		scamCode   = "SCAM"
		scamIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	classic, err := canonical.NewClassicAsset(scamCode, scamIssuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sac, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{sac: scamCode + ":" + scamIssuer})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	gate := &stubTVLGate{withhold: map[string]bool{classic.String(): true}}
	v := newTVLValuer(
		stubTVLPricer{rates: map[string]string{sac: "3", "native": "2"}},
		nil, gate, time.Now())

	if _, ok := v.legUSD(context.Background(), sac, big.NewInt(10_000_000)); ok {
		t.Error("a SAC-wrapped flagged classic must be withheld — the scam directory " +
			"is keyed by issuer, so the gate has to see the classic identity")
	}
	if _, ok := v.legUSD(context.Background(), canonical.XLMSacContractID, big.NewInt(10_000_000)); !ok {
		t.Error("native XLM's SAC must still price")
	}
	want := []string{classic.String(), "native"}
	if len(gate.asked) != len(want) {
		t.Fatalf("gate was asked about %v, want the canonical identities %v", gate.asked, want)
	}
	for i, w := range want {
		if gate.asked[i] != w {
			t.Errorf("gate.asked[%d] = %q, want %q", i, gate.asked[i], w)
		}
	}
}

// TestTVLValuer_GateBeatsTheDeclaredPeg pins ordering: the gate must run before the peg shortcut, or declared pegs go
// unscreened. Matches the asset detail path (suppressScamIssuerPricing after fillDeclaredPegPrice).
func TestTVLValuer_GateBeatsTheDeclaredPeg(t *testing.T) {
	const pegged = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	gate := &stubTVLGate{withhold: map[string]bool{}}
	v := newTVLValuer(nil, stubTVLPegInfo{pegged: map[string]int{pegged: 7}}, gate, time.Now())

	usd, ok := v.legUSD(context.Background(), pegged, big.NewInt(10_000_000))
	if !ok || usd.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("ungated declared peg = %v ok=%v, want exactly $1", usd, ok)
	}

	gate2 := &stubTVLGate{withhold: map[string]bool{pegged: true}}
	v2 := newTVLValuer(nil, stubTVLPegInfo{pegged: map[string]int{pegged: 7}}, gate2, time.Now())
	if _, ok := v2.legUSD(context.Background(), pegged, big.NewInt(10_000_000)); ok {
		t.Error("a withheld asset must not be re-valued through the declared USD peg")
	}
}

// TestTVLValuer_GateAskedOncePerToken pins the memo: the refresh walks
// hundreds of pool legs and the gates each do a cached DB lookup, so a
// per-LEG call would multiply the directory/substance read fan-out.
func TestTVLValuer_GateAskedOncePerToken(t *testing.T) {
	gate := &stubTVLGate{withhold: map[string]bool{}}
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{"native": "1"}}, nil, gate, time.Now())
	for range 5 {
		if _, ok := v.legUSD(context.Background(), canonical.XLMSacContractID, big.NewInt(1)); !ok {
			t.Fatal("leg should price")
		}
	}
	if len(gate.asked) != 1 {
		t.Errorf("gate consulted %d times for one token, want 1 (%v)", len(gate.asked), gate.asked)
	}
}

func TestTVLValuer_LegEdgeCases(t *testing.T) {
	v := newTVLValuer(stubTVLPricer{rates: map[string]string{"native": "2"}}, nil, nil, time.Now())
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
