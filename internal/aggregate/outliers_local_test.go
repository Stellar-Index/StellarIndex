package aggregate

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Fixture helpers for the time-local filter. Prices are quote/base
// with base fixed at 10^7 so `quote` reads as price × 10^7 — the
// 2026-08-28 XLM/GBP shape (0.1337 → 0.1364) is 1_337_000 → 1_364_000.
const localFixtureBase = 10_000_000

func localTrade(source string, quote int64, ts time.Time) canonical.Trade {
	return canonical.Trade{
		Source:      source,
		Timestamp:   ts,
		BaseAmount:  canonical.NewAmount(big.NewInt(localFixtureBase)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// noisy returns centre ± up to ~0.1% of deterministic pseudo-noise so
// the majority regime has a realistic (non-zero) MAD — the shape
// under which the whole-window band is tightest and the drift
// artifact is worst.
func noisy(centre int64, i int) int64 {
	return centre + int64((i*7919)%21-10)*(centre/10_000)
}

// thinStepSeries is the 2026-08-28 XLM/GBP incident replayed on a
// THIN, SINGLE-SOURCE series: one Kraken print per minute for 5 h at
// 0.1337, then a genuine +2% step to 0.1364 that the last `tailMin`
// minutes hold (the cross XLM/USD × GBP/USD moved with it — every
// print after the step agrees). Returns the trades in time order and
// the index of the first post-step print.
func thinStepSeries(t0 time.Time, totalMin, tailMin int) ([]canonical.Trade, int) {
	trades := make([]canonical.Trade, 0, totalMin)
	stepAt := totalMin - tailMin
	for i := 0; i < totalMin; i++ {
		centre := int64(1_337_000)
		if i >= stepAt {
			centre = 1_364_000
		}
		trades = append(trades, localTrade("kraken", noisy(centre, i), t0.Add(time.Duration(i)*time.Minute)))
	}
	return trades, stepAt
}

func maxZ(z []*big.Rat) *big.Rat {
	var m *big.Rat
	for _, v := range z {
		if v != nil && (m == nil || v.Cmp(m) > 0) {
			m = v
		}
	}
	return m
}

func TestFilterOutliersLocal_GenuineStepOnThinSingleSourceSeriesIsNotTrimmed(t *testing.T) {
	// Regression for the 2026-08-28 drift artifact: a genuine +2%
	// step held by the newest 13% of a thin single-source window.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades, stepAt := thinStepSeries(t0, 300, 40)
	const sigma = 4.0

	// The fixture must actually reproduce the defect under the
	// whole-window filter — otherwise the assertions below are vacuous.
	legacy := FilterOutliers(trades, sigma)
	if dropped := len(trades) - len(legacy); dropped == 0 {
		t.Fatalf("fixture does not reproduce the drift artifact: whole-window filter dropped 0 of the %d-print tail", len(trades)-stepAt)
	} else {
		t.Logf("whole-window FilterOutliers trimmed %d prints of a %d-print agreed step (the defect)", dropped, len(trades)-stepAt)
	}

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: sigma})
	if len(got) != len(trades) {
		t.Errorf("local filter trimmed %d prints of an agreed +2%% step; want 0", len(trades)-len(got))
	}
	validIdx, z := outlierScores(trades, LocalOutlierOptions{Sigma: sigma})
	if len(validIdx) != len(trades) || len(z) != len(trades) {
		t.Fatalf("outlierScores: %d valid / %d scores, want %d", len(validIdx), len(z), len(trades))
	}
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if m := maxZ(z); m == nil || m.Cmp(sigmaRat) > 0 {
		f, _ := m.Float64()
		t.Errorf("max local z = %.3f exceeds sigma %v — an agreed step must not score as an outlier", f, sigma)
	}
	for k := stepAt; k < len(trades); k++ {
		if z[k].Cmp(sigmaRat) > 0 {
			f, _ := z[k].Float64()
			t.Errorf("post-step print %d scored z=%.3f > sigma", k, f)
		}
	}
	// The survivors must carry the new regime: the newest print's
	// price is the stepped level, so a window VWAP over the survivors
	// can follow the market instead of lagging it.
	if last := got[len(got)-1]; last.QuoteAmount.BigInt().Int64() < 1_360_000 {
		t.Errorf("newest survivor quote = %s, want the stepped regime (≥1_360_000)", last.QuoteAmount.String())
	}
}

func TestFilterOutliersLocal_SinglePrintSpikeOnThinSeriesIsTrimmed(t *testing.T) {
	// Same thin series, no step, ONE +10% print in the middle: that IS
	// an outlier (disagrees with the window AND its neighbours).
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades, _ := thinStepSeries(t0, 300, 0)
	const spikeAt = 150
	trades[spikeAt] = localTrade("kraken", 1_470_000, trades[spikeAt].Timestamp)
	const sigma = 4.0

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: sigma})
	if len(got) != len(trades)-1 {
		t.Fatalf("got %d survivors, want %d (exactly the spike removed)", len(got), len(trades)-1)
	}
	for _, tr := range got {
		if tr.QuoteAmount.BigInt().Int64() == 1_470_000 {
			t.Fatalf("spike print survived the local filter")
		}
	}
	_, z := outlierScores(trades, LocalOutlierOptions{Sigma: sigma})
	sigmaRat := new(big.Rat).SetFloat64(sigma)
	if z[spikeAt] == nil || z[spikeAt].Cmp(sigmaRat) <= 0 {
		t.Errorf("spike z = %v, want > sigma %v", z[spikeAt], sigma)
	}
	// Preserved input order among survivors.
	for k := 1; k < len(got); k++ {
		if got[k].Timestamp.Before(got[k-1].Timestamp) {
			t.Fatalf("survivor order not preserved at %d", k)
		}
	}
}

func TestFilterOutliersLocal_DenseMultiSourceTailAndFatFinger(t *testing.T) {
	// The design's red-proof shape: 8 400 old-regime + 1 600 new-regime
	// prints (16% tail) across three agreeing venues at ~50 prints/min,
	// plus ONE fat-finger 2× print inside the shift. Expect: 0 agreed
	// prints dropped, the fat-finger dropped.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	sources := []string{"binance", "coinbase", "kraken"}
	const total, tail = 10_000, 1_600
	trades := make([]canonical.Trade, 0, total+1)
	for i := 0; i < total; i++ {
		centre := int64(1_875_000) // 0.1875
		if i >= total-tail {
			centre = 1_828_125 // −2.5%
		}
		ts := t0.Add(time.Duration(i) * 1200 * time.Millisecond) // 50/min
		trades = append(trades, localTrade(sources[i%3], noisy(centre, i), ts))
	}
	fat := localTrade("kraken", 3_656_250, trades[total-tail/2].Timestamp) // 2× inside the shift
	trades = append(trades, fat)

	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(got) != total {
		t.Errorf("got %d survivors, want %d (only the fat-finger removed)", len(got), total)
	}
	for _, tr := range got {
		if tr.QuoteAmount.BigInt().Int64() == 3_656_250 {
			t.Fatalf("fat-finger survived")
		}
	}
}

func TestFilterOutliersLocal_MidBucketStepUsesNextBucketReference(t *testing.T) {
	// A step landing 48 s into a 1-minute bucket leaves the new-regime
	// prints a 20% minority of their OWN bucket; the following bucket
	// is their honest reference. 5 prints/min, two venues.
	t0 := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	trades := make([]canonical.Trade, 0, 600)
	stepAt := 60*5 + 4 // 60 full old buckets, then 4 old prints into bucket 61
	for i := 0; i < 600; i++ {
		centre := int64(1_337_000)
		if i >= stepAt {
			centre = 1_364_000
		}
		src := "kraken"
		if i%2 == 1 {
			src = "coinbase"
		}
		trades = append(trades, localTrade(src, noisy(centre, i), t0.Add(time.Duration(i)*12*time.Second)))
	}
	got := FilterOutliersLocal(trades, LocalOutlierOptions{Sigma: 4})
	if len(got) != len(trades) {
		t.Errorf("mid-bucket step: %d agreed prints trimmed, want 0", len(trades)-len(got))
	}
}

func TestFilterOutliersLocal_LegacyParityOnUntimedInputs(t *testing.T) {
	// Untimed trades (zero Timestamp — the /v1 handler tests' shape)
	// all share one bucket, so the local filter must reproduce the
	// whole-window verdicts exactly: masking case dropped, honest
	// dispersion kept, zero-base skipped, short input passed through.
	mk := func(quote int64) canonical.Trade { return localTrade("x", quote, time.Time{}) }
	masking := []canonical.Trade{mk(100), mk(100), mk(100), mk(100), mk(200)}
	if got := FilterOutliersLocal(masking, LocalOutlierOptions{Sigma: 4}); len(got) != 4 {
		t.Errorf("masking [100×4,200]: %d survivors, want 4", len(got))
	}
	honest := []canonical.Trade{mk(100), mk(100), mk(100), mk(100), mk(101)}
	if got := FilterOutliersLocal(honest, LocalOutlierOptions{Sigma: 4}); len(got) != 5 {
		t.Errorf("honest [100×4,101]: %d survivors, want 5", len(got))
	}
	zeroBase := localTrade("x", 100, time.Time{})
	zeroBase.BaseAmount = canonical.NewAmount(big.NewInt(0))
	withZero := append([]canonical.Trade{zeroBase}, honest...)
	if got := FilterOutliersLocal(withZero, LocalOutlierOptions{Sigma: 4}); len(got) != 5 {
		t.Errorf("zero-base: %d survivors, want 5", len(got))
	}
	for _, n := range []int{0, 1, 2} {
		if got := FilterOutliersLocal(honest[:n], LocalOutlierOptions{Sigma: 4}); len(got) != n {
			t.Errorf("n=%d passthrough: got %d", n, len(got))
		}
	}
	if got := FilterOutliersLocal(masking, LocalOutlierOptions{Sigma: 0}); len(got) != 5 {
		t.Errorf("sigma=0 must be a no-op, got %d", len(got))
	}
}
