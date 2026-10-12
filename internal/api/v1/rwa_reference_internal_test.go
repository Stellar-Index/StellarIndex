package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
