package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Unit tests for the four rules that decide whether a premium may be
// published. Each one removes a way of serving a number that means
// something other than what it says, so each gets a test that pins the
// refusal AND asserts no figure came with it.

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
// TESOURO instruments to. Spelled out here rather than imported because
// the binding table is unexported by design: these tests exercise the
// API's use of the binding, not the table's contents (internal/rwa has
// its own tests for those).
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

// TestRWAReference_RefusesANonDollarDenominatedFeed is rule R-A, and it
// is the D8 failure in this surface's coordinates: a bare `_FUNDAMENTAL`
// feed publishes net asset value in the token's RESERVE asset, so its
// value is a ratio and not a dollar figure. Registering two such feeds
// as USD once served "a BTC-backed token is worth $1.00" for a token its
// own USD sibling priced at $78,313.
//
// The refusal must be stated as the denomination problem it is, and no
// number may accompany it.
func TestRWAReference_RefusesANonDollarDenominatedFeed(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		// 1.00295305 — a NAV RATIO against the reserve asset, not $1.
		refUpdate(t, "redstone", "rwa:USTRY", "crypto:BTC", "100295305", 8, now),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, now)

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

// TestRWAReference_RefusesAnOffChainQuantity is rule R-B. `rwa:XAU` is
// the FX oracle's spot-gold slot — one troy ounce — and a Stellar token
// coded XAU is a token of unstated size. Their ratio is a unit
// conversion, and published as a percentage it would read as a 99.99%
// discount on a perfectly ordinary token.
func TestRWAReference_RefusesAnOffChainQuantity(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "reflector-fx", "rwa:XAU", "fiat:USD", "440086022830869146", 14, now),
	})
	a := admittedRow("XAU", "0.5000")
	rwaApplyReference(&a, snap, now)

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
// the authority-sanity feeds write into the same hypertable for
// divergence comparison. A premium measured against an aggregator's own
// read of the market would compare the market with itself and report the
// difference as a finding.
func TestRWAReference_RefusesANonOracleSource(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "coingecko", "rwa:USTRY", "fiat:USD", "104000000", 8, now),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, now)

	if a.Reference != nil {
		t.Fatalf("an aggregator row was served as an independent oracle reference: %+v", a.Reference)
	}
	if a.Premium.Status != RWAPremiumNoReference {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumNoReference)
	}
}

// TestRWAReference_RefusesADeclaredPegPrice is rule R-D. A price carried
// on price_basis is the issuer's declared peg or a hop through another
// market. Comparing it against an independent valuation and calling the
// gap a premium reports the issuer's own claim back as a market finding.
func TestRWAReference_RefusesADeclaredPegPrice(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRow("USTRY", "1.0000")
	a.Valuation.PriceBasis = priceBasisDeclaredPeg
	rwaApplyReference(&a, snap, now)

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
			rwaApplyReference(&a, snap, now)
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

// TestRWAReference_UnpricedAssetKeepsTheReference: an instrument no
// Stellar market prices still has an independent valuation, and that is
// worth more than a blank. What must not happen is a premium of zero
// standing in for a comparison that was never made.
func TestRWAReference_UnpricedAssetKeepsTheReference(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:TESOURO", "fiat:USD", "24538100", 8, now),
	})
	a := admittedRow("TESOURO", "") // unpriced
	rwaApplyReference(&a, snap, now)

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

// TestRWAReference_FlaggedIssuerGetsNoIndependentValuationEither. The
// scam-flag suppression withholds this issuer's own price everywhere on
// the site. Handing its token a REAL instrument's oracle NAV instead
// would publish a larger claim through the gap: a dollar figure on an
// impersonator, sourced from an oracle that never named it.
func TestRWAReference_FlaggedIssuerGetsNoIndependentValuationEither(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := RWAAsset{Code: "USTRY", Valuation: RWAValuation{Status: RWAValuationIssuerFlagged}}
	rwaApplyReference(&a, snap, now)

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
	rwaApplyReference(&a, snap, now)

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
	rwaApplyReference(&b, fresh, now)
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

// TestRWAReference_UnavailableSnapshotStatesItsOwnStatus. A read that
// did not answer knows nothing either way, and must not report "no
// oracle publishes a valuation for this instrument" — that is a finding
// about the world, and a failed read has not earned it. The status is
// the assertion that matters: without a distinct one, an outage is
// indistinguishable on the wire from a genuine absence.
func TestRWAReference_UnavailableSnapshotPublishesNothing(t *testing.T) {
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, rwaReferences{}, time.Now())
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
	rwaApplyReference(&b, rwaReferenceSnapshotFrom(nil), time.Now())
	if b.Premium.Status != RWAPremiumNoReference {
		t.Errorf("an empty but SUCCESSFUL read gave status %q, want %q — the two must be distinguishable",
			b.Premium.Status, RWAPremiumNoReference)
	}
}

// TestRWAReference_CarriedForwardRowsStillExpire is the age bound on the
// carry-forward. A snapshot is reused across a failed read, so under a
// sustained outage its rows age past the seven-day window an active
// stream is defined by — the bound this file and the methodology both
// state is absolute. Enforced on the observation, not the snapshot, so
// it holds however the row reached the request.
func TestRWAReference_CarriedForwardRowsStillExpire(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now.Add(-8*24*time.Hour)),
	})
	a := admittedRow("USTRY", "1.0400")
	rwaApplyReference(&a, snap, now)

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
	rwaApplyReference(&b, fresh, now)
	if b.Reference == nil || !b.Reference.Stale {
		t.Errorf("a six-day-old observation must be served and labelled stale: %+v", b.Reference)
	}
}

// TestRWAReference_RefusesAnUnboundIssuer is R-0, the identity rule.
//
// Asset codes are not unique on Stellar. A join on the code alone hands
// every account issuing a token called USTRY the real instrument's net
// asset value — here, a token trading at $0.20 published at an 81%
// discount to a security it has nothing to do with.
func TestRWAReference_RefusesAnUnboundIssuer(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRowFor("USTRY", unboundIssuer, "0.20000000")
	rwaApplyReference(&a, snap, now)

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
	rwaApplyReference(&b, snap, now)
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
	rwaApplyReference(&a, snap, now)
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
	rwaApplyReference(&a, snap, now)
	if a.Premium.Status != RWAPremiumReferenceNotPositive {
		t.Errorf("premium status = %q, want %q", a.Premium.Status, RWAPremiumReferenceNotPositive)
	}
	if a.Premium.Pct != nil {
		t.Errorf("premium pct = %q, want absent", *a.Premium.Pct)
	}
}
