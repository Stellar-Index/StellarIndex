package v1

import (
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Forward-fill: a price day uses the most-recent supply at-or-before
// it, including the carry-in row that precedes the price window.
func TestMarketCapPoints_ForwardFill(t *testing.T) {
	price := []HistoryPoint{
		{Bucket: day(2026, 6, 1), VWAP: "0.10"},
		{Bucket: day(2026, 6, 2), VWAP: "0.20"},
		{Bucket: day(2026, 6, 3), VWAP: "0.30"},
	}
	// Supply snapshots: carry-in (May 30) + one mid-window update (Jun 2).
	// 10^10 stroops at 7 decimals = 1000.0 major units; 2×10^10 = 2000.0.
	supply := []timescale.SupplyDayPoint{
		{Bucket: day(2026, 5, 30), Circulating: big.NewInt(1_000_0000000)}, // 1000.0
		{Bucket: day(2026, 6, 2), Circulating: big.NewInt(2_000_0000000)},  // 2000.0
	}

	got := marketCapPoints(price, supply, 7)
	want := []struct {
		t time.Time
		p string
	}{
		{day(2026, 6, 1), "100.00"}, // 0.10 × 1000.0 (carry-in)
		{day(2026, 6, 2), "400.00"}, // 0.20 × 2000.0 (updated this day)
		{day(2026, 6, 3), "600.00"}, // 0.30 × 2000.0 (forward-filled)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d points, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if !got[i].T.Equal(w.t) || got[i].P != w.p {
			t.Errorf("point %d = (%s, %s), want (%s, %s)", i, got[i].T, got[i].P, w.t, w.p)
		}
	}
}

// A price day before the first supply snapshot is skipped (no
// fabricated zero), and emission resumes once supply exists.
func TestMarketCapPoints_SkipBeforeFirstSupply(t *testing.T) {
	price := []HistoryPoint{
		{Bucket: day(2026, 6, 1), VWAP: "0.10"}, // no supply yet → skipped
		{Bucket: day(2026, 6, 5), VWAP: "0.50"}, // supply exists → emitted
	}
	supply := []timescale.SupplyDayPoint{
		{Bucket: day(2026, 6, 3), Circulating: big.NewInt(1_000_0000000)}, // 1000.0
	}
	got := marketCapPoints(price, supply, 7)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1: %+v", len(got), got)
	}
	if !got[0].T.Equal(day(2026, 6, 5)) || got[0].P != "500.00" {
		t.Errorf("got (%s, %s), want (2026-06-05, 500.00)", got[0].T, got[0].P)
	}
}

// No supply at all → empty series (not a panic, not zeros).
func TestMarketCapPoints_NoSupply(t *testing.T) {
	price := []HistoryPoint{{Bucket: day(2026, 6, 1), VWAP: "0.10"}}
	if got := marketCapPoints(price, nil, 7); len(got) != 0 {
		t.Errorf("want empty series with no supply, got %+v", got)
	}
}

// M2: the supply leg divides by 10^baseDecimals, NOT a hardcoded 10^7. A 9dp
// token's circulating supply is denominated in 10^9 stroops, so a 10^12-stroop
// balance is 1000 whole tokens — dividing by 10^7 (the old constant) would
// report 100,000 tokens and a market cap 100× too large. VWAP here is already
// the decimals-normalized USD price (the price leg is corrected upstream in
// handleChartMarketCapCrypto via adjustHistoryPointPrices).
func TestMarketCapPoints_NonstandardDecimalsSupplyLeg(t *testing.T) {
	price := []HistoryPoint{{Bucket: day(2026, 7, 1), VWAP: "5.00"}}
	// 10^12 stroops @9dp = 1000 whole tokens.
	supply := []timescale.SupplyDayPoint{
		{Bucket: day(2026, 7, 1), Circulating: new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)},
	}
	// Correct: 1000 tokens × $5.00 = $5,000.00.
	got := marketCapPoints(price, supply, 9)
	if len(got) != 1 || got[0].P != "5000.00" {
		t.Fatalf("9dp supply leg: got %+v, want single point 5000.00", got)
	}
	// Regression guard: the old hardcoded-7 divisor would 100× it.
	old := marketCapPoints(price, supply, 7)
	if len(old) != 1 || old[0].P != "500000.00" {
		t.Fatalf("sanity: 7dp divisor should over-report as 500000.00, got %+v", old)
	}
}

// TestFiatSupplyWholeUnits_ExactBeyondFloat64 (MNY) — the catalogue
// carries circulating supply as an EXACT decimal string. Parsing it to
// float64 (the pre-fix path) truncates any value past a float's 53-bit
// mantissa (~9.007e15). fiatSupplyWholeUnits keeps it exact so the
// served market cap doesn't silently drop integer digits.
func TestFiatSupplyWholeUnits_ExactBeyondFloat64(t *testing.T) {
	// 2^53 + 1 = 9007199254740993 — the smallest integer a float64
	// cannot represent (it rounds to 9007199254740992).
	const supplyStr = "9007199254740993"

	got, ok := fiatSupplyWholeUnits(supplyStr, 0)
	if !ok {
		t.Fatalf("fiatSupplyWholeUnits(%q) not ok", supplyStr)
	}
	if got.RatString() != supplyStr {
		t.Errorf("exact supply = %s, want %s (big.Rat must not truncate)", got.RatString(), supplyStr)
	}

	// Demonstrate the pre-fix float64 path genuinely loses the low digit.
	f, _ := strconv.ParseFloat(supplyStr, 64)
	if strconv.FormatFloat(f, 'f', -1, 64) == supplyStr {
		t.Fatalf("test premise broken: float64 unexpectedly represented %s exactly", supplyStr)
	}
	if strconv.FormatFloat(f, 'f', -1, 64) != "9007199254740992" {
		t.Errorf("float64(%s) = %s, want 9007199254740992 (proves the precision loss the fix avoids)",
			supplyStr, strconv.FormatFloat(f, 'f', -1, 64))
	}

	// End-to-end: market cap at rate 1.0 must preserve the exact digit.
	mc := computeFiatMarketCap(supplyStr, "1")
	if mc == nil || *mc != "9007199254740993.00" {
		t.Errorf("computeFiatMarketCap = %v, want 9007199254740993.00 (exact)", mc)
	}
}

// TestComputeFiatMarketCap_ExactRat (MNY) — the served market cap is
// exact big.Rat, matching the crypto path's usdMarketValue. Covers the
// existing pinned catalogue cases plus a fractional-cent rounding.
func TestComputeFiatMarketCap_ExactRat(t *testing.T) {
	cases := []struct{ supply, price, want string }{
		{"21700000000000", "1.00000000000000", "21700000000000.00"},  // USD identity
		{"302000000000000", "0.14000000000000", "42280000000000.00"}, // CNY M2 × 0.14
		{"1000000000000000000", "0.000000000000000001", "1.00"},      // 1e18 × 1e-18 stays exact
	}
	for _, c := range cases {
		got := computeFiatMarketCap(c.supply, c.price)
		if got == nil || *got != c.want {
			t.Errorf("computeFiatMarketCap(%q,%q) = %v, want %q", c.supply, c.price, got, c.want)
		}
	}
}

// TestFormatCrossRate (MNY) — the fiat cross-rate is serialised from an
// exact big.Rat (no float64 division on the served price), trailing
// zeros trimmed.
func TestFormatCrossRate(t *testing.T) {
	cases := []struct {
		r    *big.Rat
		want string
	}{
		{new(big.Rat).SetFrac64(25, 23), "1.08695652173913"}, // 1/0.92 exact, 15dp, trailing zero trimmed
		{new(big.Rat).SetInt64(155), "155"},                  // integer trims to no dot
		{new(big.Rat).SetFrac64(1, 8), "0.125"},              // terminating, trailing zeros trimmed
	}
	for _, c := range cases {
		if got := formatCrossRate(c.r); got != c.want {
			t.Errorf("formatCrossRate(%s) = %q, want %q", c.r.RatString(), got, c.want)
		}
	}
}
