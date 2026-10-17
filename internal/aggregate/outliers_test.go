package aggregate_test

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestFilterOutliers_DropsFatTail(t *testing.T) {
	// A sigma threshold has a known weakness: one huge outlier inflates sigma so it cannot remove itself, so use enough baseline
	// samples (20) that the outlier does not drag the mean too far.
	baseline := []int64{
		100, 101, 99, 100, 102, 98, 101, 100, 99, 100,
		101, 100, 99, 101, 100, 102, 99, 100, 101, 100,
	}
	trades := make([]canonical.Trade, 0, len(baseline)+1)
	for _, p := range baseline {
		trades = append(trades, mkTrade(1, p))
	}
	trades = append(trades, mkTrade(1, 10_000)) // outlier

	got := aggregate.FilterOutliers(trades, 3.0)
	if len(got) != len(baseline) {
		t.Errorf("got %d trades after filter, want %d (outlier not dropped)", len(got), len(baseline))
	}
	for _, trade := range got {
		if trade.QuoteAmount.BigInt().Int64() > 500 {
			t.Errorf("outlier not removed: quote = %s", trade.QuoteAmount.String())
		}
	}
}

func TestFilterOutliers_AllWithinThresholdKept(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100),
		mkTrade(1, 101),
		mkTrade(1, 99),
		mkTrade(1, 100),
		mkTrade(1, 102),
	}
	got := aggregate.FilterOutliers(trades, 3.0)
	if len(got) != len(trades) {
		t.Errorf("filter stripped %d trades from a tight distribution", len(trades)-len(got))
	}
}

func TestFilterOutliers_ShortInputPassthrough(t *testing.T) {
	// < 3 trades → σ is meaningless; filter returns input unchanged.
	for _, n := range []int{0, 1, 2} {
		trades := make([]canonical.Trade, n)
		for i := range trades {
			trades[i] = mkTrade(1, int64(100+i))
		}
		got := aggregate.FilterOutliers(trades, 3.0)
		if len(got) != n {
			t.Errorf("n=%d: got %d, want %d (short-input passthrough)", n, len(got), n)
		}
	}
}

func TestFilterOutliers_NonPositiveSigmaNoop(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 101), mkTrade(1, 10_000),
	}
	for _, sigma := range []float64{0, -1, -3.5} {
		got := aggregate.FilterOutliers(trades, sigma)
		if len(got) != len(trades) {
			t.Errorf("sigma=%v: expected no-op, got %d/%d", sigma, len(got), len(trades))
		}
	}
}

func TestFilterOutliers_SkipsZeroBaseTrades(t *testing.T) {
	// Zero-base trades have no defined price — filter must drop them
	// pre-computation rather than divide-by-zero or feed them into σ.
	trades := []canonical.Trade{
		mkTrade(1, 100),
		mkTrade(1, 101),
		mkTrade(0, 999),
		mkTrade(1, 99),
		mkTrade(1, 100),
	}
	got := aggregate.FilterOutliers(trades, 3.0)
	if len(got) != 4 {
		t.Errorf("got %d, want 4 (zero-base must be filtered)", len(got))
	}
	for _, trade := range got {
		if trade.BaseAmount.IsZero() {
			t.Error("zero-base trade survived filter")
		}
	}
}

func TestFilterOutliers_SkipsZeroQuoteTrades(t *testing.T) {
	// Symmetric to zero-base: a trade with zero quote has no defined
	// price either. Must be dropped pre-statistics, not treated as
	// "price = 0" and dragged into the σ computation.
	trades := []canonical.Trade{
		mkTrade(1, 100),
		mkTrade(1, 101),
		mkTrade(1, 0), // zero quote
		mkTrade(1, 99),
		mkTrade(1, 100),
	}
	got := aggregate.FilterOutliers(trades, 3.0)
	if len(got) != 4 {
		t.Errorf("got %d, want 4 (zero-quote must be filtered)", len(got))
	}
	for _, trade := range got {
		if trade.QuoteAmount.IsZero() {
			t.Error("zero-quote trade survived filter")
		}
	}
}

func TestFilterOutliers_FewValidAfterPrefilter(t *testing.T) {
	// Post-filter prices count is what matters for the "< 3"
	// short-circuit. 5 total, 3 invalid → only 2 usable → passthrough
	// (returning just the valid ones, not the invalid ones).
	trades := []canonical.Trade{
		mkTrade(0, 100), // invalid
		mkTrade(1, 101),
		mkTrade(1, 99),
		mkTrade(0, 999), // invalid
		mkTrade(1, 0),   // invalid (zero quote)
	}
	got := aggregate.FilterOutliers(trades, 3.0)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (only valid trades returned when σ can't be computed)", len(got))
	}
	for _, trade := range got {
		if trade.BaseAmount.IsZero() || trade.QuoteAmount.IsZero() {
			t.Error("invalid trade survived filter")
		}
	}
}

func TestFilterOutliers_IdenticalPricesAllKept(t *testing.T) {
	// σ = 0 edge case. Every price identical — no trade is an outlier
	// no matter the threshold.
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100),
	}
	got := aggregate.FilterOutliers(trades, 1.0)
	if len(got) != len(trades) {
		t.Errorf("stripped trades from a σ=0 distribution: %d/%d kept", len(got), len(trades))
	}
}

// TestFilterOutliers_MADCatchesMaskedOutlier is the masked-outlier proof: mean/sigma keeps a 2x fat finger in a small window, while median+MAD (MAD=0 here) drops it.
func TestFilterOutliers_MADCatchesMaskedOutlier(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100),
		mkTrade(1, 200), // masked outlier
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != 4 {
		t.Fatalf("MAD filter kept %d trades, want 4 (the masked 200 outlier must be dropped)", len(got))
	}
	for _, tr := range got {
		if tr.QuoteAmount.BigInt().Int64() >= 200 {
			t.Fatalf("masked outlier 200 survived the filter: quote=%s", tr.QuoteAmount.String())
		}
	}
}

// TestFilterOutliers_ZeroMADKeepsHonestDispersion asserts on the served VWAP that a zero-MAD count majority at one price does not erase honest price discovery (100.2, not 100).
func TestFilterOutliers_ZeroMADKeepsHonestDispersion(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100),
		mkTrade(1, 101), // honest print 1% off the majority price
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != len(trades) {
		t.Errorf("kept %d/%d — the 101 is honest dispersion, not an outlier", len(got), len(trades))
	}

	vwap, err := aggregate.VWAPOf(got)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	// (100+100+100+100+101) / 5 = 100.2 exactly.
	if want := big.NewRat(501, 5); vwap.Cmp(want) != 0 {
		t.Errorf("served VWAP = %s, want %s (%s) — dropping the 101 serves the majority price, not the market's",
			vwap.RatString(), want.RatString(), want.FloatString(2))
	}
}

// TestFilterOutliers_ZeroMADStillDropsFatFinger pins that the zero-MAD relative floor still rejects a print well outside it and is not a no-op.
func TestFilterOutliers_ZeroMADStillDropsFatFinger(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100), mkTrade(1, 100),
		mkTrade(1, 110), // 10% off — outside the ±2% band at sigma=4
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != 4 {
		t.Fatalf("kept %d/5, want 4 — a 10%% print off a zero-MAD majority is an outlier", len(got))
	}
	vwap, err := aggregate.VWAPOf(got)
	if err != nil {
		t.Fatalf("VWAP: %v", err)
	}
	if want := big.NewRat(100, 1); vwap.Cmp(want) != 0 {
		t.Errorf("served VWAP = %s, want 100 — the fat-finger print must not reach VWAP", vwap.RatString())
	}
}

// TestFilterOutliers_MADCleanWindowUnaffected pins that a tight window with genuine spread (MAD > 0) keeps every trade.
func TestFilterOutliers_MADCleanWindowUnaffected(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 100), mkTrade(1, 101), mkTrade(1, 99),
		mkTrade(1, 100), mkTrade(1, 102), mkTrade(1, 98),
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != len(trades) {
		t.Fatalf("MAD filter stripped %d trades from a clean spread", len(trades)-len(got))
	}
}

// TestFilterOutliers_DownAndUpOutliersBothDropped pins that a crash print and its mirror-image pump are both rejected, leaving the clean window's VWAP.
func TestFilterOutliers_DownAndUpOutliersBothDropped(t *testing.T) {
	// Base window: a genuinely dispersed pair (~1.25× step between
	// prints, relative MAD 25 %). Base amount 1 ⇒ price = quote.
	base := []int64{64, 80, 100, 125, 156}
	const cleanVWAPNum = 64 + 80 + 100 + 125 + 156 // over 5 base units

	cases := []struct {
		name    string
		outlier int64
	}{
		{"crash_print_below", 1},
		{"pump_print_above", 10_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trades := make([]canonical.Trade, 0, len(base)+1)
			for _, p := range base {
				trades = append(trades, mkTrade(1, p))
			}
			trades = append(trades, mkTrade(1, tc.outlier))

			got := aggregate.FilterOutliers(trades, 4.0)
			if len(got) != len(base) {
				t.Fatalf("kept %d/%d trades, want %d — the %d print is as outlying as its mirror image",
					len(got), len(trades), len(base), tc.outlier)
			}
			for _, tr := range got {
				if tr.QuoteAmount.BigInt().Int64() == tc.outlier {
					t.Fatalf("outlier %d survived the filter", tc.outlier)
				}
			}
			vwap, err := aggregate.VWAPOf(got)
			if err != nil {
				t.Fatalf("VWAP: %v", err)
			}
			want := big.NewRat(cleanVWAPNum, int64(len(base)))
			if vwap.Cmp(want) != 0 {
				t.Errorf("served VWAP = %s, want %s — the %d print must not reach VWAP",
					vwap.RatString(), want.RatString(), tc.outlier)
			}
		})
	}
}

// TestFilterOutliers_SymmetricWindowFullyKept pins that a merely dispersed, uncontaminated window keeps every print.
func TestFilterOutliers_SymmetricWindowFullyKept(t *testing.T) {
	trades := []canonical.Trade{
		mkTrade(1, 64), mkTrade(1, 80), mkTrade(1, 100),
		mkTrade(1, 125), mkTrade(1, 156),
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != len(trades) {
		t.Fatalf("kept %d/%d — an honest dispersed window must survive intact", len(got), len(trades))
	}
}

// TestFilterOutliers_TightDustClusterKeepsNearbyVolume pins the floor on a small but non-zero MAD: hundreds of dust prints within 0.01%
// must not turn a 0.3% move carrying the volume into outliers, which the volume-majority rule then turns into an empty window.
func TestFilterOutliers_TightDustClusterKeepsNearbyVolume(t *testing.T) {
	var trades []canonical.Trade
	for i := 0; i < 300; i++ {
		trades = append(trades, mkTrade(1, 100_000+int64(i%3)-1))
	}
	for i := 0; i < 5; i++ {
		trades = append(trades, mkTrade(1_000, 1_000*100_300))
	}
	got := aggregate.FilterOutliers(trades, 4.0)
	if len(got) != len(trades) {
		t.Fatalf("kept %d of %d trades; prints 0.3%% off a tight cluster are not outliers", len(got), len(trades))
	}
}
