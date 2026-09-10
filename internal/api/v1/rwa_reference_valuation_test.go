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
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

	// The breakdowns carry the same total on the market basis, and the
	// reference basis did not move a row between groups either.
	if len(withOracle.ByClass) != len(without.ByClass) {
		t.Fatalf("by_class rows = %d vs %d", len(withOracle.ByClass), len(without.ByClass))
	}
	for i := range withOracle.ByClass {
		a, b := withOracle.ByClass[i], without.ByClass[i]
		if a.Class != b.Class || optString(a.MarketCapUSD) != optString(b.MarketCapUSD) ||
			a.Assets != b.Assets || a.AssetsUnvalued != b.AssetsUnvalued {
			t.Errorf("by_class[%d] market basis moved: %+v vs %+v", i, a, b)
		}
	}
	if len(withOracle.ByIssuer) != len(without.ByIssuer) {
		t.Fatalf("by_issuer rows = %d vs %d", len(withOracle.ByIssuer), len(without.ByIssuer))
	}
	for i := range withOracle.ByIssuer {
		a, b := withOracle.ByIssuer[i], without.ByIssuer[i]
		if a.Issuer != b.Issuer || optString(a.MarketCapUSD) != optString(b.MarketCapUSD) ||
			a.Assets != b.Assets || a.AssetsUnvalued != b.AssetsUnvalued {
			t.Errorf("by_issuer[%d] market basis moved: %+v vs %+v", i, a, b)
		}
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

	sum := newRat()
	rows := 0
	for _, a := range v.Assets {
		if a.ReferenceValuation.ValueUSD == nil {
			continue
		}
		addDecimal(t, sum, *a.ReferenceValuation.ValueUSD)
		rows++
	}
	if rows != v.Summary.ReferenceValuation.AssetsValued {
		t.Errorf("%d rows carry a figure but assets_valued = %d", rows, v.Summary.ReferenceValuation.AssetsValued)
	}
	if got, want := sum.FloatString(2), optString(v.Summary.ReferenceValuation.ValueUSD); got != want {
		t.Errorf("rows sum to %s but the total says %s", got, want)
	}

	// The per-issuer and per-class breakdowns are the same sum, split.
	perIssuer := newRat()
	for _, i := range v.ByIssuer {
		if i.ReferenceValueUSD != nil {
			addDecimal(t, perIssuer, *i.ReferenceValueUSD)
		}
	}
	if got, want := perIssuer.FloatString(2), optString(v.Summary.ReferenceValuation.ValueUSD); got != want {
		t.Errorf("by_issuer reference totals sum to %s but the summary says %s", got, want)
	}
	perClass := newRat()
	for _, c := range v.ByClass {
		if c.ReferenceValueUSD != nil {
			addDecimal(t, perClass, *c.ReferenceValueUSD)
		}
	}
	if got, want := perClass.FloatString(2), optString(v.Summary.ReferenceValuation.ValueUSD); got != want {
		t.Errorf("by_class reference totals sum to %s but the summary says %s", got, want)
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
		if a.Decimals != 7 {
			t.Errorf("%s: decimals = %d, want 7 for a classic asset", a.Code, a.Decimals)
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
	a := v.Assets[0]
	if a.ReferenceValuation.Status != v1.RWAPremiumIssuerFlagged {
		t.Errorf("reference_valuation.status = %q, want %q",
			a.ReferenceValuation.Status, v1.RWAPremiumIssuerFlagged)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a flagged issuer was handed a real instrument's valuation times its own float: %s",
			*a.ReferenceValuation.ValueUSD)
	}
	if a.Reference != nil {
		t.Errorf("a reference block reached a flagged issuer: %+v", a.Reference)
	}
	if v.Summary.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a withheld row reached the reference total: %s", *v.Summary.ReferenceValuation.ValueUSD)
	}
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

	drops := map[string]v1.RWAFunnelDrop{}
	for _, d := range stages[0].Dropped {
		drops[d.Reason] = d
	}
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
	// And with no oracle at all every served row is dropped under the
	// outage, which is a different statement from a network whose
	// instruments nobody prices.
	stages := rwaValuationArm(t, without)
	if stages[1].Count != 0 {
		t.Errorf("reference-valued = %d with no oracle wired, want 0", stages[1].Count)
	}
	if len(stages[0].Dropped) != 1 || stages[0].Dropped[0].Reason != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("drops = %+v, want every row under %q", stages[0].Dropped, v1.RWAPremiumReferenceUnavailable)
	}
	if stages[0].Dropped[0].Actor != "operator" {
		t.Errorf("an outage was attributed to %q, not the operator who can fix it", stages[0].Dropped[0].Actor)
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
	if a.ReferenceValuation.Status != v1.RWAPremiumContractNotBound {
		t.Errorf("reference_valuation.status = %q, want %q",
			a.ReferenceValuation.Status, v1.RWAPremiumContractNotBound)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a contract token was valued through its own declared symbol: %q", *a.ReferenceValuation.ValueUSD)
	}
	if a.Reference != nil {
		t.Errorf("a reference reached a contract row: %+v", a.Reference)
	}
	if v.Summary.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a contract row reached the reference total: %q", *v.Summary.ReferenceValuation.ValueUSD)
	}

	// The market basis is untouched by any of that: the row still
	// carries the market cap the pipeline computed for it, at the
	// contract's REAL decimals.
	if a.Decimals != 6 {
		t.Errorf("decimals = %d, want the contract's declared 6 — a 7 here is a tenth of the real figure", a.Decimals)
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
	actors := map[string]string{}
	for _, d := range stages[0].Dropped {
		actors[d.Reason] = d.Actor
	}
	if got := actors[v1.RWAReferenceValuationNoSupply]; got != "operator" {
		t.Errorf("a missing supply reading was attributed to %q, want operator — it is a gap here, not a refusal", got)
	}
	if got := actors[v1.RWAPremiumNotInstrumentScoped]; got != "definition" {
		t.Errorf("an ounce-priced feed was attributed to %q, want definition — nobody can act on it", got)
	}
	if actors[v1.RWAReferenceValuationNoSupply] == actors[v1.RWAPremiumNotInstrumentScoped] {
		t.Error("a coverage gap and a refusal carry the same actor; the field distinguishes nothing")
	}
	if !v.Funnel.Balanced {
		t.Errorf("funnel unbalanced: %+v", v.Funnel.Stages)
	}
}
