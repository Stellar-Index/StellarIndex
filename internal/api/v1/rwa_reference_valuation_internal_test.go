package v1

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
)

// Unit tests for the reference-priced valuation (supply x oracle value), pinned directly: scale is the asset's decimals,
// arithmetic is exact, and every non-valuation input is refused.

// TestRWAReferenceValue_UsesTheAssetsOwnDecimals guards against a hardcoded scale of 7, a plausible number off by a
// power of ten. Same supply and price at two scales, so a constant can produce only one of the answers.
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

// TestRWAReferenceValue_IsExact pins ADR-0003: exact rational arithmetic with a single rounding at the end. The inputs
// need 18 significant digits, so float64 is off by a cent (exact 39874251874.64 vs float64 39874251874.65), and the
// summary sums the rounded row strings so the column adds up to the total.
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

// TestRWAReferenceValue_RefusesWhatIsNotAValuation: every input that is not a supply and a
// positive price is refused outright, never turned into a zero. A published "0.00" asserts the
// backing is worth nothing, which none of these cases is.
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
		// big.Int.Exp answers a negative exponent with 1, so an unguarded scale would publish the raw smallest-unit count as
		// dollars, 10^7 too large yet plausible.
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

// TestRWAReferenceValuation_RefusesANonPositiveOracleValue: the premium path refuses to DIVIDE
// by a non-positive net asset value; this path must refuse to MULTIPLY by one, since zero times a
// supply is a clean-looking $0.00 the oracle never claimed.
func TestRWAReferenceValuation_RefusesANonPositiveOracleValue(t *testing.T) {
	now := time.Now()
	for _, raw := range []string{"0", "-107403800"} {
		snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", raw, 8, now),
		})
		a := admittedRow("USTRY", "")
		supply := "12336218000000"
		a.CirculatingSupply = &supply
		a.Decimals = intPtr(7)
		rwaApplyReference(&a, snap, nil, nil, now)

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
	a.Decimals = intPtr(7)
	rwaApplyReference(&a, snap, nil, nil, now)

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

// TestRWAReferenceValuation_UnboundPairGetsNothing: a token merely wearing an instrument's
// ticker must not be handed that instrument's net asset value; multiplying by the impostor's
// whole supply would publish a LARGER false claim than the per-unit figure.
func TestRWAReferenceValuation_UnboundPairGetsNothing(t *testing.T) {
	now := time.Now()
	snap := rwaReferenceSnapshotFrom([]canonical.OracleUpdate{
		refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8, now),
	})
	a := admittedRowFor("USTRY", unboundIssuer, "1.0400")
	supply := "12336218000000"
	a.CirculatingSupply = &supply
	a.Decimals = intPtr(7)
	rwaApplyReference(&a, snap, nil, nil, now)

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

// TestRWAReferenceValuation_StatusIsNeverEmpty: every row carries a decision. An empty status
// reads as "nothing to say" when it means "nobody decided", and an absence must state its reason.
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
		rwaApplyReference(&a, snap, nil, nil, now)
		if a.ReferenceValuation.Status == "" {
			t.Errorf("%s: reference_valuation.status is empty", name)
		}
	}
	// And the flagged-issuer branch, which returns before any of the
	// above is consulted.
	flagged := RWAAsset{Code: "USTRY", Issuer: boundIssuer, Valuation: RWAValuation{Status: RWAValuationIssuerFlagged}}
	rwaApplyReference(&flagged, rwaReferenceSnapshotFrom(nil), nil, nil, now)
	if flagged.ReferenceValuation.Status != RWAPremiumIssuerFlagged {
		t.Errorf("flagged issuer: status = %q, want %q",
			flagged.ReferenceValuation.Status, RWAPremiumIssuerFlagged)
	}
}

// rwaTestContractID is a valid C-strkey standing for nothing on the
// network. Valid because the contract arm CRC-checks the address, so a
// made-up string would be refused for the wrong reason.
const rwaTestContractID = "CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM"

// TestRWAReferenceValuation_ContractMemberIsRefusedByName: nothing binds a contract address to an oracle feed (only its
// self-authored SEP-41 symbol, a refused code join). The refusal needs its own name, as `reference_not_bound` would
// imply a pair.
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
		Decimals:   intPtr(6),
	}
	supply := "8500000000000"
	a.CirculatingSupply = &supply
	rwaApplyReference(&a, snap, nil, nil, now)

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

// TestRWAReferenceDropActors_CoverEveryStatusARowCanCarry: a status missing from the actor map silently falls to a
// default and blames the wrong party. Pinned both ways: new refusals need an actor, stale entries fail.
func TestRWAReferenceDropActors_CoverEveryStatusARowCanCarry(t *testing.T) {
	// Every status reference_valuation.status can carry other than published. Premium-only statuses are excluded since a row
	// with a reference but no market price still has a full reference valuation.
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
		// A declared ISIN contradicting the constant-NAV table.
		RWAPremiumReferenceISINMismatch: {},
		// A contract row whose own declared scale could not be read.
		// The exponent IS the figure on this surface, so an unread one
		// refuses the valuation rather than defaulting to 7.
		RWAReferenceValuationDecimalsUnknown: {},
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
	for _, premiumOnly := range []string{
		RWAPremiumNoMarketPrice, RWAPremiumMarketNotObserved, RWAPremiumPublished,
		// The non-oracle refusals are premium-only BY DESIGN: each
		// refuses a premium while the reference valuation beside it is
		// published — so none may appear as a valuation drop.
		RWAPremiumReferenceNotOracle,
		RWAPremiumReferenceNotOracleCNAV,
		RWAPremiumReferenceNotOracleCurator,
		RWAPremiumReferenceNotOracleFundNAV,
	} {
		if _, ok := rwaReferenceDropActors[premiumOnly]; ok {
			t.Errorf("%q is a premium-only status and must not be a reference-valuation drop", premiumOnly)
		}
	}
}

// TestRWAValuationStages_BalanceIsNotTautological proves the funnel balance can fail: stages count rows carrying a
// figure, drops count rows whose status is not published, so a "published" status over an absent figure breaks it.
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
