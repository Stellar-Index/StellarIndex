package aggregate

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeDecimalsLookup is a trivial in-memory DecimalsLookup for tests.
type fakeDecimalsLookup map[string]int

func (f fakeDecimalsLookup) Lookup(assetID string) (int, bool) {
	d, ok := f[assetID]
	return d, ok
}

// testContract is a real, valid on-chain C-strkey — the founding
// CS-026/2026-07-08 decimals incident's contract id
// (docs/operations/runbooks/dex-nonstandard-decimals.md), reused here
// purely as a memorable, checksum-valid fixture.
const testContract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

func TestResolveDecimals_NilLookupDefaultsStandard(t *testing.T) {
	asset, err := canonical.NewSorobanAsset(testContract)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	if got := ResolveDecimals(nil, asset); got != StandardDecimals {
		t.Errorf("ResolveDecimals(nil, ...) = %d, want %d", got, StandardDecimals)
	}
}

func TestResolveDecimals_UnflaggedAssetDefaultsStandard(t *testing.T) {
	lookup := fakeDecimalsLookup{"COTHER": 9}
	asset, err := canonical.NewSorobanAsset(testContract)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	if got := ResolveDecimals(lookup, asset); got != StandardDecimals {
		t.Errorf("ResolveDecimals for an asset absent from the table = %d, want %d (7dp-by-policy default)", got, StandardDecimals)
	}
}

func TestResolveDecimals_FlaggedAssetReturnsConfirmedValue(t *testing.T) {
	const contract = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	lookup := fakeDecimalsLookup{contract: 9}
	asset, err := canonical.NewSorobanAsset(contract)
	if err != nil {
		t.Fatalf("NewSorobanAsset: %v", err)
	}
	if got := ResolveDecimals(lookup, asset); got != 9 {
		t.Errorf("ResolveDecimals = %d, want 9", got)
	}
}

func TestDecimalsAdjustment_EqualDecimalsIsExactlyOne(t *testing.T) {
	got := DecimalsAdjustment(7, 7)
	want := big.NewRat(1, 1)
	if got.Cmp(want) != 0 {
		t.Errorf("DecimalsAdjustment(7,7) = %s, want 1", got.String())
	}
	got = DecimalsAdjustment(18, 18)
	if got.Cmp(want) != 0 {
		t.Errorf("DecimalsAdjustment(18,18) = %s, want 1", got.String())
	}
}

func TestDecimalsAdjustment_PositiveAndNegativeDelta(t *testing.T) {
	// base more granular than quote (delta > 0): scale UP by 10^delta.
	got := DecimalsAdjustment(9, 7) // e.g. base declares 9dp, quote 7dp
	want := big.NewRat(100, 1)      // 10^(9-7)
	if got.Cmp(want) != 0 {
		t.Errorf("DecimalsAdjustment(9,7) = %s, want 100", got.String())
	}

	// base coarser than quote (delta < 0): scale DOWN by 10^-delta.
	got = DecimalsAdjustment(7, 9)
	want = new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(100))
	if got.Cmp(want) != 0 {
		t.Errorf("DecimalsAdjustment(7,9) = %s, want 1/100", got.String())
	}
}

// TestAdjustPrice_ByteIdenticalWhenBothStandard proves constraint #5's
// "7dp assets are byte-identical before/after": AdjustPrice on a
// standard-decimals pair returns the exact same *big.Rat value (not merely
// an equal one) as the unadjusted input — the historical/default code path
// is untouched.
func TestAdjustPrice_ByteIdenticalWhenBothStandard(t *testing.T) {
	raw := new(big.Rat).SetFrac(big.NewInt(340), big.NewInt(120)) // 17/6
	adjusted := AdjustPrice(raw, StandardDecimals, StandardDecimals)
	if adjusted != raw {
		t.Errorf("AdjustPrice with equal decimals must return the identical *big.Rat (no-op), got a different pointer")
	}
	if adjusted.Cmp(raw) != 0 {
		t.Errorf("AdjustPrice(7,7) changed the value: got %s, want %s", adjusted.String(), raw.String())
	}
}

func TestAdjustPrice_NilInputReturnsNil(t *testing.T) {
	if AdjustPrice(nil, 9, 7) != nil {
		t.Error("AdjustPrice(nil, ...) must return nil")
	}
}

// TestAdjustPrice_Golden18DecimalToken is the golden-number regression
// constraint #5 asks for: a real-shaped trade of an 18-decimal bridged
// token (base leg) quoted against a 7-decimal SAC/classic asset (quote
// leg), showing the OLD unadjusted ratio is off by exactly 10^11 — the
// |18-7| skew CLAUDE.md and the runbook describe — and that AdjustPrice
// corrects it exactly.
//
// Trade shape: base_amount = 2_500_000_000_000_000_000 (2.5 whole tokens
// at 18dp), quote_amount = 12_420_000 (1.242 USDC at 7dp). True price =
// 1.242 / 2.5 = 0.4968 USDC per token.
func TestAdjustPrice_Golden18DecimalToken(t *testing.T) {
	baseAmount := new(big.Int)
	baseAmount.SetString("2500000000000000000", 10) // 2.5 * 10^18
	quoteAmount := big.NewInt(12_420_000)           // 1.242 * 10^7

	rawRatio := new(big.Rat).SetFrac(quoteAmount, baseAmount)

	truePrice := new(big.Rat).SetFrac(big.NewInt(4968), big.NewInt(10000)) // 0.4968
	adjusted := AdjustPrice(rawRatio, 18, StandardDecimals)
	if adjusted.Cmp(truePrice) != 0 {
		t.Fatalf("AdjustPrice(18dp base, 7dp quote) = %s, want %s (true price 0.4968)",
			adjusted.RatString(), truePrice.RatString())
	}

	// Show exactly how wrong the OLD (unadjusted) served value was:
	// rawRatio should equal truePrice / 10^11 — the "served skewed by
	// 10^(7-decimals)" landmine CLAUDE.md and the runbook describe.
	tenToThe11 := new(big.Int).Exp(big.NewInt(10), big.NewInt(11), nil)
	expectedRaw := new(big.Rat).Quo(truePrice, new(big.Rat).SetInt(tenToThe11))
	if rawRatio.Cmp(expectedRaw) != 0 {
		t.Fatalf("raw (unadjusted) ratio = %s, want %s (true price / 10^11)",
			rawRatio.RatString(), expectedRaw.RatString())
	}

	errorFactor := new(big.Rat).Quo(truePrice, rawRatio)
	if errorFactor.Cmp(new(big.Rat).SetInt(tenToThe11)) != 0 {
		t.Fatalf("old code path was off by %s, want exactly 10^11", errorFactor.RatString())
	}
}

// TestAdjustPrice_Golden6DecimalQuoteToken covers the opposite direction
// (a 6dp quote leg — one of the five confirmed offenders per the ROADMAP
// is exactly 6dp) — the base leg is standard, so the adjustment scales
// DOWN.
func TestAdjustPrice_Golden6DecimalQuoteToken(t *testing.T) {
	// base_amount = 100 * 10^7 (100 whole units at 7dp), quote_amount =
	// 250 * 10^6 (250 whole units at 6dp). True price = 250/100 = 2.5.
	baseAmount := big.NewInt(100 * 1e7)
	quoteAmount := big.NewInt(250 * 1e6)
	rawRatio := new(big.Rat).SetFrac(quoteAmount, baseAmount) // 2.5 * 10^-1 = 0.25, off by 10x

	adjusted := AdjustPrice(rawRatio, StandardDecimals, 6)
	truePrice := big.NewRat(5, 2) // 2.5
	if adjusted.Cmp(truePrice) != 0 {
		t.Fatalf("AdjustPrice(7dp base, 6dp quote) = %s, want 2.5", adjusted.RatString())
	}
}
