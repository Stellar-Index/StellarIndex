package v1

import (
	"math/big"
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
