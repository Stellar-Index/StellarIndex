package v1

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// Unit tests for the reference-priced valuation: supply x the oracle's
// value of the instrument.
//
// The arithmetic is the whole of the claim, so it is pinned directly
// rather than only through the handler. Three properties matter and
// each gets its own refutation: the scale is the ASSET's decimals, the
// arithmetic is exact, and every input that is not a valuation is
// refused instead of being turned into one.

// TestRWAReferenceValue_UsesTheAssetsOwnDecimals is the decimals
// defect in this figure's coordinates.
//
// A market cap computed against a hardcoded 7 was a real production
// bug. The same arithmetic on the same row is exposed the same way, and
// a wrong scale does not look wrong — it looks like a plausible number
// off by a power of ten, which is exactly the kind of error a reader
// cannot catch from the output.
//
// The two cases below use the SAME supply integer and the SAME price
// and differ only in the scale, so a constant in place of the field can
// only produce one of the two answers.
func TestRWAReferenceValue_UsesTheAssetsOwnDecimals(t *testing.T) {
	price := big.NewRat(2, 1) // $2.00 per whole token.
	cases := []struct {
		name     string
		decimals int
		want     string
	}{
		// 12,336,218,000,000 smallest units at 7dp = 1,233,621.8 tokens.
		{"classic seven", 7, "2467243.60"},
		// The same integer at 6dp is ten times the float, and so ten
		// times the valuation. Nothing in the number itself says which
		// reading is right.
		{"six", 6, "24672436.00"},
		{"zero — the integer IS the float", 0, "24672436000000.00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rwaReferenceValueUSD("12336218000000", c.decimals, price)
			if got != c.want {
				t.Errorf("value at %d decimals = %q, want %q", c.decimals, got, c.want)
			}
		})
	}
}

// TestRWAReferenceValue_IsExact proves the ADR-0003 property: exact
// rational arithmetic end to end, with a single rounding at the last
// step.
//
// A ten-digit float times an eight-decimal oracle price is past the
// point where float64 is a faithful record of the product — it carries
// about fifteen significant digits, and this needs eighteen. The inputs
// below are chosen so the difference reaches the cent:
//
//	supply    43985621232331962 smallest units (4,398,562,123.2331962)
//	reference 9.06529242
//	exact     39874251874.64
//	float64   39874251874.65
//
// A money total that is quietly a penny wrong is worse than an absent
// one, because nothing about it looks wrong. The same discipline is why
// the summary sums the already-rounded per-row strings: a reader who
// adds up the column lands on the published total.
func TestRWAReferenceValue_IsExact(t *testing.T) {
	price := ratFromScaledInt(big.NewInt(906529242), 8)
	got := rwaReferenceValueUSD("43985621232331962", 7, price)
	if got != "39874251874.64" {
		t.Errorf("value = %q, want 39874251874.64 (float64 arithmetic answers 39874251874.65 here)", got)
	}

	// And the same figure derived independently, as integers, so the
	// assertion above is pinned to the arithmetic rather than to a
	// literal somebody could adjust to match a regression.
	want := new(big.Rat).SetFrac(
		new(big.Int).Mul(bigFromString(t, "43985621232331962"), big.NewInt(906529242)),
		bigFromString(t, "1000000000000000"),
	).FloatString(2)
	if got != want {
		t.Errorf("value = %q, want the exact product %q", got, want)
	}
}

func bigFromString(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad integer %q", s)
	}
	return n
}

// TestRWAReferenceValue_RefusesWhatIsNotAValuation — every input that
// is not a supply and a positive price is refused outright. None of
// them may become a zero: a published "0.00" on this field asserts that
// the backing behind the circulating tokens is worth nothing, which is
// a finding, and none of these cases is that finding.
func TestRWAReferenceValue_RefusesWhatIsNotAValuation(t *testing.T) {
	price := big.NewRat(2, 1)
	cases := []struct {
		name     string
		supply   string
		decimals int
		price    *big.Rat
	}{
		{"no price at all", "1000", 7, nil},
		{"negative supply is bad data, not a negative valuation", "-1000", 7, price},
		{"an unparseable supply is not a supply", "many", 7, price},
		{"a blank supply is not a supply", "", 7, price},
		// big.Int.Exp answers a negative exponent with 1, so an
		// unguarded scale would publish the raw smallest-unit count as
		// dollars — a figure 10^7 too large, and positive, and
		// plausible.
		{"a negative scale is not a scale", "1000", -7, price},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rwaReferenceValueUSD(c.supply, c.decimals, c.price); got != "" {
				t.Errorf("value = %q, want no figure at all", got)
			}
		})
	}
}

// TestRWAReferenceValuation_RefusesANonPositiveOracleValue — the
// premium path refuses to DIVIDE by a non-positive net asset value;
// this path must refuse to MULTIPLY a float by one, and for the same
// reason. Zero times a supply is a clean-looking $0.00 that the oracle
// never claimed.
func TestRWAReferenceValuation_RefusesANonPositiveOracleValue(t *testing.T) {
	now := time.Now()
	for _, raw := range []string{"0", "-107403800"} {
		snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", raw, 8, now),
		})
		a := admittedRow("USTRY", "")
		supply := "12336218000000"
		a.CirculatingSupply = &supply
		a.Decimals = 7
		rwaApplyReference(&a, snap, now)

		if a.ReferenceValuation.Status != RWAPremiumReferenceNotPositive {
			t.Errorf("raw %q: reference_valuation.status = %q, want %q",
				raw, a.ReferenceValuation.Status, RWAPremiumReferenceNotPositive)
		}
		if a.ReferenceValuation.ValueUSD != nil {
			t.Errorf("raw %q: a valuation was published from a non-positive oracle value: %q",
				raw, *a.ReferenceValuation.ValueUSD)
		}
	}
}

// TestRWAReferenceValuation_NoSupplyIsNotZero — a bound, current,
// dollar-denominated feed with no circulating-supply reading behind it
// has nothing to value. The row says so and carries no number.
func TestRWAReferenceValuation_NoSupplyIsNotZero(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRow("USTRY", "")
	a.Decimals = 7
	rwaApplyReference(&a, snap, now)

	if a.Reference == nil {
		t.Fatal("the reference itself must still be published — the missing input is the supply, not the feed")
	}
	if a.ReferenceValuation.Status != RWAReferenceValuationNoSupply {
		t.Errorf("status = %q, want %q", a.ReferenceValuation.Status, RWAReferenceValuationNoSupply)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("value_usd = %q, want absent", *a.ReferenceValuation.ValueUSD)
	}
}

// TestRWAReferenceValuation_UnboundPairGetsNothing — R-0 governs this
// figure exactly as it governs the reference and the premium. A token
// merely wearing an instrument's ticker must not be handed that
// instrument's net asset value, and multiplying it by the impostor's
// own float would publish a LARGER false claim than the per-unit
// figure does.
func TestRWAReferenceValuation_UnboundPairGetsNothing(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRowFor("USTRY", unboundIssuer, "1.0400")
	supply := "12336218000000"
	a.CirculatingSupply = &supply
	a.Decimals = 7
	rwaApplyReference(&a, snap, now)

	if a.ReferenceValuation.Status != RWAPremiumNotBound {
		t.Errorf("status = %q, want %q", a.ReferenceValuation.Status, RWAPremiumNotBound)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("an unbound pair was valued at a real instrument's price: %q", *a.ReferenceValuation.ValueUSD)
	}
	if a.Premium.Status != a.ReferenceValuation.Status {
		t.Errorf("premium.status = %q but reference_valuation.status = %q — one refusal, two accounts of it",
			a.Premium.Status, a.ReferenceValuation.Status)
	}
}

// TestRWAReferenceValuation_StatusIsNeverEmpty — every row carries a
// decision. An empty status on a money-adjacent field reads as "nothing
// to say" when it means "nobody decided", and the two are not the same
// on a surface whose whole discipline is that an absence states its
// reason.
func TestRWAReferenceValuation_StatusIsNeverEmpty(t *testing.T) {
	now := time.Now()
	snaps := map[string]rwaReferences{
		"a read that did not answer": {},
		"an empty oracle stream":     rwaReferenceSnapshotFrom(nil),
		"a stream carrying the feed": rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
		}),
		"an expired observation": rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8,
				now.Add(-rwaReferenceMaxAge-time.Hour)),
		}),
	}
	for name, snap := range snaps {
		a := admittedRow("USTRY", "1.0400")
		rwaApplyReference(&a, snap, now)
		if a.ReferenceValuation.Status == "" {
			t.Errorf("%s: reference_valuation.status is empty", name)
		}
	}
	// And the flagged-issuer branch, which returns before any of the
	// above is consulted.
	flagged := RWAAsset{Code: "USTRY", Issuer: boundIssuer, Valuation: RWAValuation{Status: RWAValuationIssuerFlagged}}
	rwaApplyReference(&flagged, rwaReferenceSnapshotFrom(nil), now)
	if flagged.ReferenceValuation.Status != RWAPremiumIssuerFlagged {
		t.Errorf("flagged issuer: status = %q, want %q",
			flagged.ReferenceValuation.Status, RWAPremiumIssuerFlagged)
	}
}

// TestRWAReference_OneRefusalIsReportedIdentically pins the property
// that keeps the row and the funnel telling one story.
//
// Every rule in this file refuses the premium and the reference
// valuation together. If the two fields could carry different strings
// for one event, the funnel — which reads only
// `reference_valuation.status` — would report a reason the row does not
// show, and a reader reconciling the two would find them disagreeing
// about what happened.
//
// Every refusal path is driven, and each is checked to actually reach
// the status it claims, so a path that silently stopped firing cannot
// pass this by agreeing trivially.
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
			rwaApplyReference(&a, c.snap, now)
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

// rwaTestContractID is a valid C-strkey standing for nothing on the
// network. Valid because the contract arm CRC-checks the address, so a
// made-up string would be refused for the wrong reason.
const rwaTestContractID = "CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM"

// TestRWAReferenceValuation_ContractMemberIsRefusedByName is the
// decision the contract arm forces, stated as a test.
//
// A contract-issued member has everything the arithmetic needs — a
// supply from the certified lake and its own declared decimals — and it
// still gets no reference valuation, because nothing binds a CONTRACT
// ADDRESS to an oracle feed. The available join is its on-chain SEP-41
// symbol, which is metadata the contract itself authors; pricing a
// token by it is the code-keyed join this package exists to refuse,
// with a weaker key than the classic one.
//
// The refusal must be reported under its own name. Answering with
// `reference_not_bound` would name a (code, issuer) pair the row does
// not have, and leaving the field silent would make a deliberate
// refusal indistinguishable from an oversight.
func TestRWAReferenceValuation_ContractMemberIsRefusedByName(t *testing.T) {
	now := time.Now()
	// A live feed for the very symbol the contract declares. The
	// refusal has to hold WITH a matching feed available, or it proves
	// nothing about the join being refused.
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := RWAAsset{
		ContractID: rwaTestContractID,
		Symbol:     "USTRY",
		Basis:      rwa.BasisContractOracleFeed,
		Valuation:  RWAValuation{Status: RWAValuationUnpriced},
		Decimals:   6,
	}
	supply := "8500000000000"
	a.CirculatingSupply = &supply
	rwaApplyReference(&a, snap, now)

	if a.ReferenceValuation.Status != RWAPremiumContractNotBound {
		t.Errorf("reference_valuation.status = %q, want %q",
			a.ReferenceValuation.Status, RWAPremiumContractNotBound)
	}
	if a.Premium.Status != RWAPremiumContractNotBound {
		t.Errorf("premium.status = %q, want %q", a.Premium.Status, RWAPremiumContractNotBound)
	}
	if a.ReferenceValuation.ValueUSD != nil {
		t.Errorf("a contract token was valued at an instrument's price by its own declared symbol: %q",
			*a.ReferenceValuation.ValueUSD)
	}
	if a.Reference != nil {
		t.Errorf("a reference was attached to a contract row: %+v", a.Reference)
	}
	// And the refusal is its OWN, not the classic one wearing a
	// different row's identity.
	if a.ReferenceValuation.Status == RWAPremiumNotBound {
		t.Error("a contract row was refused under a status naming a (code, issuer) pair it does not have")
	}
}

// TestRWAReferenceDropActors_CoverEveryStatusARowCanCarry closes the
// funnel's one unguarded edge.
//
// The funnel attributes each reference-valuation drop to an actor from
// a map. A status with no entry falls through to a default, which is a
// silent mislabel: the drop would tell a reader to chase the wrong
// party, and nothing would fail. So the mapping is pinned to the
// statuses themselves, in BOTH directions — a new refusal added without
// an actor fails here, and a stale entry for a status no longer served
// fails here too.
func TestRWAReferenceDropActors_CoverEveryStatusARowCanCarry(t *testing.T) {
	// Every status reference_valuation.status can carry, other than
	// published. The premium-only statuses are excluded deliberately: a
	// row with a reference and no market price still carries a full
	// reference valuation, so they can never appear on this field.
	carried := map[string]struct{}{
		RWAPremiumIssuerFlagged:        {},
		RWAPremiumReferenceUnavailable: {},
		RWAPremiumContractNotBound:     {},
		RWAPremiumNotInstrumentScoped:  {},
		RWAPremiumNotBound:             {},
		RWAPremiumReferenceNotUSD:      {},
		RWAPremiumNoReference:          {},
		RWAPremiumReferenceExpired:     {},
		RWAPremiumReferenceNotPositive: {},
		RWAReferenceValuationNoSupply:  {},
	}
	for status := range carried {
		if _, ok := rwaReferenceDropActors[status]; !ok {
			t.Errorf("status %q has no funnel actor — its drop would be attributed to a default", status)
		}
		if !slices.Contains(rwaReferenceRefusalOrder, status) {
			t.Errorf("status %q is missing from the funnel drop order — it would sort after every known reason", status)
		}
	}
	for status := range rwaReferenceDropActors {
		if _, ok := carried[status]; !ok {
			t.Errorf("actor mapped for %q, which reference_valuation.status never carries — stale entry", status)
		}
	}
	for _, status := range rwaReferenceRefusalOrder {
		if _, ok := carried[status]; !ok {
			t.Errorf("drop order names %q, which reference_valuation.status never carries — stale entry", status)
		}
	}
	// The premium-only statuses must NOT be in either table: attributing
	// an actor to a comparison failure would put it in a funnel arm that
	// does not account for comparisons.
	for _, premiumOnly := range []string{RWAPremiumNoMarketPrice, RWAPremiumMarketNotObserved, RWAPremiumPublished} {
		if _, ok := rwaReferenceDropActors[premiumOnly]; ok {
			t.Errorf("%q is a premium-only status and must not be a reference-valuation drop", premiumOnly)
		}
	}
}

// TestRWAValuationStages_BalanceIsNotTautological proves the funnel's
// new arm can actually fail.
//
// An accounting whose two sides are computed from the same expression
// always closes and tells a reader nothing. This one derives them
// differently on purpose: the stage counts rows CARRYING A FIGURE, and
// the drops count rows whose STATUS is not published. They reconcile
// only if published and carrying-money are the same set of rows — which
// is the invariant worth having, because a status that says published
// over an absent figure is exactly how a valuation surface starts
// lying.
//
// Both directions of the break are driven.
func TestRWAValuationStages_BalanceIsNotTautological(t *testing.T) {
	money := "1325134.33"
	honest := []RWAAsset{
		{ReferenceValuation: RWAReferenceValuation{Status: RWAReferenceValuationPublished, ValueUSD: &money}},
		{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotBound}},
	}
	if bad := rwaFunnelImbalance(rwaValuationStages(honest)); bad != "" {
		t.Errorf("a consistent set failed to balance: %s", bad)
	}

	cases := map[string][]RWAAsset{
		"published with no figure": {
			{ReferenceValuation: RWAReferenceValuation{Status: RWAReferenceValuationPublished}},
			{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotBound}},
		},
		"a figure under a refusal": {
			{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotBound, ValueUSD: &money}},
			{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotBound}},
		},
	}
	for name, assets := range cases {
		t.Run(name, func(t *testing.T) {
			if bad := rwaFunnelImbalance(rwaValuationStages(assets)); bad == "" {
				t.Error("the funnel balanced over an inconsistent set — the check cannot fail and proves nothing")
			}
		})
	}
}

// TestRWAValuationStages_DropOrderIsStable — the wire order of the
// drops must not depend on map iteration, or two identical responses
// differ byte for byte and a diff of the funnel becomes unreadable.
func TestRWAValuationStages_DropOrderIsStable(t *testing.T) {
	assets := []RWAAsset{
		{ReferenceValuation: RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}},
		{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotBound}},
		{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumIssuerFlagged}},
		{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumNotInstrumentScoped}},
		{ReferenceValuation: RWAReferenceValuation{Status: RWAPremiumContractNotBound}},
	}
	want := []string{
		RWAPremiumIssuerFlagged,
		RWAPremiumContractNotBound,
		RWAPremiumNotInstrumentScoped,
		RWAPremiumNotBound,
		RWAReferenceValuationNoSupply,
	}
	for range 8 {
		got := []string{}
		for _, d := range rwaValuationStages(assets)[0].Dropped {
			got = append(got, d.Reason)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("drop order = %v, want the rule order %v", got, want)
		}
	}
}
