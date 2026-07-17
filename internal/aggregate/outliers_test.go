package aggregate_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestFilterOutliers_DropsFatTail(t *testing.T) {
	// σ-thresholds have a known weakness: a single huge outlier
	// inflates σ enough that it can't remove itself. Use enough
	// baseline samples (20) so the mean isn't dragged too far by
	// the outlier.
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

// TestFilterOutliers_MADCatchesMaskedOutlier is the finding-M5 proof:
// on a SMALL window the single-pass mean/σ filter is masking-vulnerable
// and rejects nothing, whereas the median+MAD filter catches the
// injected outlier.
//
// Window: four clean prints at 100 + one fat-finger at 200 (a 2x
// outlier). Mean/σ: mean = 120, sample stdev ≈ 44.7, so even a 4σ
// band ≈ 178.9 — the 200 is only 80 off the mean and SURVIVES (the
// outlier inflated σ enough to escape its own rejection: masking).
// Median+MAD: median = 100, MAD = 0 (a strict majority are identical),
// so any deviation from the majority centre is an outlier and the 200
// is dropped.
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

// TestFilterOutliers_MADCleanWindowUnaffected is the companion "clean
// window is unaffected" half of the M5 proof: a small window with
// genuine, tight spread (MAD > 0) keeps every trade.
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
