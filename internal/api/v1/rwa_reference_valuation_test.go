package v1_test

// GET /v1/rwa/assets — the reference-priced (NAV-style) valuation.
//
// Real-world assets are bought and held, not traded. Measured on the
// production deployment, four of the six members of the set carry an
// independent oracle valuation of their instrument and only two have
// ever produced a market price this platform will publish — so a
// surface that values the set at observed market prices alone reports
// nothing at all for half of it, while an oracle prices the underlying
// instrument daily.
//
// The fixture below is that shape:
//
//	CETES     market price + reference   both bases
//	TESOURO   market price + reference   both bases
//	USTRY     reference only             reference basis only
//	USDY      reference only             reference basis only
//	XAU       neither                    unvalued on both bases
//	AUMTL     neither                    unvalued on both bases
//
// What every test here is ultimately protecting is that the second
// figure cannot move the first one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// newRat, addDecimal and containsFold keep the assertions below on
// exact rational arithmetic and on case-insensitive prose matching,
// rather than parsing served money into a float to check it.
func newRat() *big.Rat { return new(big.Rat) }

func addDecimal(t *testing.T, sum *big.Rat, s string) {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("served money %q is not a decimal string", s)
	}
	sum.Add(sum, r)
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// sumDecimals adds served 2-dp money strings (nil entries skipped) with
// exact rational arithmetic and renders the sum at 2 dp.
func sumDecimals(t *testing.T, vals []*string) string {
	t.Helper()
	sum := newRat()
	for _, v := range vals {
		if v != nil {
			addDecimal(t, sum, *v)
		}
	}
	return sum.FloatString(2)
}

// dropsByReason indexes a funnel stage's drops by their reason.
func dropsByReason(st v1.RWAFunnelStage) map[string]v1.RWAFunnelDrop {
	out := map[string]v1.RWAFunnelDrop{}
	for _, d := range st.Dropped {
		out[d.Reason] = d
	}
	return out
}

// assertNoReferenceValue checks that a refused row carries the stated
// refusal and no figure, no reference block, and reaches no reference total.
func assertNoReferenceValue(t *testing.T, v v1.RWAAssetsView, a v1.RWAAsset, wantStatus, what string) {
	t.Helper()
	if a.ReferenceValuation.Status != wantStatus {
		t.Errorf("reference_valuation.status = %q, want %q", a.ReferenceValuation.Status, wantStatus)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("%s was valued: %s", what, *a.ReferenceValuation.ValueUSD)
	}
	if a.Reference != nil {
		t.Errorf("a reference block reached %s: %+v", what, a.Reference)
	}
	if v.Summary.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a withheld row reached the reference total: %s", *v.Summary.ReferenceValuation.ValueUSD)
	}
}

// rwaOndoIssuer is the account internal/rwa binds the USDY instrument
// to — a different issuer from the Etherfuse account behind CETES,
// USTRY and TESOURO, so the fixture exercises the per-issuer breakdown
// on both bases rather than collapsing to one row.
const rwaOndoIssuer = "GAJMPX5NBOG6TQFPQGRABJEEB2YE7RFRLUKJDZAZGAD5GFX4J7TADAZ6"

// Supplies in the smallest on-chain unit. Classic assets are 7dp, so
// each is 10^7 times the whole-token float named beside it.
const (
	rwaCETESSupply   = "476000000000000" // 47,600,000 tokens
	rwaTESOUROSupply = "23260000000000"  // 2,326,000 tokens
	rwaUSTRYSupply   = "12336218000000"  // 1,233,621.8 tokens
	rwaUSDYSupply    = "8500000000000"   // 850,000 tokens
	rwaXAUSupply     = "1200000000"      // 120 tokens
	rwaAUMTLSupply   = "45000000000"     // 4,500 tokens
)

// rwaProductionShapedServer builds the six-asset fixture above.
//
// withOracle: false wires no oracle at all, which is how the market
// basis is measured in isolation — the reference snapshot is then
// unavailable and no row can carry a reference valuation.
func rwaProductionShapedServer(t *testing.T, withOracle bool) *v1.Server {
	t.Helper()
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("CETES", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("TESOURO", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("XAU", rwaGoodIssuer, "etherfuse.com", "commodity"),
		rwaBound("AUMTL", rwaGoodIssuer, "etherfuse.com", "commodity"),
		rwaBound("USDY", rwaOndoIssuer, "ondo.finance", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaOndoIssuer: recognisedIssuer(rwaOndoIssuer, "Ondo"),
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("CETES", rwaGoodIssuer, sptr("0.069000"), 412009),
			rwaRow("TESOURO", rwaGoodIssuer, sptr("0.245000"), 13318),
			// Bought and held: no market has produced a price this
			// platform will publish for these.
			rwaRow("USTRY", rwaGoodIssuer, nil, 346312),
			rwaRow("XAU", rwaGoodIssuer, nil, 1436),
			rwaRow("AUMTL", rwaGoodIssuer, nil, 902),
		},
		rwaOndoIssuer: {rwaRow("USDY", rwaOndoIssuer, nil, 8299)},
	}
	supply := map[string]string{
		"CETES-" + rwaGoodIssuer:   rwaCETESSupply,
		"TESOURO-" + rwaGoodIssuer: rwaTESOUROSupply,
		"USTRY-" + rwaGoodIssuer:   rwaUSTRYSupply,
		"XAU-" + rwaGoodIssuer:     rwaXAUSupply,
		"AUMTL-" + rwaGoodIssuer:   rwaAUMTLSupply,
		"USDY-" + rwaOndoIssuer:    rwaUSDYSupply,
	}
	opts := v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              supply,
		},
	}
	if withOracle {
		// The four bound instruments, at the values measured on the
		// production stream — plus rwa:XAU, which the stream really
		// does carry and which nothing may be bound to.
		//
		// XAU is in the fixture precisely BECAUSE the feed exists. It
		// prices a troy ounce of spot metal, so a join on the code
		// would hand a token called XAU $4,115.67 a unit and value 120
		// of them at nearly half a million dollars. The feed being
		// absent would make that failure untestable; the feed being
		// present makes the refusal load-bearing. AUMTL is the other
		// case: no oracle prices an instrument of that name at all.
		opts.Oracle = rwaOracle(t,
			rwaOracleRow(t, "redstone", "rwa:CETES", "fiat:USD", "69485", 6),
			rwaOracleRow(t, "redstone", "rwa:TESOURO", "fiat:USD", "245350", 6),
			rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "1074182", 6),
			rwaOracleRow(t, "redstone", "rwa:USDY", "fiat:USD", "1145745", 6),
			rwaOracleRow(t, "redstone", "rwa:XAU", "fiat:USD", "4115670000", 6),
		)
	}
	return v1.New(opts)
}

func rwaByCode(t *testing.T, v v1.RWAAssetsView) map[string]v1.RWAAsset {
	t.Helper()
	out := map[string]v1.RWAAsset{}
	for _, a := range v.Assets {
		out[a.Code] = a
	}
	return out
}

func optString(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}

// renderValuation flattens a market-basis valuation to a comparable
// string. The struct holds POINTERS to money, so comparing two of them
// directly compares addresses and passes whatever the figures say.
func renderValuation(v v1.RWAValuation) string {
	return strings.Join([]string{
		v.Status, v.PriceBasis, optString(v.PriceUSD), optString(v.MarketCapUSD),
	}, "|")
}

// TestRWAAssets_MarketCapIsUnchangedByTheReferenceBasis is the property
// most worth protecting, and the one this whole change is measured
// against.
//
// `market_cap_usd` means one thing: circulating supply times a price
// somebody was observed paying, admitted only after the thin-market
// substance gate, the dust-liquidity guard and the scam-issuer
// suppression have each declined to withhold it. A consumer relying on
// that figure must keep getting the same number, whatever the oracle
// stream happens to carry.
//
// So the SAME six-asset fixture is served twice — once with the oracle
// wired and once with no oracle at all — and every field on the market
// basis is compared across the two. The second basis is fully populated
// in one run and entirely absent in the other, which is precisely the
// perturbation that would expose a fold: sum the two totals together,
// let a reference valuation fill in for a withheld market cap, or let a
// reference-valued row count as valued, and the two runs diverge here.
func TestRWAAssets_MarketCapIsUnchangedByTheReferenceBasis(t *testing.T) {
	withOracle := getRWA(t, rwaProductionShapedServer(t, true))
	without := getRWA(t, rwaProductionShapedServer(t, false))

	// Guard the guard: the perturbation must actually have happened, or
	// this test compares two identical runs and proves nothing.
	if withOracle.Summary.ReferenceValuation.AssetsValued != 4 {
		t.Fatalf("the oracle-wired run reference-valued %d assets, want 4 — the comparison below would be vacuous",
			withOracle.Summary.ReferenceValuation.AssetsValued)
	}
	if without.Summary.ReferenceValuation.AssetsValued != 0 ||
		without.Summary.ReferenceValuation.ValueUSD != nil {
		t.Fatalf("the oracle-less run published a reference total: %+v", without.Summary.ReferenceValuation)
	}

	if got, want := optString(withOracle.Summary.MarketCapUSD), optString(without.Summary.MarketCapUSD); got != want {
		t.Errorf("summary.market_cap_usd = %s with the reference basis, %s without — the two bases were folded", got, want)
	}
	// The exact figure, pinned. 47,600,000 x 0.069000 plus 2,326,000 x
	// 0.245000, and NOTHING else: the four reference-valued rows
	// contribute nothing to it.
	if got := optString(withOracle.Summary.MarketCapUSD); got != "3854270.00" {
		t.Errorf("summary.market_cap_usd = %s, want 3854270.00", got)
	}
	if withOracle.Summary.AssetsValued != 2 || withOracle.Summary.AssetsUnvalued != 4 {
		t.Errorf("summary valued/unvalued = %d/%d, want 2/4 — a reference valuation is not a market valuation",
			withOracle.Summary.AssetsValued, withOracle.Summary.AssetsUnvalued)
	}
	if !withOracle.Summary.LowerBound {
		t.Error("summary.lower_bound = false — four members publish no market cap, so the total is a lower bound")
	}

	byCodeA, byCodeB := rwaByCode(t, withOracle), rwaByCode(t, without)
	if len(byCodeA) != 6 || len(byCodeB) != 6 {
		t.Fatalf("membership changed with the oracle: %d vs %d rows", len(byCodeA), len(byCodeB))
	}
	for code, a := range byCodeA {
		b := byCodeB[code]
		if got, want := renderValuation(a.Valuation), renderValuation(b.Valuation); got != want {
			t.Errorf("%s: valuation = %s with the reference basis, %s without", code, got, want)
		}
		if a.Valuation.Status == v1.RWAValuationPublished && a.Valuation.MarketCapUSD == nil {
			t.Errorf("%s: published valuation with no market cap", code)
		}
	}
	// The per-row market caps, pinned the same way.
	for code, want := range map[string]string{
		"CETES":   "3284400.00",
		"TESOURO": "569870.00",
	} {
		if got := optString(byCodeA[code].Valuation.MarketCapUSD); got != want {
			t.Errorf("%s market_cap_usd = %s, want %s", code, got, want)
		}
	}
	for _, code := range []string{"USTRY", "USDY", "XAU", "AUMTL"} {
		if byCodeA[code].Valuation.MarketCapUSD != nil {
			t.Errorf("%s published a market cap from a reference price: %s",
				code, *byCodeA[code].Valuation.MarketCapUSD)
		}
	}

	// The breakdowns carry the same market basis, and the reference basis
	// did not move a row between groups either.
	breakdowns := func(v v1.RWAAssetsView) (classes, issuers []string) {
		for _, g := range v.ByClass {
			classes = append(classes, strings.Join([]string{
				g.Class, optString(g.MarketCapUSD), strconv.Itoa(g.Assets), strconv.Itoa(g.AssetsUnvalued),
			}, "|"))
		}
		for _, g := range v.ByIssuer {
			issuers = append(issuers, strings.Join([]string{
				g.Issuer, optString(g.MarketCapUSD), strconv.Itoa(g.Assets), strconv.Itoa(g.AssetsUnvalued),
			}, "|"))
		}
		return classes, issuers
	}
	classesA, issuersA := breakdowns(withOracle)
	classesB, issuersB := breakdowns(without)
	if got, want := strings.Join(classesA, "\n"), strings.Join(classesB, "\n"); got != want {
		t.Errorf("by_class market basis moved:\n%s\nvs\n%s", got, want)
	}
	if got, want := strings.Join(issuersA, "\n"), strings.Join(issuersB, "\n"); got != want {
		t.Errorf("by_issuer market basis moved:\n%s\nvs\n%s", got, want)
	}
}

// TestRWAAssets_ReferenceValuationCoversTheAssetsNobodyTrades is the
// point of the second basis.
//
// USTRY and USDY have no market price, so they contribute nothing to
// `market_cap_usd` and are counted as unvalued there — correctly, since
// nobody was observed paying anything for them. Both nevertheless have
// a bound, current, dollar-denominated oracle valuation of the
// instrument they anchor to, and a circulating supply on chain. The
// reference basis values them; the market basis still does not.
func TestRWAAssets_ReferenceValuationCoversTheAssetsNobodyTrades(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))
	byCode := rwaByCode(t, v)

	for code, want := range map[string]string{
		// 47,600,000 x 0.069485
		"CETES": "3307486.00",
		// 2,326,000 x 0.245350
		"TESOURO": "570684.10",
		// 1,233,621.8 x 1.074182
		"USTRY": "1325134.33",
		// 850,000 x 1.145745
		"USDY": "973883.25",
	} {
		a := byCode[code]
		if a.ReferenceValuation.Status != v1.RWAReferenceValuationPublished {
			t.Errorf("%s: reference_valuation.status = %q, want published", code, a.ReferenceValuation.Status)
			continue
		}
		if got := optString(a.ReferenceValuation.ValueUSD); got != want {
			t.Errorf("%s: reference_valuation.value_usd = %s, want %s", code, got, want)
		}
	}
	// The two the market never priced now carry a figure, and still
	// carry no market cap. Both statements have to hold at once.
	for _, code := range []string{"USTRY", "USDY"} {
		a := byCode[code]
		if a.Valuation.Status != v1.RWAValuationUnpriced || a.Valuation.MarketCapUSD != nil {
			t.Errorf("%s: valuation = %+v, want unpriced with no market cap", code, a.Valuation)
		}
		if a.ReferenceValuation.ValueUSD == nil {
			t.Errorf("%s: an asset with a reference price and a supply contributed nothing", code)
		}
	}

	// 3,307,486.00 + 570,684.10 + 1,325,134.33 + 973,883.25.
	if got := optString(v.Summary.ReferenceValuation.ValueUSD); got != "6177187.68" {
		t.Errorf("summary.reference_valuation.value_usd = %s, want 6177187.68", got)
	}
	if v.Summary.ReferenceValuation.AssetsValued != 4 || v.Summary.ReferenceValuation.AssetsUnvalued != 2 {
		t.Errorf("reference valued/unvalued = %d/%d, want 4/2",
			v.Summary.ReferenceValuation.AssetsValued, v.Summary.ReferenceValuation.AssetsUnvalued)
	}
	if !v.Summary.ReferenceValuation.LowerBound {
		t.Error("reference lower_bound = false, but two members carry no reference valuation")
	}
}

// TestRWAAssets_ReferenceTotalIsTheSumOfTheRows — add up the column and
// you land on the published total, exactly, with no rounding drift
// between the levels. Same property `market_cap_usd` has, and the
// reason both sum already-rounded 2-dp strings with exact rational
// arithmetic rather than re-deriving from prices.
func TestRWAAssets_ReferenceTotalIsTheSumOfTheRows(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))

	var rowVals []*string
	for _, a := range v.Assets {
		if a.ReferenceValuation.ValueUSD != nil {
			rowVals = append(rowVals, a.ReferenceValuation.ValueUSD)
		}
	}
	if len(rowVals) != v.Summary.ReferenceValuation.AssetsValued {
		t.Errorf("%d rows carry a figure but assets_valued = %d", len(rowVals), v.Summary.ReferenceValuation.AssetsValued)
	}
	total := optString(v.Summary.ReferenceValuation.ValueUSD)
	if got := sumDecimals(t, rowVals); got != total {
		t.Errorf("rows sum to %s but the total says %s", got, total)
	}

	// The per-issuer and per-class breakdowns are the same sum, split.
	var issuerVals, classVals []*string
	for _, i := range v.ByIssuer {
		issuerVals = append(issuerVals, i.ReferenceValueUSD)
	}
	for _, c := range v.ByClass {
		classVals = append(classVals, c.ReferenceValueUSD)
	}
	if got := sumDecimals(t, issuerVals); got != total {
		t.Errorf("by_issuer reference totals sum to %s but the summary says %s", got, total)
	}
	if got := sumDecimals(t, classVals); got != total {
		t.Errorf("by_class reference totals sum to %s but the summary says %s", got, total)
	}
}

// TestRWAAssets_UnreferencedAssetsStayUnvaluedOnBothBases — XAU and
// AUMTL have no reference at all, and no fallback may be invented for
// them.
//
// They fail for DIFFERENT reasons and both reasons are reported. An
// oracle does publish a feed called XAU, but it prices a troy ounce of
// spot metal: its ratio to a token price is a unit conversion wearing a
// premium's clothes, and its product with a token supply is not a
// valuation of anything. Nothing prices an instrument called AUMTL at
// all. Neither may be answered with a nearby number.
func TestRWAAssets_UnreferencedAssetsStayUnvaluedOnBothBases(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))
	byCode := rwaByCode(t, v)

	for code, wantReason := range map[string]string{
		"XAU":   v1.RWAPremiumNotInstrumentScoped,
		"AUMTL": v1.RWAPremiumNotBound,
	} {
		a := byCode[code]
		if a.Reference != nil {
			t.Errorf("%s: a reference was attached: %+v", code, a.Reference)
		}
		// One refusal, reported identically in both places. XAU and
		// AUMTL fail for DIFFERENT reasons and each says which: a
		// shared placeholder would collapse them.
		if a.Premium.Status != wantReason {
			t.Errorf("%s: premium.status = %q, want %q", code, a.Premium.Status, wantReason)
		}
		if a.ReferenceValuation.Status != wantReason {
			t.Errorf("%s: reference_valuation.status = %q, want %q",
				code, a.ReferenceValuation.Status, wantReason)
		}
		if a.ReferenceValuation.ValueUSD != nil {
			t.Errorf("%s: a valuation was invented for an asset nothing prices: %s",
				code, *a.ReferenceValuation.ValueUSD)
		}
		// A supply IS served for them — it is a chain fact, not a price
		// claim — which is exactly why the absent valuation has to be an
		// absence rather than supply x some default.
		if a.CirculatingSupply == nil {
			t.Errorf("%s: circulating_supply is absent; it is a raw chain fact and is served regardless", code)
		}
	}
	if v.Summary.ReferenceValuation.AssetsUnvalued != 2 {
		t.Errorf("reference assets_unvalued = %d, want 2", v.Summary.ReferenceValuation.AssetsUnvalued)
	}
}

// TestRWAAssets_NoReferenceValuationOmitsTheTotal — with no oracle
// wired nothing is reference-valued, and the total is ABSENT rather
// than "0.00". A zero there asserts that the backing behind every
// tokenized treasury in the set is worth nothing, which is the one
// reading certain to be wrong.
func TestRWAAssets_NoReferenceValuationOmitsTheTotal(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, false))

	if v.Summary.ReferenceValuation.ValueUSD != nil {
		t.Errorf("summary.reference_valuation.value_usd = %q, want absent",
			*v.Summary.ReferenceValuation.ValueUSD)
	}
	if v.Summary.ReferenceValuation.AssetsValued != 0 || !v.Summary.ReferenceValuation.LowerBound {
		t.Errorf("reference valued/lower_bound = %d/%v, want 0/true",
			v.Summary.ReferenceValuation.AssetsValued, v.Summary.ReferenceValuation.LowerBound)
	}
	if v.Summary.ReferenceValuation.Basis == "" {
		t.Error("no basis served for the reference figure")
	}
	for _, g := range v.ByClass {
		if g.ReferenceValueUSD != nil {
			t.Errorf("by_class[%s].reference_value_usd = %q, want absent", g.Class, *g.ReferenceValueUSD)
		}
	}
	for _, i := range v.ByIssuer {
		if i.ReferenceValueUSD != nil {
			t.Errorf("by_issuer[%s].reference_value_usd = %q, want absent", i.Issuer, *i.ReferenceValueUSD)
		}
	}
	if v.Summary.BothBases.Assets != 0 ||
		v.Summary.BothBases.MarketCapUSD != nil || v.Summary.BothBases.ReferenceValueUSD != nil {
		t.Errorf("both_bases = %+v, want empty when nothing is reference-valued", v.Summary.BothBases)
	}
}

// TestRWAAssets_BothBasesTotalsOnlyTheOverlap — the two totals are sums
// over DIFFERENT rows, so subtracting one from the other measures
// nothing. `both_bases` restricts each to the members carrying both, so
// the difference between those two figures is the aggregate gap between
// what the market pays and what the reference says it is worth.
func TestRWAAssets_BothBasesTotalsOnlyTheOverlap(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))

	if v.Summary.BothBases.Assets != 2 {
		t.Fatalf("both_bases.assets = %d, want 2 (CETES and TESOURO)", v.Summary.BothBases.Assets)
	}
	// The market total over the overlap is the whole market total here,
	// because only those two rows carry a market cap at all.
	if got := optString(v.Summary.BothBases.MarketCapUSD); got != "3854270.00" {
		t.Errorf("both_bases.market_cap_usd = %s, want 3854270.00", got)
	}
	// 3,307,486.00 + 570,684.10 — the reference basis over the SAME two
	// rows, which is 2,299,017.58 less than the reference total over
	// the whole set.
	if got := optString(v.Summary.BothBases.ReferenceValueUSD); got != "3878170.10" {
		t.Errorf("both_bases.reference_value_usd = %s, want 3878170.10", got)
	}
	if optString(v.Summary.ReferenceValuation.ValueUSD) == optString(v.Summary.BothBases.ReferenceValueUSD) {
		t.Error("the set-wide reference total and the overlap total are equal; the fixture no longer distinguishes them")
	}
	// And the per-unit form of the same gap is already on each row.
	byCode := rwaByCode(t, v)
	for _, code := range []string{"CETES", "TESOURO"} {
		if byCode[code].Premium.Status != v1.RWAPremiumPublished || byCode[code].Premium.Pct == nil {
			t.Errorf("%s: premium = %+v, want a published percentage beside the two figures",
				code, byCode[code].Premium)
		}
	}
	if v.Summary.AssetsCompared != 2 {
		t.Errorf("summary.assets_compared = %d, want 2", v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_EveryReferenceValuationCarriesItsProvenance — a dollar
// figure with no traceable source is worse than an absent one here,
// because it cannot be checked and cannot be challenged. Every row
// carrying a reference valuation must name the feed, the publisher, the
// denominator and the vintage behind it, and the summary must name the
// oracles the total is made of.
func TestRWAAssets_EveryReferenceValuationCarriesItsProvenance(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))

	valued := 0
	for _, a := range v.Assets {
		if a.ReferenceValuation.ValueUSD == nil {
			continue
		}
		valued++
		if a.Reference == nil {
			t.Errorf("%s: a reference valuation with no reference block — the figure has no source", a.Code)
			continue
		}
		if a.Reference.Source == "" || a.Reference.Feed == "" {
			t.Errorf("%s: reference names no publisher or feed: %+v", a.Code, a.Reference)
		}
		if a.Reference.Quote != "fiat:USD" {
			t.Errorf("%s: reference quote = %q — a total of non-dollar values is not dollars",
				a.Code, a.Reference.Quote)
		}
		if time.Time(a.Reference.AsOf).IsZero() {
			t.Errorf("%s: reference carries no vintage", a.Code)
		}
		// The reference price and the row's own decimals are both on the
		// wire, so the figure can be re-derived by hand from the
		// published supply.
		if a.Decimals == nil || *a.Decimals != 7 {
			t.Errorf("%s: decimals = %s, want 7 for a classic asset", a.Code, decimalsText(a.Decimals))
		}
		if a.CirculatingSupply == nil {
			t.Errorf("%s: a valuation was published with no supply on the wire to back it", a.Code)
		}
		// The feed is one of the audited bindings, served with the rows.
		if _, err := canonical.ParseAsset(a.Reference.Feed); err != nil {
			t.Errorf("%s: reference feed %q is not a canonical asset id", a.Code, a.Reference.Feed)
		}
	}
	if valued != 4 {
		t.Fatalf("%d rows carry a reference valuation, want 4", valued)
	}
	if got := v.Summary.ReferenceValuation.Sources; len(got) != 1 || got[0] != "redstone" {
		t.Errorf("summary.reference_valuation.sources = %v, want [redstone]", got)
	}
	// Every bound pair behind a served figure is auditable from the
	// definition the response carries.
	bindings := map[string]string{}
	for _, b := range v.Definition.BoundInstruments {
		bindings[b.Code+"-"+b.Issuer] = b.Feed
	}
	for _, a := range v.Assets {
		if a.ReferenceValuation.ValueUSD == nil {
			continue
		}
		if bindings[a.Code+"-"+a.Issuer] != a.Reference.Feed {
			t.Errorf("%s: the binding behind the served figure is not in definition.bound_instruments", a.Code)
		}
	}
}

// TestRWAAssets_ReferenceBasisNamesItselfAsAClaim — the field name is
// only one of three places the distinction is carried, and prose is the
// one a careless reader actually reads. The basis must say, in the
// response itself, that nobody was observed paying this, and the
// market-cap basis must say the reference figure is not in its total.
//
// The "nobody was seen paying it" claim is asserted here against an
// ORACLE-priced total, which is the only kind it is true of. A
// listing-priced row IS a price somebody paid, on venues this index does
// not gate, and TestRWAAssets_ListingBasisDoesNotInheritTheOracleWording
// pins that the prose says so instead of inheriting this sentence.
func TestRWAAssets_ReferenceBasisNamesItselfAsAClaim(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))

	basis := v.Summary.ReferenceValuation.Basis
	for _, want := range []string{
		"NOT A MARKET CAPITALISATION",
		"nobody was seen paying it",
		"never added to it",
	} {
		if !containsFold(basis, want) {
			t.Errorf("reference basis does not state %q: %s", want, basis)
		}
	}
	if !containsFold(v.Summary.Basis, "not in this total") {
		t.Errorf("the market-cap basis does not say the reference figure is excluded: %s", v.Summary.Basis)
	}
}

// TestRWAAssets_FlaggedIssuerGetsNoReferenceValuationEither — a
// scam-class directory tag withholds every valuation, a third party's
// included. The per-unit case is already covered; this is the
// supply-multiplied form, which is the LARGER claim: a real
// instrument's net asset value times an impersonator's own float, from
// an oracle that never named the impersonator.
func TestRWAAssets_FlaggedIssuerGetsNoReferenceValuationEither(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &rwaSkewedDirectory{
			membership: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer"}, Source: "stellar-expert",
			},
			rowFill: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
			},
		},
		Oracle: rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "1074182", 6)),
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
			},
			supply: map[string]string{"USTRY-" + rwaGoodIssuer: rwaUSTRYSupply},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	assertNoReferenceValue(t, v, v.Assets[0], v1.RWAPremiumIssuerFlagged,
		"a flagged issuer (a real instrument's valuation times its own float)")
}

// rwaValuationArm returns the funnel's valuation stages, and fails if
// the arm is absent — every published response must account for the
// reference basis, and a missing arm is the failure mode this test
// series exists to catch.
func rwaValuationArm(t *testing.T, v v1.RWAAssetsView) []v1.RWAFunnelStage {
	t.Helper()
	out := make([]v1.RWAFunnelStage, 0, 2)
	for _, st := range v.Funnel.Stages {
		if st.Arm == "valuation" {
			out = append(out, st)
		}
	}
	if len(out) != 2 {
		t.Fatalf("funnel carries %d valuation stages, want 2: %+v", len(out), v.Funnel.Stages)
	}
	return out
}

// TestRWAAssets_FunnelAccountsForEveryUnvaluedRow is the integration
// the funnel demands: an accounting that stops at "served" says nothing
// about whether an admitted asset carries a reference valuation, and a
// reader would have to filter the rows by hand to find out.
//
// The arm continues past the served set and counts every row that
// carries no figure under the reason that refused it, on the same
// three-actor vocabulary the membership arms use — so a coverage gap
// somebody can close reads differently from the definition working.
func TestRWAAssets_FunnelAccountsForEveryUnvaluedRow(t *testing.T) {
	v := getRWA(t, rwaProductionShapedServer(t, true))
	stages := rwaValuationArm(t, v)

	if stages[0].Stage != "assets_served_all_arms" || stages[0].Count != 6 {
		t.Errorf("first valuation stage = %s/%d, want assets_served_all_arms/6", stages[0].Stage, stages[0].Count)
	}
	if stages[1].Stage != "assets_reference_valued" || stages[1].Count != 4 {
		t.Errorf("second valuation stage = %s/%d, want assets_reference_valued/4", stages[1].Stage, stages[1].Count)
	}
	if stages[0].Unit != "assets" || stages[1].Unit != "assets" {
		t.Errorf("valuation stages count %q then %q, want assets throughout", stages[0].Unit, stages[1].Unit)
	}

	drops := dropsByReason(stages[0])
	// XAU and AUMTL fail for different reasons and the funnel says
	// which, rather than collapsing both into one bucket.
	for reason, want := range map[string]struct {
		count int
		actor string
	}{
		v1.RWAPremiumNotInstrumentScoped: {1, "definition"},
		v1.RWAPremiumNotBound:            {1, "definition"},
	} {
		got, ok := drops[reason]
		if !ok {
			t.Errorf("no funnel drop for %q: %+v", reason, stages[0].Dropped)
			continue
		}
		if got.Count != want.count {
			t.Errorf("drop %q count = %d, want %d", reason, got.Count, want.count)
		}
		if got.Actor != want.actor {
			t.Errorf("drop %q actor = %q, want %q — an actor tells a reader who can move the number",
				reason, got.Actor, want.actor)
		}
	}

	// THE COHERENCE PROPERTY. The funnel's drop reasons are the strings
	// the rows carry, and the counts are a tally of them. Two views of
	// one event, never two events.
	fromRows := map[string]int{}
	for _, a := range v.Assets {
		if a.ReferenceValuation.Status == v1.RWAReferenceValuationPublished {
			continue
		}
		fromRows[a.ReferenceValuation.Status]++
	}
	if len(fromRows) != len(drops) {
		t.Errorf("rows carry %d distinct refusal reasons but the funnel reports %d", len(fromRows), len(drops))
	}
	for reason, n := range fromRows {
		if drops[reason].Count != n {
			t.Errorf("%d rows say %q but the funnel drops %d under it", n, reason, drops[reason].Count)
		}
	}
	if !v.Funnel.Balanced {
		t.Errorf("the funnel does not balance with the valuation arm attached: %+v", v.Funnel.Stages)
	}
	if !containsFold(v.Funnel.Basis, "continues PAST the served set") {
		t.Errorf("the funnel basis does not say the valuation arm is not a membership narrowing: %s", v.Funnel.Basis)
	}
}

// TestRWAAssets_FunnelValuationArmDoesNotAdmitOrRefuse guards the
// boundary the funnel's own charter draws. The valuation arm accounts
// for a FIGURE; it must not change which assets are in the set, and the
// membership stages must read the same with it attached.
func TestRWAAssets_FunnelValuationArmDoesNotAdmitOrRefuse(t *testing.T) {
	withOracle := getRWA(t, rwaProductionShapedServer(t, true))
	without := getRWA(t, rwaProductionShapedServer(t, false))

	membership := func(v v1.RWAAssetsView) []string {
		out := []string{}
		for _, st := range v.Funnel.Stages {
			if st.Arm == "valuation" {
				continue
			}
			row := st.Arm + "/" + st.Stage + "/" + st.Unit + "/" + strconv.Itoa(st.Count)
			for _, d := range st.Dropped {
				row += "|" + d.Reason + ":" + strconv.Itoa(d.Count) + ":" + d.Actor
			}
			out = append(out, row)
		}
		return out
	}
	a, b := membership(withOracle), membership(without)
	if len(a) == 0 {
		t.Fatal("no membership stages to compare — the funnel lost its narrowing")
	}
	if len(a) != len(b) {
		t.Fatalf("membership stages = %d with the reference basis, %d without", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("membership stage %d moved with the reference basis:\n  %s\n  %s", i, a[i], b[i])
		}
	}
	// The comparison above perturbs both runs identically, so it cannot
	// see a change that affects them equally — a drop invented on a
	// membership stage, for instance. The arithmetic can: a drop across
	// the arm boundary is unreconcilable by construction, and the funnel
	// says so rather than absorbing it.
	for name, v := range map[string]v1.RWAAssetsView{"with the oracle": withOracle, "without": without} {
		if !v.Funnel.Balanced {
			t.Errorf("%s: the funnel does not balance — a stage gained a drop it cannot account for: %+v",
				name, v.Funnel.Stages)
		}
	}
	// With no oracle at all every oracle-bound row is dropped under the
	// outage, which is a different statement from a network whose
	// instruments nobody prices. A row no oracle is bound to never reads
	// the oracle, so its verdict must not move with the outage.
	stages := rwaValuationArm(t, without)
	if stages[1].Count != 0 {
		t.Errorf("reference-valued = %d with no oracle wired, want 0", stages[1].Count)
	}
	withDrops := dropsByReason(rwaValuationArm(t, withOracle)[0])
	outage := 0
	for _, d := range stages[0].Dropped {
		if d.Reason == v1.RWAPremiumReferenceUnavailable {
			outage = d.Count
			if d.Actor != "operator" {
				t.Errorf("an outage was attributed to %q, not the operator who can fix it", d.Actor)
			}
			continue
		}
		if withDrops[d.Reason].Count != d.Count {
			t.Errorf("drop %q = %d without the oracle, %d with it: an unbound row's verdict moved with an oracle outage",
				d.Reason, d.Count, withDrops[d.Reason].Count)
		}
	}
	if outage == 0 {
		t.Errorf("drops = %+v, want the oracle-bound rows under %q", stages[0].Dropped, v1.RWAPremiumReferenceUnavailable)
	}
}

// rwaContractServerWithOracle wires the contract arm fully AND an
// oracle stream, so the contract refusal is measured with a matching
// feed available rather than in its absence.
func rwaContractServerWithOracle(
	t *testing.T, symbol, supply string, decimals uint32, oracle *rwaOracleStub,
) *v1.Server {
	t.Helper()
	dir := map[string]timescale.DirectoryEntry{
		rwaContractGood: recognisedContract(rwaContractGood, "Example Treasury Fund"),
	}
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts: &stubRWAContractReader{
			contracts: []timescale.DirectoryEntry{recognisedContract(rwaContractGood, "Example Treasury Fund")},
			accounts:  18000,
		},
		ContractCatalogue: &stubContractCatalogue{rows: map[string]timescale.AssetRow{
			rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000")),
		}},
		TokenSymbol:           &stubTokenSymbols{byID: map[string]string{rwaContractGood: symbol}},
		TokenSupply:           &stubTokenSupplies{byID: map[string]string{rwaContractGood: supply}},
		TokenDecimals:         &stubTokenDecimalsRdr{byID: map[string]uint32{rwaContractGood: decimals}},
		NonstandardDecimals:   confirmedNonstandardDecimals(t, map[string]uint32{rwaContractGood: decimals}),
		Oracle:                oracle,
		MinMarketCapVolumeUSD: 1000,
	})
}

// TestRWAAssets_ContractMemberStatesWhyItIsNotReferenceValued is the
// decision the contract arm forced, end to end.
//
// A contract-issued member reaches this surface with everything the
// arithmetic needs: a supply from the certified lake and its own
// declared decimals. It is still not reference-valued, because nothing
// binds a contract ADDRESS to an oracle feed — the only available join
// is the symbol the contract itself declares, and pricing a token by a
// self-declared ticker is the join this whole surface refuses.
//
// What matters is that the refusal is STATED. A silently absent figure
// would be indistinguishable from an oversight, and a reader could not
// tell "we will not do this" from "we forgot".
func TestRWAAssets_ContractMemberStatesWhyItIsNotReferenceValued(t *testing.T) {
	// The oracle publishes rwa:USTRY, and the contract's on-chain
	// symbol is USTRY. The refusal has to hold with the tempting join
	// sitting right there, or it proves nothing.
	v := getRWA(t, rwaContractServerWithOracle(t, "USTRY", "8500000000000", 6,
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "1074182", 6))))

	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want the one admitted contract", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.ContractID == "" {
		t.Fatalf("the served row is not contract-issued: %+v", a)
	}
	assertNoReferenceValue(t, v, a, v1.RWAPremiumContractNotBound,
		"a contract token (through its own declared symbol)")

	// The market basis is untouched by any of that: the row still
	// carries the market cap the pipeline computed for it, at the
	// contract's REAL decimals.
	if a.Decimals == nil || *a.Decimals != 6 {
		t.Errorf("decimals = %s, want the contract's declared 6 — a 7 here is a tenth of the real figure", decimalsText(a.Decimals))
	}
	// 8,500,000,000,000 smallest units at 6dp is 8,500,000 tokens, at
	// 1.074 a share.
	if got := optString(a.Valuation.MarketCapUSD); got != "9129000.00" {
		t.Errorf("market_cap_usd = %s, want 9129000.00", got)
	}

	// And the funnel names the refusal, attributing it to the operator:
	// the curated contract-to-feed set is empty for want of primary
	// sources, and a reviewer with one can add an entry.
	stages := rwaValuationArm(t, v)
	if len(stages[0].Dropped) != 1 {
		t.Fatalf("valuation drops = %+v, want exactly the contract refusal", stages[0].Dropped)
	}
	d := stages[0].Dropped[0]
	if d.Reason != v1.RWAPremiumContractNotBound || d.Count != 1 || d.Actor != "operator" {
		t.Errorf("valuation drop = %+v, want %q/1/operator", d, v1.RWAPremiumContractNotBound)
	}
	if !v.Funnel.Balanced {
		t.Errorf("the funnel does not balance on the contract arm: %+v", v.Funnel.Stages)
	}
}

// TestRWAAssets_FunnelDistinguishesACoverageGapFromARefusal is what
// the `actor` field is FOR, and the six-asset production fixture cannot
// prove it: every refusal in that set is the definition working, so a
// funnel that hardcoded `definition` on every drop would look correct.
//
// This fixture holds one of each. USTRY is bound, fed and has a supply,
// so it is valued. CETES is bound and fed and has NO supply reading —
// a gap in this platform's own pipeline that an operator can close.
// XAU is refused because the feed of that name prices a troy ounce,
// which nobody can or should fix.
//
// A reader deciding whether to chase something needs those three told
// apart, and the difference is exactly one string per drop.
func TestRWAAssets_FunnelDistinguishesACoverageGapFromARefusal(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("CETES", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("XAU", rwaGoodIssuer, "etherfuse.com", "commodity"),
	}
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {
		rwaRow("USTRY", rwaGoodIssuer, nil, 346312),
		rwaRow("CETES", rwaGoodIssuer, nil, 412009),
		rwaRow("XAU", rwaGoodIssuer, nil, 1436),
	}}
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
			rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		}},
		// CETES and XAU are deliberately absent from the supply map.
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              map[string]string{"USTRY-" + rwaGoodIssuer: rwaUSTRYSupply},
		},
		Oracle: rwaOracle(t,
			rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "1074182", 6),
			rwaOracleRow(t, "redstone", "rwa:CETES", "fiat:USD", "69485", 6),
			rwaOracleRow(t, "redstone", "rwa:XAU", "fiat:USD", "4115670000", 6),
		),
	})
	v := getRWA(t, srv)
	byCode := rwaByCode(t, v)

	if byCode["USTRY"].ReferenceValuation.Status != v1.RWAReferenceValuationPublished {
		t.Fatalf("USTRY = %+v, want a published reference valuation", byCode["USTRY"].ReferenceValuation)
	}
	if byCode["CETES"].ReferenceValuation.Status != v1.RWAReferenceValuationNoSupply {
		t.Fatalf("CETES = %+v, want %q — the fixture no longer withholds its supply",
			byCode["CETES"].ReferenceValuation, v1.RWAReferenceValuationNoSupply)
	}
	// CETES still carries the reference itself: the missing input is
	// this platform's supply reading, not the oracle's price.
	if byCode["CETES"].Reference == nil {
		t.Error("CETES lost its reference along with its supply — two different absences")
	}

	stages := rwaValuationArm(t, v)
	drops := dropsByReason(stages[0])
	if got := drops[v1.RWAReferenceValuationNoSupply].Actor; got != "operator" {
		t.Errorf("a missing supply reading was attributed to %q, want operator — it is a gap here, not a refusal", got)
	}
	if got := drops[v1.RWAPremiumNotInstrumentScoped].Actor; got != "definition" {
		t.Errorf("an ounce-priced feed was attributed to %q, want definition — nobody can act on it", got)
	}
	if drops[v1.RWAReferenceValuationNoSupply].Actor == drops[v1.RWAPremiumNotInstrumentScoped].Actor {
		t.Error("a coverage gap and a refusal carry the same actor; the field distinguishes nothing")
	}
	if !v.Funnel.Balanced {
		t.Errorf("funnel unbalanced: %+v", v.Funnel.Stages)
	}
}

// TestRWAAssets_FunnelAccountsForTheWholePopulation is the headline
// regression. A response that serves one asset must state the size of
// the population it narrowed from and where the rest went — every
// stage, every drop, with the arithmetic closing.
func TestRWAAssets_FunnelAccountsForTheWholePopulation(t *testing.T) {
	// The shape of the production deployment in miniature: most issuer
	// accounts have never had their toml fetched, a few payloads will
	// not decode, a few declare nothing, and of the declarations that
	// exist the overwhelming majority name somebody else's account.
	// The fixture materialises only the three survivors, so the
	// pre-filter drop absorbs the rest of the bound population.
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain:      44376,
		IssuersWithPayload:         14635,
		IssuersPayloadUnreadable:   41,
		IssuersDeclaringNothing:    2109,
		IssuersDeclaring:           12485,
		Entries:                    1182000,
		EntriesMissingCode:         3140,
		EntriesMissingIssuer:       9612,
		EntriesNamingAnotherIssuer: 1145346,
		EntriesBound:               23902,
		EntriesFiltered:            23902,
	}
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
		},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer:    {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}

	v := getRWA(t, rwaServerWithUpstream(t, upstream, bound, dir, rows))
	if len(v.Funnel.Stages) == 0 {
		t.Fatal("no funnel served: the response narrows a population and must account for it")
	}
	checkFunnelArithmetic(t, v)

	st := rwaFunnelStages(t, v)
	// The population the surface narrows from, which the
	// response must state.
	if got := st["issuers_with_home_domain"].Count; got != 44376 {
		t.Errorf("issuers_with_home_domain = %d, want 44376", got)
	}
	if got := st["issuers_with_sep1_attestation"].Count; got != 14635 {
		t.Errorf("issuers_with_sep1_attestation = %d, want 14635", got)
	}
	// 44,376 - 14,635 = 29,741 issuer accounts whose toml has never
	// been fetched. This is the largest single coverage lever on the
	// surface and it belongs to an operator, not to the definition.
	if got := rwaDropCount(st, "issuers_with_home_domain", "sep1_attestation_never_fetched"); got != 29741 {
		t.Errorf("sep1_attestation_never_fetched = %d, want 29741", got)
	}
	for _, d := range st["issuers_with_home_domain"].Dropped {
		if d.Reason == "sep1_attestation_never_fetched" && d.Actor != "operator" {
			t.Errorf("never-fetched drop actor = %q, want operator — it is a fetch nobody ran, not a refusal", d.Actor)
		}
	}
	// The two silent stages.
	if got := rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_payload_unreadable"); got != 41 {
		t.Errorf("sep1_payload_unreadable = %d, want 41 — the swallowed-parse-error bucket", got)
	}
	if got := rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_declares_no_currencies"); got != 2109 {
		t.Errorf("sep1_declares_no_currencies = %d, want 2109", got)
	}
	// The provenance rule, which is the definition working and not a gap.
	if got := rwaDropCount(st, "sep1_currency_entries", "entry_declares_another_issuer"); got != 1145346 {
		t.Errorf("entry_declares_another_issuer = %d, want 1145346", got)
	}
	// Requirement 4's pre-filter, its own stage because it runs before
	// requirement 3 is evaluated for those entries.
	if got := rwaDropCount(st, "issuer_bound_entries", "no_real_world_instrument_basis"); got != 23902 {
		t.Errorf("pre-filter drop = %d, want 23902", got)
	}
	// Three candidates reached the ordered evaluation: one admitted,
	// one scam-flagged, one unrecognised.
	if got := st["candidate_assets_evaluated"].Count; got != 3 {
		t.Errorf("candidate_assets_evaluated = %d, want 3", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "issuer_scam_flagged"); got != 1 {
		t.Errorf("issuer_scam_flagged = %d, want 1", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "issuer_not_independently_recognised"); got != 1 {
		t.Errorf("issuer_not_independently_recognised = %d, want 1", got)
	}
	if got := st["assets_served"].Count; got != 1 || len(v.Assets) != 1 {
		t.Errorf("assets_served = %d with %d rows, want 1/1", got, len(v.Assets))
	}
}

// A payload past the attestation age bound is walked but not read. The
// funnel must name it, or the issuer stage stops closing and the stale
// population vanishes the way unreadable payloads once did.
func TestRWAAssets_FunnelCountsStaleAttestations(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain:      44376,
		IssuersWithPayload:         14635,
		IssuersPayloadStale:        500,
		IssuersPayloadUnreadable:   41,
		IssuersDeclaringNothing:    2109,
		IssuersDeclaring:           11985,
		Entries:                    1182000,
		EntriesMissingCode:         3140,
		EntriesMissingIssuer:       9612,
		EntriesNamingAnotherIssuer: 1145346,
		EntriesBound:               23902,
		EntriesFiltered:            23902,
	}
	bound := []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}
	dir := map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
	}

	v := getRWA(t, rwaServerWithUpstream(t, upstream, bound, dir, rows))
	checkFunnelArithmetic(t, v)
	st := rwaFunnelStages(t, v)
	if got := rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_attestation_stale"); got != 500 {
		t.Errorf("sep1_attestation_stale = %d, want 500", got)
	}
	for _, d := range st["issuers_with_sep1_attestation"].Dropped {
		if d.Reason == "sep1_attestation_stale" && d.Actor != "issuer" {
			t.Errorf("stale drop actor = %q, want issuer — its domain has served nothing since", d.Actor)
		}
	}
}

// TestRWAAssets_FunnelSeparatesNeverFetchedFromDeclaresNothing pins the
// distinction the surface most needs and least had. An issuer whose
// stellar.toml nobody has fetched, one whose payload will not decode,
// and one that decoded and declares nothing are three different
// findings with three different owners. Collapsed into one silent
// `continue`, they are indistinguishable — and the first of the three
// is the one an operator can fix today.
func TestRWAAssets_FunnelSeparatesNeverFetchedFromDeclaresNothing(t *testing.T) {
	// IssuersFetchedWithoutPayload is zero here on purpose: this census
	// says no domain has been reached and come back empty, so all six
	// issuers holding no payload really are a fetch nobody has run.
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain:    10,
		IssuersWithPayload:       4,
		IssuersPayloadUnreadable: 1,
		IssuersDeclaringNothing:  2,
		IssuersDeclaring:         1,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	checkFunnelArithmetic(t, v)

	st := rwaFunnelStages(t, v)
	byOwner := map[string]int{
		"never_fetched":    rwaDropCount(st, "issuers_with_home_domain", "sep1_attestation_never_fetched"),
		"unreadable":       rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_payload_unreadable"),
		"declares_nothing": rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_declares_no_currencies"),
	}
	want := map[string]int{"never_fetched": 6, "unreadable": 1, "declares_nothing": 2}
	for k, w := range want {
		if byOwner[k] != w {
			t.Errorf("%s = %d, want %d — the three fates must stay apart (%+v)", k, byOwner[k], w, byOwner)
		}
	}
	if got := st["issuers_declaring_currencies"].Count; got != 1 {
		t.Errorf("issuers_declaring_currencies = %d, want 1", got)
	}
}

// TestRWAAssets_FunnelDoesNotCallAReachedDomainUnfetched is the
// regression for a label that was false on both halves.
//
// The gap between the issuers publishing a domain and the issuers
// holding a payload was published entirely as
// `sep1_attestation_never_fetched`, actor `operator` — a backlog
// somebody here could clear. Measured on production the gap
// was 40,838 issuers and exactly ONE of them had never been attempted:
// an overnight drain had already reached the other 40,837, and their
// domains served nothing storable (quantumstellar.vercel.app,
// 5138.8888skulls.com, rivalcoins.io — dead, parked, or publishing no
// SEP-1 document). The surface named all of them an operator's unfetched
// backlog, overstating both the coverage within reach and this side's
// share of the gap, on a page whose whole purpose is saying who can
// move a number.
//
// The fixture is that production shape. A fetch that ran and came back
// empty belongs to the issuer; only the genuinely untried one belongs
// to the operator.
func TestRWAAssets_FunnelDoesNotCallAReachedDomainUnfetched(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain: 76658,
		IssuersWithPayload:    35820,
		// Reached, and holding no payload all the same.
		IssuersFetchedWithoutPayload: 40837,
		IssuersPayloadUnreadable:     41,
		IssuersDeclaringNothing:      2109,
		IssuersDeclaring:             33670,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	// The split must not buy its honesty by breaking the accounting:
	// the two drops still have to account exactly for the difference to
	// the next stage, and `balanced` still has to be true.
	checkFunnelArithmetic(t, v)

	st := rwaFunnelStages(t, v)
	actors := map[string]string{}
	counts := map[string]int{}
	for _, d := range st["issuers_with_home_domain"].Dropped {
		actors[d.Reason] = d.Actor
		counts[d.Reason] = d.Count
	}
	if got := counts["domain_served_no_sep1_attestation"]; got != 40837 {
		t.Errorf("domain_served_no_sep1_attestation = %d, want 40837 — every one of these domains was "+
			"reached and served no usable SEP-1 (drops: %+v)", got, st["issuers_with_home_domain"].Dropped)
	}
	if got := actors["domain_served_no_sep1_attestation"]; got != "issuer" {
		t.Errorf("reached-domain drop actor = %q, want issuer — nobody here can fetch a file that is not "+
			"published, and attributing it to the operator advertises coverage that does not exist", got)
	}
	if got := counts["sep1_attestation_never_fetched"]; got != 1 {
		t.Errorf("sep1_attestation_never_fetched = %d, want 1 — only the genuinely untried issuer is a "+
			"backlog an operator can clear", got)
	}
	if got := actors["sep1_attestation_never_fetched"]; got != "operator" {
		t.Errorf("never-fetched drop actor = %q, want operator", got)
	}
	if got := st["issuers_with_sep1_attestation"].Count; got != 35820 {
		t.Errorf("issuers_with_sep1_attestation = %d, want 35820", got)
	}
}

// TestRWAAssets_FunnelSaysUnbalancedWhenTheFetchSplitCannotHold — the
// two halves of the domain-bearing population are read by different
// queries, so they can contradict each other. The wire numbers are
// clamped (a drop larger than the gap it explains is nonsense to
// publish), and the clamp must not be able to make an impossible census
// read as sound: `balanced` comes from the census check as well as the
// stage arithmetic, and the census check bounds the two counts against
// the population independently.
func TestRWAAssets_FunnelSaysUnbalancedWhenTheFetchSplitCannotHold(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		// 4 payloads plus 8 reached-and-empty is 12 issuers, out of a
		// domain-bearing population of 10. No deployment looks like this.
		IssuersWithHomeDomain:        10,
		IssuersWithPayload:           4,
		IssuersFetchedWithoutPayload: 8,
		IssuersPayloadUnreadable:     1,
		IssuersDeclaringNothing:      2,
		IssuersDeclaring:             1,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	if v.Funnel.Balanced {
		t.Errorf("funnel.balanced = true over a census whose fetch split exceeds its own population: %+v",
			v.Funnel.Stages)
	}
}

// TestRWAAssets_DuplicateDeclarationIsServedOnceAndCounted — SEP-1 does
// not forbid a toml declaring the same asset twice, and identity here is
// (code, issuer), so the second declaration is the SAME asset. Serving
// it twice would put its market cap into the summary, the class total
// and the issuer total twice each.
func TestRWAAssets_DuplicateDeclarationIsServedOnceAndCounted(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}},
	))
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want ONE row — (code, issuer) is the identity, so the second declaration "+
			"is the same asset and serving it twice doubles its market cap in every total", rwaAssetIDs(v))
	}
	if v.Summary.Assets != 1 {
		t.Errorf("summary.assets = %d, want 1", v.Summary.Assets)
	}
	// The exact total, not an approximation: 12336218000000 stroops of
	// supply at 1.0412 is 1284507193.16 dollars, once.
	if v.Summary.MarketCapUSD == nil {
		t.Fatal("summary.market_cap_usd absent, want the single asset's cap")
	}
	single := *v.Summary.MarketCapUSD
	for _, g := range v.ByClass {
		if g.Assets != 1 {
			t.Errorf("by_class[%s].assets = %d, want 1", g.Class, g.Assets)
		}
		if g.MarketCapUSD == nil || *g.MarketCapUSD != single {
			t.Errorf("by_class[%s].market_cap_usd = %v, want the same %q the summary carries",
				g.Class, g.MarketCapUSD, single)
		}
	}
	for _, i := range v.ByIssuer {
		if i.Assets != 1 {
			t.Errorf("by_issuer[%s].assets = %d, want 1", i.Issuer, i.Assets)
		}
	}
	st := rwaFunnelStages(t, v)
	if got := rwaDropCount(st, "candidate_assets_evaluated", "duplicate_declaration_of_the_same_asset"); got != 1 {
		t.Errorf("duplicate drop = %d, want 1 — a deduplicated candidate is still a candidate that entered", got)
	}
	checkFunnelArithmetic(t, v)
}

// TestRWAAssets_AdmittedButNeverObservedIsCounted — an asset can meet
// every requirement and still have no row in the catalogue, because the
// index has never seen it on chain. It is correctly absent from the set;
// it was NOT correct for it to vanish without a count, which made
// "admitted" and "served" silently different numbers.
func TestRWAAssets_AdmittedButNeverObservedIsCounted(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("NEVERSEEN", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		// The catalogue holds USTRY only.
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}},
	))
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want only the observed one", rwaAssetIDs(v))
	}
	st := rwaFunnelStages(t, v)
	if got := st["assets_admitted"].Count; got != 2 {
		t.Errorf("assets_admitted = %d, want 2 — both met the definition", got)
	}
	if got := rwaDropCount(st, "assets_admitted", "admitted_but_never_observed_on_chain"); got != 1 {
		t.Errorf("never-observed drop = %d, want 1 — admitted and served were silently different numbers", got)
	}
	checkFunnelArithmetic(t, v)
}

// TestRWAAssets_IssuerAssetPageTruncationIsReported — the per-issuer
// listing read is capped, and the cap has always been DOCUMENTED as
// reported ("the cap is reported the same way the issuer cap is") while
// nothing reported it. An issuer with more classic assets than one page
// has its tail unread, so a member in that tail disappears from the set
// with nothing to show for it.
//
// The count is issuers-with-an-unread-tail, not assets, so it is served
// as its own signal and kept out of the asset arithmetic. Counting it
// as assets would make the funnel close by inventing a number.
func TestRWAAssets_IssuerAssetPageTruncationIsReported(t *testing.T) {
	const page = 500
	rows := make([]timescale.AssetRow, 0, page)
	rows = append(rows, rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312))
	for i := 1; i < page; i++ {
		rows = append(rows, rwaRow("FILLER"+string(rune('A'+i%26))+itoaRWA(i), rwaGoodIssuer, sptr("0.01"), int64(i)))
	}
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: rows},
	))
	st := rwaFunnelStages(t, v)
	if got := rwaDropCount(st, "assets_admitted", "issuer_asset_page_truncated"); got != 1 {
		t.Errorf("issuer_asset_page_truncated = %d, want 1 — a full page means an unread tail, and a member "+
			"in it would vanish from the set unreported", got)
	}
	// The signal must not be allowed into the asset subtraction.
	checkFunnelArithmetic(t, v)
}

// TestRWAAssets_FunnelIsNotServedAsZerosWhenUnmeasured — when the
// attestation read fails there is no population to report. Publishing a
// funnel of zeros would read as a measured network with nothing in it,
// which is the same lie as a market cap of "0.00".
func TestRWAAssets_FunnelIsNotServedAsZerosWhenUnmeasured(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache:    &stubSep1BoundReader{err: errRWAScan},
		Directory:    &stubDirectoryReader{},
		AssetsReader: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}},
	})
	v := getRWA(t, srv)
	if len(v.Funnel.Stages) != 0 {
		t.Errorf("funnel served %d stages for an unmeasured population: %+v", len(v.Funnel.Stages), v.Funnel.Stages)
	}
	if v.Funnel.Balanced {
		t.Error("funnel.balanced = true for a population that was never walked")
	}
	if v.Funnel.Basis == "" {
		t.Error("funnel.basis is empty — the absence has to be stated, not implied by empty stages")
	}
}

// TestRWAAssets_FunnelSaysUnbalancedWhenTheCensusDoesNot — an
// accounting that cannot be reconciled must SAY so on the wire. A
// reader silently failing to make the numbers meet is the outcome the
// whole structure exists to prevent, and it is worse than publishing no
// numbers at all.
//
// The fixture is a census that cannot describe any deployment: more
// issuers carrying a fetched payload than carrying a home_domain, when
// the payload is fetched FROM the home_domain.
func TestRWAAssets_FunnelSaysUnbalancedWhenTheCensusDoesNot(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		// Fewer issuers with a home_domain than with a payload, which
		// cannot happen: a payload is fetched FROM a home_domain.
		IssuersWithHomeDomain:   1,
		IssuersWithPayload:      9,
		IssuersDeclaringNothing: 8,
		IssuersDeclaring:        1,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	if v.Funnel.Balanced {
		t.Errorf("funnel.balanced = true over a census that cannot be reconciled: %+v", v.Funnel.Stages)
	}
}

// TestRWAAssets_RefusalTallyStaysConsistentWithTheFunnel — the two views
// answer different questions (an ordered requirement tally versus the
// whole narrowing) and must never disagree about the requirement
// refusals they both report.
func TestRWAAssets_RefusalTallyStaysConsistentWithTheFunnel(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("MEME", rwaGoodIssuer, "etherfuse.com", "crypto"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {Address: rwaScamIssuer, Tags: []string{"issuer", "malicious"}, Source: "stellar-expert"},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("MEME", rwaGoodIssuer, sptr("0.02"), 12),
		},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))
	checkFunnelArithmetic(t, v)

	refused := map[string]int{}
	for _, r := range v.Refused {
		refused[r.Reason] = r.Assets
	}
	st := rwaFunnelStages(t, v)
	for _, reason := range []string{"issuer_scam_flagged", "issuer_not_independently_recognised"} {
		if got, want := rwaDropCount(st, "candidate_assets_evaluated", reason), refused[reason]; got != want {
			t.Errorf("%s: funnel says %d, refused[] says %d — the two views must never disagree",
				reason, got, want)
		}
	}
	// The requirement-4 pre-filter appears in BOTH views on purpose: as
	// its own funnel stage (where it happened) and in refused[] under
	// requirement 4 (which requirement it is). The funnel must not
	// double-count it into the evaluated stage.
	if got := rwaDropCount(st, "issuer_bound_entries", "no_real_world_instrument_basis"); got != 1 {
		t.Errorf("pre-filter stage = %d, want 1 (the crypto-anchored token)", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "no_real_world_instrument_basis"); got != 0 {
		t.Errorf("evaluated stage also counts %d under requirement 4 — the pre-filter drop is counted twice "+
			"and the funnel cannot close", got)
	}
	if refused["no_real_world_instrument_basis"] != 1 {
		t.Errorf("refused[no_real_world_instrument_basis] = %d, want 1",
			refused["no_real_world_instrument_basis"])
	}
}

// TestRWAAssets_ChargesOneTokenPerListingRead: /v1/rwa/assets runs one
// uncached /v1/assets listing read (and its pipeline) per member issuer
// on every request, so two member issuers cost two tokens, not the one
// the pre-dispatch charge takes.
func TestRWAAssets_ChargesOneTokenPerListingRead(t *testing.T) {
	ts, reader := newRWALimitedServer(t, 100)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := reader.lists.Load(); got != 2 {
		t.Fatalf("listing reads = %d, want 2 (one per member issuer)", got)
	}
	if got := rwaRemaining(t, ts, resp); got != 98 {
		t.Fatalf("X-RateLimit-Remaining = %d, want 98", got)
	}
}

// TestRWAAssets_ExhaustsTheBucketInLimitOverCostRequests: a 4-token
// budget buys two 2-token requests, and the third is refused with a
// truthful 429 before any listing read.
func TestRWAAssets_ExhaustsTheBucketInLimitOverCostRequests(t *testing.T) {
	ts, reader := newRWALimitedServer(t, 4)
	for i := 1; i <= 2; i++ {
		if resp := mustGet(t, ts.URL+"/v1/rwa/assets"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
	readsBefore := reader.lists.Load()

	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 3: status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got != "4" {
		t.Fatalf("X-RateLimit-Limit = %q, want 4", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if got := reader.lists.Load(); got != readsBefore {
		t.Fatalf("a refused request made %d listing reads, want 0", got-readsBefore)
	}
}

// TestRWAAssets_ServesTheDiscountToTheInstrumentValuation is the
// end-to-end positive path: an admitted tokenized treasury, an
// independent oracle's valuation of the instrument, and the gap between
// that and what the Stellar market pays.
func TestRWAAssets_ServesTheDiscountToTheInstrumentValuation(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			// 1.02033610 against a 1.07403800 valuation — a 5% discount.
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference == nil {
		t.Fatal("no independent valuation served for an instrument an oracle prices")
	}
	if a.Reference.PriceUSD != "1.07403800" || a.Reference.Source != "redstone" {
		t.Errorf("reference = %+v, want the oracle figure verbatim from its publisher", a.Reference)
	}
	if a.Reference.Feed != "rwa:USTRY" || a.Reference.Quote != "fiat:USD" {
		t.Errorf("reference provenance = %s/%s — the instrument and its denominator travel with the figure",
			a.Reference.Feed, a.Reference.Quote)
	}
	if a.Premium.Status != v1.RWAPremiumPublished {
		t.Fatalf("premium status = %q, want published", a.Premium.Status)
	}
	if a.Premium.Pct == nil || *a.Premium.Pct != "-5.0000" {
		t.Errorf("premium pct = %v, want -5.0000", a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 1 || v.Summary.AssetsCompared != 1 {
		t.Errorf("summary reference/compared = %d/%d, want 1/1",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
	if !strings.Contains(v.Summary.Basis, "issuer declares") {
		t.Errorf("the basis does not state what the comparison rests on: %q", v.Summary.Basis)
	}
	// The curated bindings travel with the rows so a consumer can audit
	// every pair this surface is willing to compare.
	var bound bool
	for _, b := range v.Definition.BoundInstruments {
		if b.Code == "USTRY" && b.Issuer == rwaGoodIssuer && b.Feed == "rwa:USTRY" {
			bound = true
		}
	}
	if !bound {
		t.Errorf("the binding behind the served figure is not in definition.bound_instruments: %+v",
			v.Definition.BoundInstruments)
	}
}

// TestRWAAssets_ImpersonatorGetsNoInstrumentValuation is the
// impersonation case in its sharpest form. An issuer flagged AFTER
// admission keeps its row — this surface hides nothing it admitted — but
// gets no valuation of any kind, INCLUDING a third party's. Handing an
// impersonator the real instrument's oracle NAV would publish a bigger
// claim than the one the flag suppressed.
func TestRWAAssets_ImpersonatorGetsNoInstrumentValuation(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &rwaSkewedDirectory{
			membership: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer"}, Source: "stellar-expert",
			},
			rowFill: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
			},
		},
		Oracle: rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
			},
			supply: map[string]string{"USTRY-" + rwaGoodIssuer: "12336218000000"},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v — an admitted row is not removed when the flag lands", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "withheld_issuer_flagged" {
		t.Fatalf("valuation status = %q, want withheld_issuer_flagged", a.Valuation.Status)
	}
	if a.Reference != nil {
		t.Errorf("a flagged issuer's token was handed an independent instrument valuation: %+v", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumIssuerFlagged {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumIssuerFlagged)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q on a flagged issuer", *a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 0 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary counted a flagged row as valued: reference/compared = %d/%d",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_UnpricedAssetIsNotComparedToZero. An instrument no
// Stellar market prices keeps its independent valuation and reports the
// comparison as unmade. A premium of "0" there would read as "trades at
// par", which is the one reading that is certainly wrong.
func TestRWAAssets_UnpricedAssetIsNotComparedToZero(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("TESOURO", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("TESOURO", rwaGoodIssuer, nil, 13804)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:TESOURO", "fiat:USD", "24538100", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "unpriced" || a.Valuation.PriceUSD != nil {
		t.Fatalf("valuation = %+v, want unpriced with no figure", a.Valuation)
	}
	if a.Reference == nil || a.Reference.PriceUSD != "0.24538100" {
		t.Fatalf("reference = %+v — an unpriced token still has an independent valuation", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumNoMarketPrice {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumNoMarketPrice)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent — an unmade comparison is not par", *a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 1 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary reference/compared = %d/%d, want 1/0",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_SpotFeedIsNotServedAsATokenValuation carries the
// unit-scope refusal through the handler. `rwa:XAU` is spot gold per
// troy ounce; a token coded XAU is a token of unstated size, and their
// ratio published as a percentage would read as a 99.99% discount on an
// ordinary token.
func TestRWAAssets_SpotFeedIsNotServedAsATokenValuation(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("XAU", rwaGoodIssuer, "etherfuse.com", "commodity")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("XAU", rwaGoodIssuer, sptr("0.50000000"), 1438878)},
		},
		rwaOracle(t, rwaOracleRow(t, "reflector-fx", "rwa:XAU", "fiat:USD", "440086022830869146", 14)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference != nil {
		t.Errorf("a per-troy-ounce spot price was served as a token's valuation: %+v", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumNotInstrumentScoped {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumNotInstrumentScoped)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a unit conversion must never be served as a discount", *a.Premium.Pct)
	}
}

// TestRWAAssets_ReferenceSnapshotIsCachedNotRefetchedPerRequest — the
// stream read is a hypertable scan over every active oracle feed, so one
// snapshot must serve the whole set rather than one read per request.
func TestRWAAssets_ReferenceSnapshotIsCachedNotRefetchedPerRequest(t *testing.T) {
	oracle := rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8))
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 1)},
		},
		oracle,
	)
	ts := httpTestServer(t, srv)
	for range 3 {
		resp := mustGet(t, ts.URL+"/v1/rwa/assets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if oracle.calls != 1 {
		t.Errorf("LatestOracleStreams called %d times; the TTL cache should scan once", oracle.calls)
	}
}

// TestRWAAssets_NoOracleReaderStillServesTheSet. The reference is an
// addition to the surface, not a precondition for it: a deployment
// without an oracle reader serves the set with the comparison reported
// as unavailable — never as a zero, never as an error.
func TestRWAAssets_NoOracleReaderStillServesTheSet(t *testing.T) {
	srv := rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Reference != nil {
		t.Errorf("reference served with no oracle wired: %+v", a.Reference)
	}
	if a.Premium.Status != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("premium status = %q, want %q — a deployment that cannot read the oracles "+
			"has not learned that no oracle publishes this instrument",
			a.Premium.Status, v1.RWAPremiumReferenceUnavailable)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q with no oracle wired", *a.Premium.Pct)
	}
	if a.Valuation.Status != "published" {
		t.Errorf("valuation status = %q — the set does not depend on the oracle", a.Valuation.Status)
	}
}

// TestRWAAssets_FailedOracleReadIsNotServedAsAnAbsence is D2 through the
// real handler. A reader wired but erroring is the ordinary production
// failure — a refused connection, a timed-out scan — and it must not
// publish "no oracle publishes a valuation for this instrument" on every
// row. From process start until the first successful read there is
// nothing to carry forward, so this is exactly the window in which the
// wrong status would be served.
func TestRWAAssets_FailedOracleReadIsNotServedAsAnAbsence(t *testing.T) {
	failing := &rwaOracleStub{
		stubOracleReader: &stubOracleReader{},
		err:              errors.New("dial tcp 127.0.0.1:5432: connection refused"),
	}
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
		failing,
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v — the set is served whatever the oracles do", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Premium.Status != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, v1.RWAPremiumReferenceUnavailable)
	}
	if a.Premium.Status == v1.RWAPremiumNoReference {
		t.Error("a failed read was published as a finding about what the oracles carry")
	}
	if a.Reference != nil || a.Premium.Pct != nil {
		t.Errorf("a figure was served from a failed read: ref=%+v pct=%v", a.Reference, a.Premium.Pct)
	}
	if v.Summary.AssetsWithReference != 0 || v.Summary.AssetsCompared != 0 {
		t.Errorf("summary reference/compared = %d/%d on a failed read",
			v.Summary.AssetsWithReference, v.Summary.AssetsCompared)
	}
}

// TestRWAAssets_ReferenceIsBoundToTheIssuerNotTheCode is the identity
// rule this whole surface is built on, applied to the figure the last
// change added.
//
// Two recognised issuers each publish a domain-bound SEP-1 entry for
// USTRY. One is the issuer whose instrument the oracle feed tracks; the
// other is an unrelated token that happens to share the ticker. A join
// on the code alone answers BOTH with the same treasury valuation, and
// the unrelated token — trading at $0.20 — is published at an 81%
// discount to a security it has nothing to do with.
//
// That is the attacker-authored-pricing class in a new coordinate:
// identity is (code, issuer), never the code alone.
func TestRWAAssets_ReferenceIsBoundToTheIssuerNotTheCode(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("USTRY", rwaOtherRecognisedIssuer, "example.test", "bond"),
		},
		map[string]timescale.DirectoryEntry{
			rwaGoodIssuer:            recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
			rwaOtherRecognisedIssuer: recognisedIssuer(rwaOtherRecognisedIssuer, "Someone Else"),
		},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer:            {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)},
			rwaOtherRecognisedIssuer: {rwaRow("USTRY", rwaOtherRecognisedIssuer, sptr("0.20000000"), 91)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 2 {
		t.Fatalf("assets = %v, want both issuers' tokens", rwaAssetIDs(v))
	}
	var other *v1.RWAAsset
	for i := range v.Assets {
		if v.Assets[i].Issuer == rwaOtherRecognisedIssuer {
			other = &v.Assets[i]
		}
	}
	if other == nil {
		t.Fatalf("the second issuer's token is missing: %v", rwaAssetIDs(v))
	}
	if other.Reference != nil {
		t.Errorf("an unrelated issuer's token was given the instrument's valuation on a code match: %+v",
			other.Reference)
	}
	if other.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a false claim about a security this token has nothing to do with",
			*other.Premium.Pct)
	}
	if other.Premium.Status != v1.RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", other.Premium.Status, v1.RWAPremiumNotBound)
	}
}

// TestRWAAssets_ReferenceRefusesAnUnboundIssuerForABoundCode — the code-keyed join
// folded case, so XAUM matched the XAUm feed. A binding names the exact
// (code, issuer) the chain carries; a case variant under an unbound
// issuer is a different token.
func TestRWAAssets_ReferenceRefusesAnUnboundIssuerForABoundCode(t *testing.T) {
	srv := rwaServerWithOracle(t,
		[]timescale.Sep1BoundCurrency{
			rwaBound("CETES", rwaOtherRecognisedIssuer, "example.test", "bond"),
		},
		map[string]timescale.DirectoryEntry{
			rwaOtherRecognisedIssuer: recognisedIssuer(rwaOtherRecognisedIssuer, "Someone Else"),
		},
		map[string][]timescale.AssetRow{
			rwaOtherRecognisedIssuer: {rwaRow("CETES", rwaOtherRecognisedIssuer, sptr("0.20000000"), 91)},
		},
		rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:CETES", "fiat:USD", "6988900", 8)),
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v", rwaAssetIDs(v))
	}
	if v.Assets[0].Reference != nil {
		t.Errorf("an unbound issuer received a bound instrument's valuation: %+v", v.Assets[0].Reference)
	}
	if v.Assets[0].Premium.Status != v1.RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", v.Assets[0].Premium.Status, v1.RWAPremiumNotBound)
	}
}

// TestRWAAssets_AdmitsADeclaredAndRecognisedAsset is the positive path:
// a classic asset whose issuer-bound SEP-1 entry declares `bond` and
// whose issuer the curated directory recognises is served with its
// valuation and the evidence that admitted it.
func TestRWAAssets_AdmitsADeclaredAndRecognisedAsset(t *testing.T) {
	srv := rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{
			rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		},
	)
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want exactly USTRY", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.AssetID != "USTRY-"+rwaGoodIssuer {
		t.Errorf("asset_id = %q", a.AssetID)
	}
	if a.Issuer != rwaGoodIssuer {
		t.Errorf("issuer = %q — identity must carry the G-address, never the code alone", a.Issuer)
	}
	if a.Basis != "sep1_anchor_declaration" || a.AnchorClass != "bond" {
		t.Errorf("basis/class = %q/%q, want sep1_anchor_declaration/bond", a.Basis, a.AnchorClass)
	}
	if a.IssuerDirectoryName != "Etherfuse" {
		t.Errorf("issuer_directory_name = %q — the independent recognition must be shown", a.IssuerDirectoryName)
	}
	if a.HomeDomain != "etherfuse.com" {
		t.Errorf("home_domain = %q", a.HomeDomain)
	}
	if a.FirstSeenLedger != 55008233 {
		t.Errorf("first_seen_ledger = %d — the coverage claim needs the genesis-complete first sighting", a.FirstSeenLedger)
	}
	if v.Summary.Assets != 1 || v.Summary.Issuers != 1 {
		t.Errorf("summary assets/issuers = %d/%d, want 1/1", v.Summary.Assets, v.Summary.Issuers)
	}
	if v.Definition.DocumentationURL == "" || len(v.Definition.Requirements) != 4 {
		t.Errorf("definition not served with the rows: %+v", v.Definition)
	}
}

// TestRWAAssets_FullIssuerPageMakesTheTotalALowerBound pins
// The scan-cap rule on the classic arm: an issuer whose listing page fills
// may hold a member in the unread tail, so a total over the served rows is
// partial even when every one of them is valued.
func TestRWAAssets_FullIssuerPageMakesTheTotalALowerBound(t *testing.T) {
	page := []timescale.AssetRow{rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}
	for i := len(page); i < 500; i++ {
		page = append(page, rwaRow(fmt.Sprintf("FILL%03d", i), rwaGoodIssuer, nil, 1))
	}
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: page},
	))
	if len(v.Assets) != 1 || v.Summary.AssetsUnvalued != 0 || v.Summary.MarketCapUSD == nil {
		t.Fatalf("assets = %v unvalued = %d cap = %v, want one valued row",
			rwaAssetIDs(v), v.Summary.AssetsUnvalued, v.Summary.MarketCapUSD)
	}
	if !v.Summary.LowerBound || !v.Summary.Truncated {
		t.Errorf("lower_bound/truncated = %v/%v, want true/true with a full issuer page",
			v.Summary.LowerBound, v.Summary.Truncated)
	}
	if !strings.Contains(v.Summary.Basis, "1 member issuer(s) have more classic assets than one listing page reads") {
		t.Errorf("basis does not name the page cap: %q", v.Summary.Basis)
	}
}

// TestRWAAssets_RefusesAssetsFailingTheDefinition is the regression that
// matters most. Four assets are attested; three fail a requirement and
// MUST be absent, not merely downranked:
//
//   - the same instrument code from a scam-flagged lookalike issuer,
//   - a real-world declaration from an issuer nobody recognises,
//   - a recognised issuer's token that declares `crypto` — outside the
//     closed real-world vocabulary — and whose code no oracle prices.
//
// Each was checked to fail on its own, so a single over-broad rule
// cannot make the whole assertion pass.
func TestRWAAssets_RefusesAssetsFailingTheDefinition(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("MEME", rwaGoodIssuer, "etherfuse.com", "crypto"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Name: "Fake", Domain: "stellar.us.org",
			Tags: []string{"issuer", "malicious", "unsafe"}, Source: "stellar-expert",
		},
		// rwaUnknownIssuer is absent from the directory entirely.
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("MEME", rwaGoodIssuer, sptr("0.02"), 12),
		},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))

	got := rwaAssetIDs(v)
	if len(got) != 1 || got[0] != "USTRY-"+rwaGoodIssuer {
		t.Fatalf("assets = %v, want only USTRY-%s", got, rwaGoodIssuer)
	}
	for _, a := range v.Assets {
		if a.Issuer == rwaScamIssuer {
			t.Error("a scam-flagged issuer reached the RWA set — the surface is a phishing amplifier")
		}
		if a.Issuer == rwaUnknownIssuer {
			t.Error("an unrecognised issuer reached the set on its own say-so")
		}
		if a.Code == "MEME" {
			t.Error("a crypto-anchored token reached the real-world set")
		}
	}

	refused := map[string]int{}
	for _, r := range v.Refused {
		refused[r.Reason] = r.Assets
	}
	if refused["issuer_scam_flagged"] != 1 {
		t.Errorf("refused[issuer_scam_flagged] = %d, want 1 (%v)", refused["issuer_scam_flagged"], v.Refused)
	}
	if refused["issuer_not_independently_recognised"] != 1 {
		t.Errorf("refused[issuer_not_independently_recognised] = %d, want 1 (%v)",
			refused["issuer_not_independently_recognised"], v.Refused)
	}
	if refused["no_real_world_instrument_basis"] != 1 {
		t.Errorf("refused[no_real_world_instrument_basis] = %d, want 1 (%v)",
			refused["no_real_world_instrument_basis"], v.Refused)
	}
}

// TestRWAAssets_OracleBasisNeedsARecognisedIssuer pins the one arm that
// matches on a CODE. An RWA oracle prices an instrument called XAU;
// dozens of Stellar accounts issue a token called XAU. The oracle basis
// admits one only when an independent party has already recognised the
// issuing account, and it never invents an anchor class.
func TestRWAAssets_OracleBasisNeedsARecognisedIssuer(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		{Code: "XAU", Issuer: rwaGoodIssuer, HomeDomain: "xau.cl", Name: "XAU"},
		{Code: "XAU", Issuer: rwaScamIssuer, HomeDomain: "swisscustody.net", Name: "XAU"},
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "XAU CL"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Tags: []string{"malicious", "unsafe"}, Source: "stellar-expert",
		},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("XAU", rwaGoodIssuer, sptr("4115.67"), 1436959)},
		rwaScamIssuer: {rwaRow("XAU", rwaScamIssuer, sptr("4115.67"), 57775)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))

	if got := rwaAssetIDs(v); len(got) != 1 || got[0] != "XAU-"+rwaGoodIssuer {
		t.Fatalf("assets = %v, want only XAU-%s", got, rwaGoodIssuer)
	}
	a := v.Assets[0]
	if a.Basis != "oracle_rwa_feed" {
		t.Errorf("basis = %q, want oracle_rwa_feed", a.Basis)
	}
	if a.AnchorClass != "" {
		t.Errorf("anchor_class = %q — an oracle feed names an instrument, not a class", a.AnchorClass)
	}
	for _, g := range v.ByClass {
		if g.Class == "unclassified" && g.Assets == 1 {
			return
		}
	}
	t.Errorf("by_class = %+v, want the oracle-basis asset grouped as unclassified", v.ByClass)
}

// TestRWAAssets_UnpricedAssetRendersWithheldNotZero — an asset with no
// served USD price publishes NO market cap and says why. The summary
// total counts it as unvalued and marks itself a lower bound rather
// than absorbing a zero.
func TestRWAAssets_UnpricedAssetRendersWithheldNotZero(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("TESOURO", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {
		rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
		// No price: the aggregator produced none, or the substance
		// gate withheld it as too thin to aggregate.
		rwaRow("TESOURO", rwaGoodIssuer, nil, 13318),
	}}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		rows))

	if len(v.Assets) != 2 {
		t.Fatalf("assets = %v, want both members", rwaAssetIDs(v))
	}
	var unpriced *v1.RWAAsset
	for i := range v.Assets {
		if v.Assets[i].Code == "TESOURO" {
			unpriced = &v.Assets[i]
		}
	}
	if unpriced == nil {
		t.Fatal("the unpriced member was dropped; it must be served as unavailable, not hidden")
	}
	if unpriced.Valuation.Status != "unpriced" {
		t.Errorf("valuation.status = %q, want unpriced", unpriced.Valuation.Status)
	}
	if unpriced.Valuation.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q — an unavailable valuation must be ABSENT, never a number",
			*unpriced.Valuation.MarketCapUSD)
	}
	if unpriced.Valuation.PriceUSD != nil {
		t.Errorf("price_usd = %q, want absent", *unpriced.Valuation.PriceUSD)
	}
	if v.Assets[0].Code != "USTRY" {
		t.Errorf("assets[0] = %q — an unvalued row must never rank above a valued one", v.Assets[0].Code)
	}
	if v.Summary.AssetsUnvalued != 1 || !v.Summary.LowerBound {
		t.Errorf("summary unvalued/lower_bound = %d/%v, want 1/true",
			v.Summary.AssetsUnvalued, v.Summary.LowerBound)
	}
}

// TestRWAAssets_NoPublishedValuationOmitsTheTotal — when nothing in the
// set publishes a market cap, the summary carries NO total. Serving
// "0.00" would read as a real total of zero dollars, which is the one
// reading that is certainly wrong.
func TestRWAAssets_NoPublishedValuationOmitsTheTotal(t *testing.T) {
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, nil, 1)}},
	))
	if v.Summary.MarketCapUSD != nil {
		t.Errorf("summary.market_cap_usd = %q, want absent", *v.Summary.MarketCapUSD)
	}
	if v.Summary.AssetsValued != 0 || !v.Summary.LowerBound {
		t.Errorf("summary valued/lower_bound = %d/%v, want 0/true", v.Summary.AssetsValued, v.Summary.LowerBound)
	}
	for _, g := range v.ByClass {
		if g.MarketCapUSD != nil {
			t.Errorf("by_class[%s].market_cap_usd = %q, want absent", g.Class, *g.MarketCapUSD)
		}
	}
}

// TestRWAAssets_IssuerFlaggedAfterAdmissionRendersWithheld — the
// membership set is cached for ten minutes, so an issuer can acquire a
// scam-class tag while a member row is still in it. The row is then
// served with the same suppression /v1/assets applies: the price and
// market cap are withheld and the status says why. Membership hides
// nothing it admitted; it withholds the number.
//
// The stub directory answers the BATCH lookup used by the row-fill
// overlay with the flag, and the single lookup the membership build
// consumed with a clean entry — reproducing the staleness window
// directly.
func TestRWAAssets_IssuerFlaggedAfterAdmissionRendersWithheld(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &rwaSkewedDirectory{
			membership: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer"}, Source: "stellar-expert",
			},
			rowFill: timescale.DirectoryEntry{
				Address: rwaGoodIssuer, Name: "Etherfuse",
				Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
			},
		},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
			},
			supply: map[string]string{"USTRY-" + rwaGoodIssuer: "12336218000000"},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want the admitted row still served", rwaAssetIDs(v))
	}
	a := v.Assets[0]
	if a.Valuation.Status != "withheld_issuer_flagged" {
		t.Errorf("valuation.status = %q, want withheld_issuer_flagged", a.Valuation.Status)
	}
	if a.Valuation.PriceUSD != nil || a.Valuation.MarketCapUSD != nil {
		t.Error("a flagged issuer published a price or a market cap on the RWA surface")
	}
	if v.Summary.MarketCapUSD != nil {
		t.Errorf("summary.market_cap_usd = %q — a withheld row must not reach the total", *v.Summary.MarketCapUSD)
	}
}

// TestRWAAssets_UnavailableWhenTheDirectoryCannotAnswer — requirement 3
// is what keeps impersonators out, so it fails CLOSED. With no
// directory wired the surface publishes an empty set and says why,
// rather than serving every self-declared candidate.
func TestRWAAssets_UnavailableWhenTheDirectoryCannotAnswer(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
			rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.04"), 1)},
				rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.14"), 1)},
			},
		},
	})
	v := getRWA(t, srv)
	if len(v.Assets) != 0 {
		t.Fatalf("assets = %v, want an empty set when recognition cannot be evaluated", rwaAssetIDs(v))
	}
	if v.Summary.MarketCapUSD != nil {
		t.Error("a total was published for a set that could not be established")
	}
	if v.Summary.Basis == "" {
		t.Error("an empty set was served with no statement of why")
	}
}

// TestRWAAssets_MembershipIsCachedNotRebuiltPerRequest — the scan behind
// membership walks every issuer carrying a SEP-1 payload, so it must
// stay off the request path.
func TestRWAAssets_MembershipIsCachedNotRebuiltPerRequest(t *testing.T) {
	sep1 := &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
	}}
	srv := v1.New(v1.Options{
		Sep1Cache: sep1,
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
			rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer: map[string][]timescale.AssetRow{
				rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 1)},
			},
		},
	})
	ts := httpTestServer(t, srv)
	for range 3 {
		resp := mustGet(t, ts.URL+"/v1/rwa/assets")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if sep1.calls != 1 {
		t.Errorf("BoundSep1Currencies called %d times; the TTL cache should scan once", sep1.calls)
	}
}

// TestRWAAssets_CodesDifferingOnlyInCaseAreDistinctAssets: Stellar asset
// codes are case-sensitive, so one issuer's USTRY and ustry are two
// assets. The membership join must attach the declaration to the asset
// it names and never to its case twin.
func TestRWAAssets_CodesDifferingOnlyInCaseAreDistinctAssets(t *testing.T) {
	dir := map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}
	twins := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("ustry", rwaGoodIssuer, sptr("0.02"), 12),
		},
	}

	// Only USTRY is declared: its twin must not be served in its place.
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}, dir, twins))
	if got := rwaAssetIDs(v); len(got) != 1 || got[0] != "USTRY-"+rwaGoodIssuer {
		t.Errorf("USTRY declared: assets = %v, want exactly [USTRY-%s]", got, rwaGoodIssuer)
	}

	// Both declared: both are members, each joined to its own row.
	v = getRWA(t, rwaServer(t, []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("ustry", rwaGoodIssuer, "etherfuse.com", "bond"),
	}, dir, twins))
	got := rwaAssetIDs(v)
	sort.Strings(got)
	if want := []string{"USTRY-" + rwaGoodIssuer, "ustry-" + rwaGoodIssuer}; !slices.Equal(got, want) {
		t.Errorf("both declared: assets = %v, want %v", got, want)
	}

	// A mis-cased declaration still reaches the one asset it can mean.
	v = getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("Ustry", rwaGoodIssuer, "etherfuse.com", "bond")}, dir,
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}}))
	if got := rwaAssetIDs(v); len(got) != 1 || got[0] != "USTRY-"+rwaGoodIssuer {
		t.Errorf("mis-cased declaration: assets = %v, want [USTRY-%s]", got, rwaGoodIssuer)
	}

	// A mis-cased declaration that fits both twins names neither.
	v = getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("Ustry", rwaGoodIssuer, "etherfuse.com", "bond")}, dir, twins))
	if got := rwaAssetIDs(v); len(got) != 0 {
		t.Errorf("ambiguous mis-cased declaration: assets = %v, want none", got)
	}
}

const (
	// A recognised, non-flagged issuer that declares a real-world
	// anchor type on a bound SEP-1 entry.
	rwaGoodIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	// An issuer that declares the SAME instrument codes from a
	// lookalike domain and carries scam-class directory tags.
	rwaScamIssuer = "GCUG7ARUFEEUMSL56K7245YCPXPZPOXAY6TSRXZB2JZFBI4DOBVOTUSA"
	// An issuer that declares a real-world anchor type but that no
	// independent party has recognised.
	rwaUnknownIssuer = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
)

// stubSep1BoundReader serves canned issuer-bound SEP-1 entries. It also
// satisfies v1.Sep1CachedReader so it can be wired as Options.Sep1Cache.
//
// It censuses what it serves the way the real scan does, so a test that
// asserts on the funnel is asserting against an accounting that closes
// rather than against numbers a stub invented. `upstream` lets a test
// state the population BEFORE the bound entries — the issuer rows and
// declarations a real deployment holds — which is the part of the
// funnel no fixture of bound entries can express on its own.
type stubSep1BoundReader struct {
	bound    []timescale.Sep1BoundCurrency
	upstream timescale.Sep1BoundCensus
	err      error
	calls    int
}

func (s *stubSep1BoundReader) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (s *stubSep1BoundReader) BoundSep1Currencies(
	_ context.Context, keep timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	s.calls++
	if s.err != nil {
		return nil, timescale.Sep1BoundCensus{}, s.err
	}
	census := s.upstream
	out := make([]timescale.Sep1BoundCurrency, 0, len(s.bound))
	issuers := map[string]struct{}{}
	for _, c := range s.bound {
		issuers[c.Issuer] = struct{}{}
		census.Entries++
		census.EntriesBound++
		if keep == nil || keep(c) {
			census.EntriesKept++
			out = append(out, c)
			continue
		}
		census.EntriesFiltered++
	}
	if census.IssuersDeclaring == 0 {
		census.IssuersDeclaring = len(issuers)
	}
	if census.IssuersWithPayload == 0 {
		census.IssuersWithPayload = census.IssuersDeclaring +
			census.IssuersPayloadUnreadable + census.IssuersDeclaringNothing
	}
	if census.IssuersWithHomeDomain == 0 {
		census.IssuersWithHomeDomain = census.IssuersWithPayload
	}
	return out, census, nil
}

// rwaListStub answers ListAssetsExt from a per-issuer row map, the way
// the real store answers the Issuer-filtered listing query.
type rwaListStub struct {
	*stubAssetsReaderExt
	byIssuer map[string][]timescale.AssetRow
	supply   map[string]string
}

func (l *rwaListStub) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	return l.byIssuer[opts.Issuer], nil
}

// LatestSupplyObservations satisfies the optional supply seam
// fillMarketCapsFromSupply type-asserts for, so these tests exercise
// the real market-cap fill rather than a path where every row is
// unvalued for want of a supply reader. The stub ignores the freshness
// bound — [TestLatestPreciseSupply_AsksForABoundedRead] is what pins that
// the serving path asks for one.
func (l *rwaListStub) LatestSupplyObservations(
	context.Context, time.Duration,
) (map[string]timescale.SupplyObservation, error) {
	out := make(map[string]timescale.SupplyObservation, len(l.supply))
	for assetID, circ := range l.supply {
		out[assetID] = timescale.SupplyObservation{
			CirculatingSupply: circ,
			Basis:             string(supply.BasisIssuerExclusion),
			ObservedAt:        time.Now(),
		}
	}
	return out, nil
}

func rwaRow(code, issuer string, price *string, obs int64) timescale.AssetRow {
	return timescale.AssetRow{
		Slug:             code + "-" + issuer,
		AssetID:          code + "-" + issuer,
		Code:             code,
		IssuerGStrkey:    issuer,
		FirstSeenLedger:  55008233,
		LastSeenLedger:   63410221,
		ObservationCount: obs,
		PriceUSD:         price,
		// Comfortably above the dust-liquidity floor, so a suppressed
		// market cap in these tests is always the condition under test
		// and never an incidental dust verdict.
		Volume24hUSD: sptr("8214.55"),
	}
}

// rwaSupplyFor gives every row a circulating supply so the market-cap
// fill has both of its inputs. Membership never reads it — it is
// valuation, and valuation is decided after membership.
func rwaSupplyFor(rows map[string][]timescale.AssetRow) map[string]string {
	out := map[string]string{}
	for _, list := range rows {
		for _, r := range list {
			out[r.AssetID] = "12336218000000"
		}
	}
	return out
}

func rwaBound(code, issuer, domain, anchorType string) timescale.Sep1BoundCurrency {
	return timescale.Sep1BoundCurrency{
		Code:            code,
		Issuer:          issuer,
		HomeDomain:      domain,
		OrgName:         domain,
		Name:            code + " token",
		AnchorAsset:     "US Treasury Notes",
		AnchorAssetType: anchorType,
	}
}

// rwaServer builds a Server with the three seams the surface reads.
func rwaServer(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

func getRWA(t *testing.T, srv *v1.Server) v1.RWAAssetsView {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.RWAAssetsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

func rwaAssetIDs(v v1.RWAAssetsView) []string {
	out := make([]string, 0, len(v.Assets))
	for _, a := range v.Assets {
		out = append(out, a.AssetID)
	}
	return out
}

func recognisedIssuer(addr, name string) timescale.DirectoryEntry {
	return timescale.DirectoryEntry{
		Address: addr, Name: name, Domain: "etherfuse.com",
		Tags: []string{"issuer"}, Source: "stellar-expert",
	}
}

// rwaSkewedDirectory answers the membership build (single lookups are
// unused by it; the batch it makes is the FIRST batch) and the
// per-row overlay (every later batch) from different entries, so a test
// can reproduce a tag landing between the two.
type rwaSkewedDirectory struct {
	membership timescale.DirectoryEntry
	rowFill    timescale.DirectoryEntry
	batches    int
}

func (d *rwaSkewedDirectory) DirectoryEntryByAddress(
	_ context.Context, address string,
) (timescale.DirectoryEntry, bool, error) {
	if address != rwaGoodIssuer {
		return timescale.DirectoryEntry{}, false, nil
	}
	return d.rowFill, true, nil
}

func (d *rwaSkewedDirectory) DirectoryEntriesByAddresses(
	_ context.Context, addresses []string,
) (map[string]timescale.DirectoryEntry, error) {
	d.batches++
	e := d.rowFill
	if d.batches == 1 {
		e = d.membership
	}
	out := map[string]timescale.DirectoryEntry{}
	for _, a := range addresses {
		if a == rwaGoodIssuer {
			out[a] = e
		}
	}
	return out, nil
}

// errRWAScan stands in for the attestation read failing.
var errRWAScan = errors.New("issuers scan unavailable")

// rwaFunnelStages indexes a served funnel by stage name.
func rwaFunnelStages(t *testing.T, v v1.RWAAssetsView) map[string]v1.RWAFunnelStage {
	t.Helper()
	out := map[string]v1.RWAFunnelStage{}
	for _, s := range v.Funnel.Stages {
		if _, dup := out[s.Stage]; dup {
			t.Fatalf("stage %q served twice — a funnel with a repeated stage cannot be reconciled", s.Stage)
		}
		out[s.Stage] = s
	}
	return out
}

// rwaDropCount reads one drop off one stage.
func rwaDropCount(stages map[string]v1.RWAFunnelStage, stage, reason string) int {
	for _, d := range stages[stage].Dropped {
		if d.Reason == reason {
			return d.Count
		}
	}
	return 0
}

// checkFunnelArithmetic re-derives the narrowing INDEPENDENTLY of the
// production balance check, so `balanced: true` is never self-certifying.
// Adjacent stages counted in the same unit must satisfy
// count - sum(dropped) == next.count.
func checkFunnelArithmetic(t *testing.T, v v1.RWAAssetsView) {
	t.Helper()
	st := v.Funnel.Stages
	// Stages that COUNT a population an earlier stage already dropped
	// and that nothing follows from. They take no part in the
	// narrowing, so no subtraction relates them to their neighbours on
	// either side. Re-derived here from the wire rather than read out
	// of production, so this check stays independent of the code it is
	// checking.
	terminal := map[string]bool{
		"directory_recognised_issuing_accounts":     true,
		"listing_contracts_without_curated_binding": true,
	}
	for i := 0; i+1 < len(st); i++ {
		cur, next := st[i], st[i+1]
		// An ARM BOUNDARY. The arms narrow different populations from
		// different roots and meet only at the served set, so the last
		// stage of one arm has no arithmetic relation to the first
		// stage of the next — and must carry no drops, since a drop
		// across it could not be accounted for anywhere.
		if cur.Arm != next.Arm {
			if len(cur.Dropped) > 0 {
				t.Errorf("stage %q is the last of the %q arm and drops %d reasons — "+
					"a drop across an arm boundary cannot be reconciled", cur.Stage, cur.Arm, len(cur.Dropped))
			}
			continue
		}
		if terminal[cur.Stage] || terminal[next.Stage] {
			if terminal[cur.Stage] && len(cur.Dropped) > 0 {
				t.Errorf("terminal census stage %q carries drops — it takes no part in the narrowing", cur.Stage)
			}
			continue
		}
		// The one place a subtraction means nothing: issuer accounts to
		// the declarations they publish. That pair must carry no drops.
		// Every other unit change is a relabelling of a population that
		// maps one-to-one, so it still has to reconcile.
		if cur.Unit == "issuer_accounts" && next.Unit != "issuer_accounts" {
			if len(cur.Dropped) > 0 {
				t.Errorf("stage %q is the last counted in issuer accounts and drops %d reasons — "+
					"a drop across that change of unit cannot be reconciled", cur.Stage, len(cur.Dropped))
			}
			continue
		}
		sum := 0
		for _, d := range cur.Dropped {
			// issuer_asset_page_truncated counts ISSUERS whose asset
			// tail went unread, not assets; it is a signal, not a term
			// in the asset arithmetic.
			if d.Reason == "issuer_asset_page_truncated" {
				continue
			}
			if d.Actor == "" {
				t.Errorf("stage %q drop %q carries no actor — a reader cannot tell a coverage gap from a refusal",
					cur.Stage, d.Reason)
			}
			sum += d.Count
		}
		if cur.Count-sum != next.Count {
			t.Errorf("funnel does not close: %s %d less %d dropped is %d, but %s is %d",
				cur.Stage, cur.Count, sum, cur.Count-sum, next.Stage, next.Count)
		}
	}
	if !v.Funnel.Balanced {
		t.Errorf("funnel.balanced = false for an accounting that adds up: %+v", v.Funnel.Stages)
	}
	if v.Funnel.Basis == "" {
		t.Error("funnel.basis is empty — the units change down the funnel and nothing else says so")
	}
}

// rwaServerWithUpstream is rwaServer with the population UPSTREAM of the
// bound entries stated: the issuer rows and declarations a real
// deployment holds, which no fixture of bound entries can express.
func rwaServerWithUpstream(
	t *testing.T,
	upstream timescale.Sep1BoundCensus,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound, upstream: upstream},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

// itoaRWA is a tiny int-to-string so the filler codes above stay
// distinct without pulling strconv into the fixture's reading.
func itoaRWA(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// rwaOracleStub answers the oracle-stream read and counts the calls, so
// a test can prove the scan stays off the per-request path.
type rwaOracleStub struct {
	*stubOracleReader
	streams []canonical.OracleUpdate
	err     error
	calls   int
}

func (r *rwaOracleStub) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.streams, nil
}

// rwaOracleRow builds one oracle observation of an instrument.
func rwaOracleRow(t *testing.T, source, assetID, quoteID, raw string, decimals uint8) canonical.OracleUpdate {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", assetID, err)
	}
	q, err := canonical.ParseAsset(quoteID)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", quoteID, err)
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		t.Fatalf("bad raw price %q", raw)
	}
	return canonical.OracleUpdate{
		Source: source, Timestamp: time.Now().Add(-time.Minute),
		Asset: a, Quote: q, Price: canonical.NewAmount(n), Decimals: decimals,
	}
}

func rwaOracle(t *testing.T, rows ...canonical.OracleUpdate) *rwaOracleStub {
	t.Helper()
	return &rwaOracleStub{stubOracleReader: &stubOracleReader{}, streams: rows}
}

// rwaServerWithOracle is [rwaServer] plus the oracle seam the reference
// is read through.
func rwaServerWithOracle(
	t *testing.T,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
	oracle *rwaOracleStub,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound},
		Directory: &stubDirectoryReader{entries: dir},
		Oracle:    oracle,
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

// rwaOtherRecognisedIssuer is a SECOND directory-recognised issuer that
// also publishes a domain-bound SEP-1 entry for a code an oracle prices.
// Nothing about it is exotic: asset codes are not unique on Stellar, and
// the network holds many accounts issuing tokens called USTRY, BENJI or
// XAU. It exists here because a code-keyed join cannot tell it apart
// from the issuer whose instrument the feed actually tracks.
const rwaOtherRecognisedIssuer = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"

// rwaCountingListStub counts the per-issuer listing reads /v1/rwa/assets
// makes, so a test can tie the charge to the work and prove a denial
// stopped the reads.
type rwaCountingListStub struct {
	*rwaListStub
	lists atomic.Int64
}

func (l *rwaCountingListStub) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	l.lists.Add(1)
	return l.rwaListStub.ListAssetsExt(ctx, opts)
}

// newRWALimitedServer wires /v1/rwa/assets with two admitted classic
// member issuers behind the production limiter over a Redis-backed
// anonymous bucket of anonLimit tokens.
func newRWALimitedServer(t *testing.T, anonLimit int) (*testServerImpl, *rwaCountingListStub) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		rwaOndoIssuer: {rwaRow("USDY", rwaOndoIssuer, sptr("1.0923"), 90211)},
	}
	reader := &rwaCountingListStub{rwaListStub: &rwaListStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		byIssuer:            rows,
		supply:              rwaSupplyFor(rows),
	}}
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("USDY", rwaOndoIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
			rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
			rwaOndoIssuer: recognisedIssuer(rwaOndoIssuer, "Ondo"),
		}},
		AssetsReader: reader,
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, anonLimit, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
	})
	return startHTTPTest(t, srv.Handler()), reader
}

// rwaRemaining reads the post-request remainder, through the no-store
// probe when the response is shared-cacheable and so carries none.
func rwaRemaining(t *testing.T, ts *testServerImpl, resp *http.Response) int {
	t.Helper()
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
		n, err := strconv.Atoi(got)
		if err != nil {
			t.Fatalf("X-RateLimit-Remaining %q: %v", got, err)
		}
		return n
	}
	return remainingBeforeProbe(t, resp, ts.URL+assetsProbe)
}
