package v1

import (
	"context"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Unit tests for the four rules deciding whether a premium may be published. Each pins the refusal and asserts no figure
// accompanied it.

func refUpdate(t *testing.T, source, assetID, quoteID, raw string, decimals uint8, ts time.Time) canonical.OracleUpdate {
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
		Source:    source,
		Timestamp: ts,
		Asset:     a,
		Quote:     q,
		Price:     canonical.NewAmount(n),
		Decimals:  decimals,
	}
}

// boundIssuer is the G-address internal/rwa binds the CETES / USTRY /
// TESOURO instruments to. Spelled out here because the binding table is unexported by design:
// these tests exercise the API's use of the binding, not the table's contents.
const boundIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"

// unboundIssuer is a different account. Nothing binds it to any feed,
// which is the ordinary case for all but a handful of Stellar accounts.
const unboundIssuer = "GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"

// admittedRow builds a row that has already passed membership, under the
// issuer the instrument is bound to. Tests that care about the binding
// itself use [admittedRowFor].
func admittedRow(code, price string) RWAAsset {
	return admittedRowFor(code, boundIssuer, price)
}

func admittedRowFor(code, issuer, price string) RWAAsset {
	a := RWAAsset{Code: code, Issuer: issuer, Valuation: RWAValuation{Status: RWAValuationUnpriced}}
	if price != "" {
		p := price
		a.Valuation = RWAValuation{Status: RWAValuationPublished, PriceUSD: &p, MarketCapUSD: &p}
	}
	return a
}

// TestRWAReference_RefusesANonDollarDenominatedFeed is rule R-A: a bare `_FUNDAMENTAL` feed publishes NAV in the token's
// reserve asset, a ratio not dollars. The refusal must state the denomination problem and carry no number.
func TestRWAReference_RefusesANonDollarDenominatedFeed(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		// 1.00295305 — a NAV RATIO against the reserve asset, not $1.
		refUpdate(t, "redstone", "rwa:USTRY", "crypto:BTC", "100295305", 8, now),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("reference published from a %s-denominated feed: %+v — its value is a ratio, not dollars",
			"crypto:BTC", a.Reference)
	}
	if a.Premium.Status != RWAPremiumReferenceNotUSD {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumReferenceNotUSD)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent — a refused comparison is not a comparison of zero", *a.Premium.Pct)
	}
}

// TestRWAReference_RefusesAnOffChainQuantity is rule R-B: `rwa:XAU` is spot gold per troy ounce while a token coded XAU
// has unstated size, so their ratio is a unit conversion that would read as a near-total discount.
func TestRWAReference_RefusesAnOffChainQuantity(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "reflector-fx", "rwa:XAU", "fiat:USD", "440086022830869146", 14, now),
	})
	a := admittedRow("XAU", "0.5000")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("spot gold per troy ounce published as a token reference: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNotInstrumentScoped {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNotInstrumentScoped)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a unit conversion must never be served as a discount", *a.Premium.Pct)
	}
}

// TestRWAReference_RefusesANonOracleSource is rule R-C. Aggregators and
// the authority-sanity feeds write into the same hypertable for divergence comparison. A premium
// measured against an aggregator's own read of the market would compare the market with itself.
func TestRWAReference_RefusesANonOracleSource(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "coingecko", "rwa:USTRY", "fiat:USD", "104000000", 8, now),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("an aggregator row was served as an independent oracle reference: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNoReference {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNoReference)
	}
}

// TestRWAReference_RefusesADeclaredPegPrice is rule R-D: a price_basis price is the issuer's declared peg or a market
// hop, so comparing it to an independent valuation reports the issuer's own claim as a market finding.
func TestRWAReference_RefusesADeclaredPegPrice(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRow("USTRY", "1.0000")
	a.Valuation.PriceBasis = priceBasisDeclaredPeg
	rwaApplyReference(&a, snap, nil, nil, now)

	// The reference itself is real and is still published — it is the
	// COMPARISON that is refused.
	if a.Reference == nil {
		t.Fatal("the oracle reference is independent of how the market price was derived and must still be served")
	}
	if a.Premium.Status != RWAPremiumMarketNotObserved {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumMarketNotObserved)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a declared peg is not a market observation", *a.Premium.Pct)
	}
}

// TestRWAReference_PublishesTheGapExactly checks the arithmetic and its
// sign on a market trading ABOVE and BELOW the instrument's independent
// valuation, in exact rational arithmetic (ADR-0003).
func TestRWAReference_PublishesTheGapExactly(t *testing.T) {
	now := time.Now()
	// Reference 1.07403800, the live USTRY figure at 8 decimal places.
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})

	for _, tc := range []struct {
		name, market, want string
	}{
		// 1.128740 / 1.074038 − 1 = +5.0931…%
		{"premium", "1.12874000", "5.0931"},
		// 1.020336 / 1.074038 − 1 = −5.0000%
		{"discount", "1.02033610", "-5.0000"},
		{"at par", "1.07403800", "0.0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := admittedRow("USTRY", tc.market)
			rwaApplyReference(&a, snap, nil, nil, now)
			if a.Premium.Status != RWAPremiumPublished {
				t.Fatalf("premium status = %q, want published", a.Premium.Status)
			}
			if a.Premium.Pct == nil || *a.Premium.Pct != tc.want {
				got := "<nil>"
				if a.Premium.Pct != nil {
					got = *a.Premium.Pct
				}
				t.Errorf("premium pct = %s, want %s", got, tc.want)
			}
			if a.Reference == nil || a.Reference.PriceUSD != "1.07403800" {
				t.Errorf("reference not served verbatim at the feed's own scale: %+v", a.Reference)
			}
			if a.Reference != nil && a.Reference.Quote != "fiat:USD" {
				t.Errorf("reference quote = %q — the denominator travels with the figure", a.Reference.Quote)
			}
			if a.Reference != nil && a.Reference.Feed != "rwa:USTRY" {
				t.Errorf("reference feed = %q, want the canonical instrument id", a.Reference.Feed)
			}
		})
	}
}

// TestRWAReference_UnpricedAssetKeepsTheReference: an instrument no Stellar market prices keeps its independent
// valuation, and a zero premium must not stand in for a comparison never made.
func TestRWAReference_UnpricedAssetKeepsTheReference(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:TESOURO", "fiat:USD", "24538100", 8, now),
	})
	a := admittedRow("TESOURO", "") // unpriced
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference == nil || a.Reference.PriceUSD != "0.24538100" {
		t.Fatalf("reference = %+v, want the oracle figure served even with no market price", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNoMarketPrice {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNoMarketPrice)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent", *a.Premium.Pct)
	}
}

// TestRWAReference_FlaggedIssuerGetsNoIndependentValuationEither: scam-flag suppression must also withhold a real
// instrument's oracle NAV, which would put a dollar figure on an impersonator.
func TestRWAReference_FlaggedIssuerGetsNoIndependentValuationEither(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := RWAAsset{Code: "USTRY", Valuation: RWAValuation{Status: RWAValuationIssuerFlagged}}
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("a flagged issuer's token was given an oracle valuation: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumIssuerFlagged {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumIssuerFlagged)
	}
}

// TestRWAReference_StaleReferenceIsLabelledNotHidden. A NAV struck last
// Friday IS the current NAV on Monday, so the surface labels age rather
// than withholding the only independent valuation it has.
func TestRWAReference_StaleReferenceIsLabelledNotHidden(t *testing.T) {
	now := time.Now()
	old := now.Add(-5 * 24 * time.Hour)
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, old),
	})
	a := admittedRow("USTRY", "1.07403800")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference == nil {
		t.Fatal("a five-day-old reference must be served, labelled")
	}
	if !a.Reference.Stale {
		t.Error("reference.stale = false on a five-day-old figure")
	}
	if a.Premium.Status != RWAPremiumPublished {
		t.Errorf("premium status = %q — age labels the figure, it does not withhold it", a.Premium.Status)
	}

	fresh := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now.Add(-time.Hour)),
	})
	b := admittedRow("USTRY", "1.07403800")
	rwaApplyReference(&b, fresh, nil, nil, now)
	if b.Reference == nil || b.Reference.Stale {
		t.Errorf("an hour-old reference must not be marked stale: %+v", b.Reference)
	}
}

// TestRWAReference_PicksTheMostRecentDeterministically. Several oracles
// may price one instrument. The served figure must not depend on the
// order the scan happened to return rows.
func TestRWAReference_PicksTheMostRecentDeterministically(t *testing.T) {
	now := time.Now()
	older := refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "100000000", 8, now.Add(-2*time.Hour))
	newer := refUpdate(t, "band", "rwa:USTRY", "fiat:USD", "107403800", 8, now.Add(-time.Minute))

	for _, order := range [][]canonical.OracleUpdate{{older, newer}, {newer, older}} {
		snap := rwaReferenceSnapshotFrom(order)
		got := snap.byFeed["USTRY"]
		if got.source != "band" || got.wire != "1.07403800" {
			t.Errorf("picked %s/%s, want the most recent (band/1.07403800)", got.source, got.wire)
		}
	}
}

// TestRWAReference_UnavailableSnapshotStatesItsOwnStatus: a read that did not answer must not report "no oracle
// publishes a valuation"; the distinct status keeps an outage distinguishable from absence on the wire.
func TestRWAReference_UnavailableSnapshotPublishesNothing(t *testing.T) {
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, rwaReferences{}, nil, nil, time.Now())
	if a.Reference != nil {
		t.Fatalf("reference served from an unavailable snapshot: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumReferenceUnavailable {
		t.Errorf("premium status = %q, want %q — a failed read must not be reported as an absence",
			a.Premium.Status, RWAPremiumReferenceUnavailable)
	}
	if a.Premium.Status == RWAPremiumNoReference {
		t.Error("an outage is being reported as 'no oracle publishes this instrument'")
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q on an unavailable snapshot", *a.Premium.Pct)
	}

	// And the genuine absence keeps its own, different status: a bound
	// pair whose feed the oracles simply are not publishing.
	b := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&b, rwaReferenceSnapshotFrom(nil), nil, nil, time.Now())
	if b.Premium.Status != RWAPremiumNoReference {
		t.Errorf("an empty but SUCCESSFUL read gave status %q, want %q — the two must be distinguishable",
			b.Premium.Status, RWAPremiumNoReference)
	}
}

// TestRWAReference_CarriedForwardRowsStillExpire: the seven-day age bound is enforced on the observation, not the
// snapshot, so rows carried across a failed read still age out under a sustained outage.
func TestRWAReference_CarriedForwardRowsStillExpire(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now.Add(-8*24*time.Hour)),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("an eight-day-old observation was served: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumReferenceExpired {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumReferenceExpired)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q against an expired observation", *a.Premium.Pct)
	}

	// Six days is inside the window: stale, labelled, still served.
	fresh := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now.Add(-6*24*time.Hour)),
	})
	b := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&b, fresh, nil, nil, now)
	if b.Reference == nil || !b.Reference.Stale {
		t.Errorf("a six-day-old observation must be served and labelled stale: %+v", b.Reference)
	}
}

// TestRWAReference_RefusesAnUnboundIssuer is R-0, the identity rule: asset codes are not unique on Stellar, and a code-
// only join would give every issuer of a token called USTRY the real instrument's NAV.
func TestRWAReference_RefusesAnUnboundIssuer(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRowFor("USTRY", unboundIssuer, "0.20000000")
	rwaApplyReference(&a, snap, nil, nil, now)

	if a.Reference != nil {
		t.Fatalf("an unbound issuer's token was given the instrument's valuation: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNotBound)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q — a false claim about a real security", *a.Premium.Pct)
	}

	// The bound issuer's token, same code, same snapshot, is answered.
	b := admittedRowFor("USTRY", boundIssuer, "1.07403800")
	rwaApplyReference(&b, snap, nil, nil, now)
	if b.Premium.Status != RWAPremiumPublished {
		t.Errorf("the bound pair must still be compared; status = %q", b.Premium.Status)
	}
}

// TestRWAReference_DoesNotFoldCase. The join upper-cased both sides, so
// a token coded XAUM matched the XAUm feed. A case variant is a
// different token unless a binding says otherwise.
func TestRWAReference_DoesNotFoldCase(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRowFor("ustry", boundIssuer, "0.20000000")
	rwaApplyReference(&a, snap, nil, nil, now)
	if a.Reference != nil {
		t.Fatalf("a case variant was answered with the bound instrument's valuation: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNotBound {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNotBound)
	}
}

// TestRWAReference_ZeroReferenceIsNotDividedBy. A non-positive oracle
// value cannot be a denominator, and the refusal must be stated rather
// than producing an infinity or a panic.
func TestRWAReference_ZeroReferenceIsNotDividedBy(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "0", 8, now),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, nil, nil, now)
	if a.Premium.Status != RWAPremiumReferenceNotPositive {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumReferenceNotPositive)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent", *a.Premium.Pct)
	}
}

// TestRWAReference_OneRefusalIsReportedIdentically pins that the row and the funnel (which reads only
// `reference_valuation.status`) tell one story: premium and reference valuation carry the same reason. Every refusal
// path is driven so a path that stopped firing cannot pass trivially.
func TestRWAReference_OneRefusalIsReportedIdentically(t *testing.T) {
	now := time.Now()
	live := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	cases := []struct {
		name string
		row  func() RWAAsset
		snap rwaReferences
		want string
	}{
		{
			"a flagged issuer",
			func() RWAAsset {
				return RWAAsset{Code: "USTRY", Issuer: boundIssuer, Valuation: RWAValuation{Status: RWAValuationIssuerFlagged}}
			},
			live, RWAPremiumIssuerFlagged,
		},
		{
			"a read that did not answer",
			func() RWAAsset { return admittedRow("USTRY", "1.04") },
			rwaReferences{},
			RWAPremiumReferenceUnavailable,
		},
		{
			"a contract-issued member",
			func() RWAAsset {
				a := admittedRowFor("", "", "1.04")
				a.ContractID = rwaTestContractID
				return a
			},
			live, RWAPremiumContractNotBound,
		},
		{
			"a feed that prices an ounce",
			func() RWAAsset { return admittedRowFor("XAU", unboundIssuer, "4115.67") },
			live, RWAPremiumNotInstrumentScoped,
		},
		{
			"a pair nothing binds",
			func() RWAAsset { return admittedRowFor("USTRY", unboundIssuer, "1.04") },
			live, RWAPremiumNotBound,
		},
		{
			"a feed quoted in a reserve asset",
			func() RWAAsset { return admittedRow("USTRY", "1.04") },
			rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
				refUpdate(t, "redstone", "rwa:USTRY", "crypto:BTC", "100295305", 8, now),
			}),
			RWAPremiumReferenceNotUSD,
		},
		{
			"a stream carrying no such feed",
			func() RWAAsset { return admittedRow("USTRY", "1.04") },
			rwaReferenceSnapshotFrom(nil), RWAPremiumNoReference,
		},
		{
			"an observation past the outer bound",
			func() RWAAsset { return admittedRow("USTRY", "1.04") },
			rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
				refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8,
					now.Add(-rwaReferenceMaxAge-time.Hour)),
			}),
			RWAPremiumReferenceExpired,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := c.row()
			rwaApplyReference(&a, c.snap, nil, nil, now)
			if a.Premium.Status != c.want {
				t.Fatalf("premium.status = %q, want %q — this path no longer reaches the refusal under test",
					a.Premium.Status, c.want)
			}
			if a.ReferenceValuation.Status != a.Premium.Status {
				t.Errorf("premium.status = %q but reference_valuation.status = %q — one event, two accounts of it",
					a.Premium.Status, a.ReferenceValuation.Status)
			}
			if a.ReferenceValuation.ValueUSD != nil {
				t.Errorf("a refused row carries a figure: %q", *a.ReferenceValuation.ValueUSD)
			}
		})
	}
}

// A curated row's reference is a curator's own uploaded price, not a listing platform's
// aggregate, so its premium refusal must say that rather than the listing-price wording
// rwaApplyListingReference defaults to. Premium.Status must match Reference.Provenance.
func TestRWAApplyCuratorReference_PremiumStatusMatchesCuratorProvenance(t *testing.T) {
	now := time.Now().UTC()
	supply := "100000000000"
	a := &RWAAsset{
		AssetID:           "CURATED-ADDR",
		CirculatingSupply: &supply,
		Decimals:          intPtr(7),
	}
	entry := timescale.CuratedRWAEntry{
		Address:  "CURATED-ADDR",
		Company:  "Example Fund",
		PriceUSD: "1.02",
		PricedAt: now.Add(-time.Hour),
		Source:   "curator upload",
	}

	rwaApplyCuratorReference(a, entry, now)

	if a.Reference == nil || a.Reference.Provenance != RWAReferenceCuratorPrice {
		t.Fatalf("reference = %+v, want provenance %q", a.Reference, RWAReferenceCuratorPrice)
	}
	if a.Premium.Status != RWAPremiumReferenceNotOracleCurator {
		t.Errorf("premium.status = %q, want %q (the curator-price refusal, matching reference.provenance)",
			a.Premium.Status, RWAPremiumReferenceNotOracleCurator)
	}
	if a.Premium.Status == RWAPremiumReferenceNotOracle {
		t.Errorf("premium.status = %q — the listing-price refusal, wrongly claiming this reference is a listing price", a.Premium.Status)
	}
}

// The summary's `provenances` enum is what a verified row can carry into the total; curated rows are served apart, so
// the curator provenance is on the per-row enum only. Both spec enums are pinned to the Go vocabulary.
func TestRWAReferenceProvenance_SpecEnumsAreTheGoVocabulary(t *testing.T) {
	summary := make([]string, 0, len(rwaReferenceProvenanceProseOrder))
	for _, p := range rwaReferenceProvenanceProseOrder {
		summary = append(summary, p.provenance)
	}
	row := append(append([]string{}, summary...), RWAReferenceCuratorPrice)

	doc := loadSpecDoc(t)
	gotSummary := specEnumAt(t, doc, "components", "schemas", "RWAReferenceSummary", "properties", "provenances", "items")
	gotRow := specEnumAt(t, doc, "components", "schemas", "RWAReference", "properties", "provenance")
	sort.Strings(summary)
	sort.Strings(row)
	if strings.Join(gotSummary, ",") != strings.Join(summary, ",") {
		t.Errorf("openapi RWAReferenceSummary.provenances enum = %v, Go vocabulary = %v; edit both and regenerate", gotSummary, summary)
	}
	if strings.Join(gotRow, ",") != strings.Join(row, ",") {
		t.Errorf("openapi RWAReference.provenance enum = %v, Go vocabulary = %v; edit both and regenerate", gotRow, row)
	}
}

// panicOnceOracle panics on its first LatestOracleStreams call and
// answers normally afterward, so a test can drive the exact failure
// a read that blows up mid-fill.
type panicOnceOracle struct {
	calls   int
	streams []canonical.OracleUpdate
}

func (o *panicOnceOracle) LatestOracleUpdatesForAsset(context.Context, canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *panicOnceOracle) LatestOracleUpdatesForAssets(context.Context, []canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *panicOnceOracle) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	o.calls++
	if o.calls == 1 {
		panic("boom")
	}
	return o.streams, nil
}

// TestCachedRWAReferences_PanicDoesNotWedgeTheFlight pins that an oracle-read panic must not leave rwaRefFlight latched
// on a channel nobody closes; the outer recover simulates net/http's per-request recovery.
func TestCachedRWAReferences_PanicDoesNotWedgeTheFlight(t *testing.T) {
	oracle := &panicOnceOracle{
		streams: []canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "104000000", 8, time.Now()),
		},
	}
	s := &Server{Options: Options{Oracle: oracle}, logger: slog.Default()}

	func() {
		defer func() { _ = recover() }()
		s.cachedRWAReferences(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := s.cachedRWAReferences(ctx)
	if !got.available {
		t.Fatalf("second call after a panicked read got no reference: the single-flight marker is wedged (GH-587)")
	}
}

// rwaSummaryReachableProvenances is every provenance a view.Assets row can carry into RWAReferenceSummary; the curator
// provenance is absent because curated rows live in CuratedAssets and never enter the verified total.
var rwaSummaryReachableProvenances = []string{
	RWAReferenceOracleNAV,
	RWAReferenceFundNAV,
	RWAReferenceListingPrice,
	RWAReferenceProspectusCNAV,
}

// A prospectus-CNAV row contributes to the reference total, so the
// summary's basis has to name that claim, not stay silent about it or
// describe the total by the other kinds only.
func TestRWASummaryProvenance_NamesProspectusCNAV(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	supply := "566742721191613"
	a := RWAAsset{
		AssetID: "gBENJI-GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP", Code: "gBENJI",
		Issuer: "GD5J73EKK5IYL5XS3FBTHHX7CZIYRP7QXDL57XFWGC2WVYWT326OBXRP", CirculatingSupply: &supply, Decimals: intPtr(7),
	}
	snap := rwaReferences{available: true, byFeed: map[string]rwaReference{}, nonUSD: map[string]string{}}
	rwaApplyReference(&a, snap, nil, map[string]timescale.ListingEntry{}, now)
	if a.Reference == nil || a.Reference.Provenance != RWAReferenceProspectusCNAV || a.ReferenceValuation.ValueUSD == nil {
		t.Fatalf("fixture did not produce a valued CNAV row: ref=%+v val=%+v", a.Reference, a.ReferenceValuation)
	}

	s := rwaSummariseReference([]RWAAsset{a})
	if strings.Join(s.Provenances, ",") != RWAReferenceProspectusCNAV {
		t.Fatalf("provenances = %v, want [%s]", s.Provenances, RWAReferenceProspectusCNAV)
	}
	if !strings.Contains(s.Basis, "`provenance: prospectus_constant_nav`") {
		t.Errorf("CNAV-only basis does not describe the prospectus NAV claim:\n%s", s.Basis)
	}
	if strings.Contains(s.Basis, "`provenance: oracle_instrument_nav`") || strings.Contains(s.Basis, "`provenance: listing_platform_price`") {
		t.Errorf("CNAV-only basis describes a provenance the total does not contain:\n%s", s.Basis)
	}
}

// Every reachable provenance has its own prose, and any mixture names
// each member and says it is a mixture.
func TestRWASummaryProvenance_ProseDescribesEveryReachableProvenance(t *testing.T) {
	for _, p := range rwaSummaryReachableProvenances {
		got := rwaReferenceProvenanceProse([]string{p})
		if !strings.Contains(got, "`provenance: "+p+"`") {
			t.Errorf("prose for [%s] does not name it: %q", p, got)
		}
		if strings.Contains(got, "MIXES") {
			t.Errorf("single-provenance prose for [%s] claims a mixture: %q", p, got)
		}
	}
	mixed := rwaReferenceProvenanceProse([]string{RWAReferenceOracleNAV, RWAReferenceProspectusCNAV})
	for _, p := range []string{RWAReferenceOracleNAV, RWAReferenceProspectusCNAV} {
		if !strings.Contains(mixed, "`provenance: "+p+"`") {
			t.Errorf("oracle+CNAV prose omits %s: %q", p, mixed)
		}
	}
	if !strings.HasPrefix(mixed, "The total MIXES TWO KINDS OF CLAIM") {
		t.Errorf("oracle+CNAV prose does not state the mixture: %q", mixed)
	}
	three := rwaReferenceProvenanceProse([]string{RWAReferenceOracleNAV, RWAReferenceListingPrice, RWAReferenceProspectusCNAV})
	if !strings.HasPrefix(three, "The total MIXES THREE KINDS OF CLAIM") {
		t.Errorf("three-provenance prose does not state the mixture: %q", three)
	}
	all := rwaReferenceProvenanceProse(rwaSummaryReachableProvenances)
	if !strings.HasPrefix(all, "The total MIXES FOUR KINDS OF CLAIM") {
		t.Errorf("four-provenance prose does not state the mixture: %q", all)
	}
}

// A set mixing classic and contract-issued rows: by_issuer holds only the
// classic G-addresses — a contract row's empty Issuer is not a grouping key.
func TestRWAIdentity_ByIssuerSkipsContractRows(t *testing.T) {
	const (
		etherfuse = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		ondo      = "GAKE3L4PLQPE6BMUTPGXXDDEYRNQSMWQTMVZDSXTPXDOOMFDTVUOGCEX"
	)
	mcap := func(v string) RWAValuation { return RWAValuation{MarketCapUSD: &v} }
	assets := []RWAAsset{
		{AssetID: "USTRY-" + etherfuse, Code: "USTRY", Issuer: etherfuse, IssuerDirectoryName: "Etherfuse", HomeDomain: "etherfuse.com", Valuation: mcap("10.00")},
		{AssetID: "USDY-" + ondo, Code: "USDY", Issuer: ondo, IssuerDirectoryName: "Ondo", HomeDomain: "ondo.finance", Valuation: mcap("20.00")},
		{AssetID: "CA1", ContractID: "CA1", IssuerDirectoryName: "Entity One", IssuerDirectoryDomain: "one.example", Valuation: mcap("30.00")},
		{AssetID: "CA2", ContractID: "CA2", IssuerDirectoryName: "Entity Two", IssuerDirectoryDomain: "two.example", Valuation: mcap("40.00")},
	}

	got := rwaByIssuer(assets)
	if len(got) != 2 {
		t.Fatalf("by_issuer = %+v, want exactly the 2 classic issuers", got)
	}
	want := map[string]string{etherfuse: "10.00", ondo: "20.00"}
	for _, row := range got {
		if row.Issuer == "" {
			t.Errorf("by_issuer carries a blank issuer %+v merging contract rows of distinct entities", row)
			continue
		}
		if row.MarketCapUSD == nil || *row.MarketCapUSD != want[row.Issuer] || row.Assets != 1 {
			t.Errorf("by_issuer[%s] = %+v, want 1 asset at %s", row.Issuer, row, want[row.Issuer])
		}
	}
	if s := rwaSummarise(assets, rwaTruncation{}); s.Issuers != 4 || s.Assets != 4 {
		t.Errorf("summary assets/issuers = %d/%d, want 4/4 — each contract is its own issuer, never a shared blank one", s.Assets, s.Issuers)
	}
}

// A contract row's domain comes from the curated directory, not from an
// on-chain home_domain an attestation was fetched from, and it is served
// under the field that says so.
func TestRWAIdentity_ContractDirectoryDomainIsNotHomeDomain(t *testing.T) {
	const contract = "CBNG2BVXUXUM7OUJNU6SGW7KZPG7OBNRSSFA2DSVDNCOYBJI7SE5VBSO"
	rows := rwaContractAssetRows(
		[]rwaContractMember{{contractID: contract, basis: "sep1_anchor_declaration", dirName: "Entity", dirDomain: "entity.example"}},
		map[string]AssetDetail{contract: {AssetID: contract}},
	)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0].HomeDomain != "" {
		t.Errorf("home_domain = %q, want empty — no attestation was fetched for a contract", rows[0].HomeDomain)
	}
	if rows[0].IssuerDirectoryDomain != "entity.example" {
		t.Errorf("issuer_directory_domain = %q, want the directory's entity.example", rows[0].IssuerDirectoryDomain)
	}
}

// TestRwaAssetIssuerCount_ContractsAreNotOneSharedBlankBucket pins that rwaAssetIssuerCount (summary.issuers) counts each
// admitted contract as its own issuer, not one shared blank-Issuer map key.
func TestRwaAssetIssuerCount_ContractsAreNotOneSharedBlankBucket(t *testing.T) {
	assets := make([]RWAAsset, 0, 26)
	for i := 0; i < 21; i++ {
		assets = append(assets, RWAAsset{
			AssetID: "CODE-issuer",
			Issuer:  issuerAddr(i),
		})
	}
	for i := 0; i < 5; i++ {
		assets = append(assets, RWAAsset{
			AssetID:    "contract-asset",
			ContractID: contractAddr(i),
		})
	}

	got := rwaAssetIssuerCount(assets)
	const want = 26 // 21 distinct classic issuers + 5 distinct contracts
	if got != want {
		t.Fatalf("rwaAssetIssuerCount() = %d, want %d", got, want)
	}
}

// TestRwaIssuerCount_AgreesAcrossAssetsAndHistoryViews: the same membership (21 classic issuers, 5 contracts) must publish
// the same issuers count via rwaAssetIssuerCount and via rwaHistoryIssuerCount (/v1/rwa/history, /premium).
func TestRwaIssuerCount_AgreesAcrossAssetsAndHistoryViews(t *testing.T) {
	m := rwaMembership{}
	assets := make([]RWAAsset, 0, 26)
	for i := 0; i < 21; i++ {
		m.members = append(m.members, rwaMember{issuer: issuerAddr(i)})
		assets = append(assets, RWAAsset{Issuer: issuerAddr(i)})
	}
	for i := 0; i < 5; i++ {
		m.contracts = append(m.contracts, rwaContractMember{contractID: contractAddr(i)})
		assets = append(assets, RWAAsset{ContractID: contractAddr(i)})
	}

	fromAssets := rwaAssetIssuerCount(assets)
	fromHistory := rwaHistoryIssuerCount(m)
	if fromAssets != fromHistory {
		t.Fatalf("issuers disagree across endpoints for the same membership: assets=%d history/premium=%d", fromAssets, fromHistory)
	}
	if fromAssets != 26 {
		t.Fatalf("rwaAssetIssuerCount() = %d, want 26", fromAssets)
	}
}

func issuerAddr(i int) string {
	return "GISSUER" + string(rune('A'+i))
}

func contractAddr(i int) string {
	return "CCONTRACT" + string(rune('A'+i))
}

// TestRWASummarise_EveryCapMakesALowerBound: each rebuild cap, not only the issuer cap, makes a fully-valued set's total
// a lower bound, and the basis names the cap rather than calling the set complete.
func TestRWASummarise_EveryCapMakesALowerBound(t *testing.T) {
	cap100 := "100.00"
	assets := []RWAAsset{{
		AssetID:   "USTRY-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		Valuation: RWAValuation{Status: RWAValuationPublished, MarketCapUSD: &cap100},
	}}
	for _, tc := range []struct {
		name  string
		trunc rwaTruncation
		names string
	}{
		{"issuer cap", rwaTruncation{overIssuerCap: 2}, "issuer cap turned away 2 qualifying asset(s)"},
		{"contract scan cap", rwaTruncation{overContractCap: 5}, "contract scan cap left 5 recognised contract(s) unevaluated"},
		{"full issuer page", rwaTruncation{issuerPagesFull: 1}, "1 member issuer(s) have more classic assets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum := rwaSummarise(assets, tc.trunc)
			if sum.AssetsUnvalued != 0 || sum.MarketCapUSD == nil || *sum.MarketCapUSD != "100.00" {
				t.Fatalf("unvalued=%d cap=%v, want one valued row totalling 100.00", sum.AssetsUnvalued, sum.MarketCapUSD)
			}
			if !sum.LowerBound || !sum.Truncated {
				t.Errorf("lower_bound/truncated = %v/%v, want true/true", sum.LowerBound, sum.Truncated)
			}
			if strings.Contains(sum.Basis, "Every asset in the set publishes a valuation") {
				t.Errorf("basis claims a complete set: %q", sum.Basis)
			}
			if !strings.Contains(sum.Basis, tc.names) {
				t.Errorf("basis = %q, want it to name %q", sum.Basis, tc.names)
			}
		})
	}

	sum := rwaSummarise(assets, rwaTruncation{})
	if sum.LowerBound || sum.Truncated {
		t.Errorf("untruncated fully-valued set: lower_bound/truncated = %v/%v, want false/false", sum.LowerBound, sum.Truncated)
	}
	if !strings.Contains(sum.Basis, "Every asset in the set publishes a valuation") {
		t.Errorf("untruncated basis = %q", sum.Basis)
	}
}
