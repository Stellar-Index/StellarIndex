package baseline_test

import (
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
)

// freezeThreshold mirrors orchestrator.DefaultPhase2ZScoreMinFreeze
// (ADR-0019 §"Freeze policy"). Duplicated as a literal rather than
// imported because internal/aggregate/orchestrator imports this
// package — the dependency may not run the other way.
const freezeThreshold = 5.0

// bucketsPerDay — 1m buckets in 24h, the cadence the baselines are
// trained on.
const driftBucketsPerDay = 1440

// noiseGen is a deterministic LCG (Knuth/MMIX constants): the scenarios need continuous noise, and math/rand's stream is not stable across Go releases.
type noiseGen uint64

// uniform returns the next value in [-1, 1).
func (g *noiseGen) uniform() float64 {
	*g = noiseGen(uint64(*g)*6364136223846793005 + 1442695040888963407)
	u := float64(uint64(*g)>>11) / float64(uint64(1)<<53) // [0, 1)
	return 2*u - 1
}

// driftedReturns builds days of 1m returns with uniform +/-noiseAmp noise; the last driftDays carry a constant
// push of driftPerDay/1440 per bucket (the frog-boiling profile).
func driftedReturns(days, driftDays int, driftPerDay, noiseAmp float64) []float64 {
	g := noiseGen(1)
	out := make([]float64, 0, days*driftBucketsPerDay)
	for d := 0; d < days; d++ {
		drifting := d >= days-driftDays
		for i := 0; i < driftBucketsPerDay; i++ {
			r := g.uniform() * noiseAmp
			if drifting {
				r += driftPerDay / driftBucketsPerDay
			}
			out = append(out, r)
		}
	}
	return out
}

// confidenceScoringZ mirrors the z the orchestrator hands confidence.Compute: the larger of spike score and drift score.
// It is the CONFIDENCE input only; the freeze leg uses observation-based MaxZScore because a drift score cannot be cleared by a good bucket.
func confidenceScoringZ(mb baseline.MultiBaseline, freshReturn float64) float64 {
	z, _, valid := mb.MaxZScore(bucketReturn(freshReturn))
	if !valid {
		z = 0
	}
	if dz, _, ok := mb.MaxDriftZScore(); ok && dz > z {
		z = dz
	}
	return z
}

// windowsFromReturns slices one long oldest-first return series into
// the 1d / 7d / 30d sub-windows the refresher would persist.
func windowsFromReturns(returns []float64) baseline.MultiBaseline {
	mk := func(rs []float64) *baseline.Baseline {
		b, err := baseline.FromReturns(rs)
		if err != nil {
			return nil
		}
		return &b
	}
	n := len(returns)
	return baseline.MultiBaseline{
		Day1:  mk(returns[n-1*driftBucketsPerDay:]),
		Day7:  mk(returns[n-7*driftBucketsPerDay:]),
		Day30: mk(returns),
	}
}

// TestFrogBoiling_SlowDriftReachesFreezeThreshold: a 0.5%/day push for 30 days keeps each bucket inside the noise, so
// MaxZScore sees ~0.05 at every window (undetectable by construction); the drift signal must reach the freeze threshold.
func TestFrogBoiling_SlowDriftReachesFreezeThreshold(t *testing.T) {
	const (
		driftPerDay = 0.005  // 0.5%/day — ~15% over the 30d window
		noiseAmp    = 0.0001 // 1bp/min quiet asset
	)
	returns := driftedReturns(30, 30, driftPerDay, noiseAmp)
	mb := windowsFromReturns(returns)

	// The attacker's next bucket looks exactly like every other one.
	freshReturn := driftPerDay / driftBucketsPerDay

	// Assert on the signal the orchestrator consumes, with no validity guard first, so a drift-blind detector fails on the number.
	got := confidenceScoringZ(mb, freshReturn)
	if got < freezeThreshold {
		t.Errorf("confidence scoring z = %.2f, want >= %.1f: a 15%%-over-30d sustained push must register as anomalous, not hide inside the return noise",
			got, freezeThreshold)
	}

	spikeZ, _, _ := mb.MaxZScore(bucketReturn(freshReturn))
	driftZ, window, ok := mb.MaxDriftZScore()
	if !ok {
		t.Fatal("MaxDriftZScore valid=false with three fully-populated windows")
	}
	t.Logf("spike z=%.3f (blind by construction) | drift z=%.2f from %v", spikeZ, driftZ, window)

	// The signal must come from the drift half — if this ever fails
	// because the spike score started carrying it, the detector
	// changed shape and the reasoning above needs revisiting.
	if driftZ <= spikeZ {
		t.Errorf("drift z (%.2f) <= spike z (%.2f): the drift statistic is supposed to be what detects this", driftZ, spikeZ)
	}

	// The 30d window is what catches a drift this patient: over 7d
	// the same push has not yet accumulated enough significance.
	if window != baseline.Window30d {
		t.Errorf("attributed window = %v, want %v (the long window is what a 30-day-old drift trips)", window, baseline.Window30d)
	}
}

// TestFrogBoiling_FasterDriftCaughtByShortWindow — a 2%/day ramp running for only a day is diluted in the 7d/30d medians
// but the 1d window has full coverage and trips; the three windows cover different attack durations.
func TestFrogBoiling_FasterDriftCaughtByShortWindow(t *testing.T) {
	returns := driftedReturns(30, 1, 0.025, 0.0001)
	mb := windowsFromReturns(returns)

	if got := confidenceScoringZ(mb, 0.025/driftBucketsPerDay); got < freezeThreshold {
		t.Errorf("confidence scoring z = %.2f, want >= %.1f for a 2.5%%/day ramp", got, freezeThreshold)
	}

	driftZ, window, ok := mb.MaxDriftZScore()
	if !ok {
		t.Fatal("MaxDriftZScore valid=false")
	}
	t.Logf("drift z=%.2f from %v", driftZ, window)
	if window != baseline.Window1d {
		t.Errorf("attributed window = %v, want %v (only the 1d window is fully covered by a one-day-old ramp)", window, baseline.Window1d)
	}
}

// TestDriftZScore_GenuineVolatilityDoesNotFire — a violently volatile but directionless asset (0.5%/min) must not
// register as drift: the moves cancel and the median stays ~0. Otherwise every volatile asset would freeze.
func TestDriftZScore_GenuineVolatilityDoesNotFire(t *testing.T) {
	returns := driftedReturns(30, 0, 0, 0.005) // no drift at all
	mb := windowsFromReturns(returns)

	driftZ, window, ok := mb.MaxDriftZScore()
	if !ok {
		t.Fatal("MaxDriftZScore valid=false")
	}
	t.Logf("drift z=%.2f from %v", driftZ, window)

	if driftZ >= freezeThreshold {
		t.Errorf("drift z = %.2f on a directionless 0.5%%/min series, want < %.1f — high volatility alone must not read as manipulation",
			driftZ, freezeThreshold)
	}
}

// rallyReturns builds `days` of returns for an asset that compounds
// to +totalGain over the period while carrying uniform +/-noiseAmp
// per-bucket volatility.
func rallyReturns(days int, totalGain, noiseAmp float64) []float64 {
	perDay := math.Expm1(math.Log(1+totalGain) / float64(days))
	return driftedReturns(days, days, perDay, noiseAmp)
}

// TestDriftZScore_GenuineRallyDoesNotFire — a real +50%/week rally with ordinary alt volatility stays clear of the
// freeze threshold: its own noise is the denominator, and only a smooth (stealthy) push scores high.
func TestDriftZScore_GenuineRallyDoesNotFire(t *testing.T) {
	b, err := baseline.FromReturns(rallyReturns(7, 0.50, 0.0025))
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	driftZ, ok := b.DriftZScore()
	if !ok {
		t.Fatal("DriftZScore valid=false")
	}
	t.Logf("genuine +50%%/week rally at ~105%% annualized volatility: drift z=%.2f", driftZ)

	if driftZ >= freezeThreshold {
		t.Errorf("drift z = %.2f on a genuine +50%%/week rally, want < %.1f — ordinary bull moves must not trip the manipulation defence",
			driftZ, freezeThreshold)
	}
}

// TestDriftZScore_QuietAssetLargeMoveFires_AcceptedFalsePositive pins the accepted false positive: the 7-day gain reaching z=5 scales with the asset's own volatility (+17% at ~21% annualized, +50% at ~54%, +325% at ~209%).
// It is safe only because drift feeds confidence alone; wired into the freeze it would never self-clear (see TestMaxDriftZScore_PersistsLongAfterTheMoveEnds).
// Unmeasured-liquidity pairs must pass confidence.LiquidityUnmeasured, or the confidence AND-leg is pinned true for non-USD pairs.
func TestDriftZScore_QuietAssetLargeMoveFires_AcceptedFalsePositive(t *testing.T) {
	// +25% in a week on a ~21%-annualized-volatility asset.
	b, err := baseline.FromReturns(rallyReturns(7, 0.25, 0.0005))
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	driftZ, ok := b.DriftZScore()
	if !ok {
		t.Fatal("DriftZScore valid=false")
	}
	t.Logf("quiet asset, +25%%/week: drift z=%.2f (fires — accepted posture)", driftZ)

	if driftZ < freezeThreshold {
		t.Errorf("drift z = %.2f, want >= %.1f: a 25%% weekly move on a peg-quiet asset must reach the threshold — that shape is indistinguishable from the attack and the graded response is correct",
			driftZ, freezeThreshold)
	}
}

// TestMaxDriftZScore_PersistsLongAfterTheMoveEnds is a hazard test: after +50% over 7 days then flat, observation z is ~0 but drift z stays above threshold until the move ages out of 30d,
// so gating publication on it would serve a stale price for weeks. The series is truncated at now, as RefreshPair's [now-30d, now] query does.
func TestMaxDriftZScore_PersistsLongAfterTheMoveEnds(t *testing.T) {
	const noise = 0.0005 // quiet asset, ~21% annualized
	const quietDaysAfter = 14

	returns := make([]float64, 0, 60*driftBucketsPerDay)
	returns = append(returns, driftedReturns(30, 0, 0, noise)...)             // quiet history
	returns = append(returns, rallyReturns(7, 0.50, noise)...)                // the repricing
	returns = append(returns, driftedReturns(quietDaysAfter, 0, 0, noise)...) // flat since

	levels := vwapSeries(1.0, returns)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timed := make([]baseline.TimedVWAP, len(levels))
	for i := range levels {
		timed[i] = baseline.TimedVWAP{VWAP: levels[i], BucketEnd: base.Add(time.Duration(i) * time.Minute)}
	}
	now := timed[len(timed)-1].BucketEnd
	mb := baseline.NewMultiBaseline(baseline.SplitByLookback(timed, now))

	driftZ, window, ok := mb.MaxDriftZScore()
	if !ok {
		t.Fatal("MaxDriftZScore valid=false")
	}
	// A perfectly ordinary next bucket for a flat asset.
	observedZ, _, _ := mb.MaxZScore(bucketReturn(0.0001))
	t.Logf("%d days after the move ended: drift z=%.2f (%v), observed z=%.2f",
		quietDaysAfter, driftZ, window, observedZ)

	if driftZ < freezeThreshold {
		t.Errorf("drift z = %.2f %d days after the move ended, want >= %.1f — this test exists to document that the drift score LATCHES; if it genuinely decays now, re-derive the freeze-path decision in computeConfidence rather than deleting this",
			driftZ, quietDaysAfter, freezeThreshold)
	}
	if observedZ >= freezeThreshold {
		t.Errorf("observed z = %.2f on a quiet bucket, want < %.1f", observedZ, freezeThreshold)
	}
	// The gap between the two IS the hazard: gate the freeze on the
	// left number and it stays engaged; gate it on the right and it
	// clears as soon as the market is calm.
	if driftZ <= observedZ {
		t.Errorf("expected drift z (%.2f) to exceed observed z (%.2f)", driftZ, observedZ)
	}
}

// TestDriftZScore_DetectedAtPartialWindowCoverage — a drift covering under half the window is not diluted away:
// the median shifts with the covered fraction and sqrt(N)=208 amplifies it, so a strong move is detected at about a fifth of the window.
func TestDriftZScore_DetectedAtPartialWindowCoverage(t *testing.T) {
	const noise = 0.0005

	// 7 drifting days inside a 30-day window = 23% coverage.
	returns := make([]float64, 0, 30*driftBucketsPerDay)
	returns = append(returns, driftedReturns(23, 0, 0, noise)...)
	returns = append(returns, rallyReturns(7, 0.50, noise)...)

	b, err := baseline.FromReturns(returns)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	driftZ, ok := b.DriftZScore()
	if !ok {
		t.Fatal("DriftZScore valid=false")
	}
	t.Logf("drift covering 7/30 days (23%% of the window): drift z=%.2f", driftZ)

	if driftZ < freezeThreshold {
		t.Errorf("drift z = %.2f at 23%% window coverage, want >= %.1f — partial coverage does not dilute a strong drift away",
			driftZ, freezeThreshold)
	}
}

// TestDriftZScore_SpikeCannotFakeDrift — one enormous bucket in a quiet window must not manufacture drift;
// pins MEDIAN over mean (ADR-0019), since a mean would let a single print trigger a freeze.
func TestDriftZScore_SpikeCannotFakeDrift(t *testing.T) {
	returns := driftedReturns(1, 0, 0, 0.0001)
	returns[len(returns)/2] = 5.0 // a single +500% print

	b, err := baseline.FromReturns(returns)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	driftZ, ok := b.DriftZScore()
	if !ok {
		t.Fatal("DriftZScore valid=false")
	}
	if driftZ >= freezeThreshold {
		t.Errorf("drift z = %.2f from a single outlier, want < %.1f — one spike is not a sustained drift (that is the spike detector's job)",
			driftZ, freezeThreshold)
	}
}

// TestDriftZScore_SmallSampleGate — a window with fewer than
// MinDriftSamples returns valid=false. At n=2 the measured null
// false-positive rate of driftZ>=5 is ~12%, so a sparse window must
// not be allowed to vote at all.
func TestDriftZScore_SmallSampleGate(t *testing.T) {
	// Every return identical and nonzero: the maximally drift-looking
	// input. Below the floor it must still refuse to report.
	few := make([]float64, baseline.MinDriftSamples-1)
	for i := range few {
		few[i] = 0.01
	}
	b, err := baseline.FromReturns(few)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	if z, ok := b.DriftZScore(); ok {
		t.Errorf("DriftZScore reported (z=%v, ok=true) at N=%d, want ok=false below MinDriftSamples=%d",
			z, b.N, baseline.MinDriftSamples)
	}

	// One sample more and it reports.
	enough := make([]float64, baseline.MinDriftSamples)
	for i := range enough {
		enough[i] = 0.01
	}
	b2, err := baseline.FromReturns(enough)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	if _, ok := b2.DriftZScore(); !ok {
		t.Errorf("DriftZScore ok=false at N=%d, want ok=true at MinDriftSamples", b2.N)
	}
}

// TestDriftZScore_ZeroMADConventions — mirrors ZScore's documented
// MAD==0 handling. A flat never-moving price (the common illiquid
// case) is NOT anomalous; a perfectly linear ramp is maximally so.
func TestDriftZScore_ZeroMADConventions(t *testing.T) {
	flat := make([]float64, baseline.MinDriftSamples)
	b, err := baseline.FromReturns(flat) // all zeros: median 0, MAD 0
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	z, ok := b.DriftZScore()
	if !ok || z != 0 {
		t.Errorf("flat series gave (z=%v, ok=%v), want (0, true) — an unmoving illiquid price is not manipulation", z, ok)
	}

	ramp := make([]float64, baseline.MinDriftSamples)
	for i := range ramp {
		ramp[i] = 0.001 // identical nonzero: median 0.001, MAD 0
	}
	b2, err := baseline.FromReturns(ramp)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	z2, ok2 := b2.DriftZScore()
	if !ok2 || !math.IsInf(z2, 1) {
		t.Errorf("perfectly linear ramp gave (z=%v, ok=%v), want (+Inf, true)", z2, ok2)
	}
}

// TestDriftZScore_MostlyUnchangedIlliquidPrice — a price that sits still and jumps occasionally has Median and MAD 0
// and must read as no drift, not +Inf, so thin pairs do not storm the freeze path with false positives.
func TestDriftZScore_MostlyUnchangedIlliquidPrice(t *testing.T) {
	returns := make([]float64, 4*baseline.MinDriftSamples)
	for i := range returns {
		if i%4 == 0 { // a quarter of buckets move, three quarters flat
			returns[i] = 0.02
		}
	}
	b, err := baseline.FromReturns(returns)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	z, ok := b.DriftZScore()
	if !ok {
		t.Fatal("DriftZScore valid=false")
	}
	if z != 0 {
		t.Errorf("drift z = %v on a mostly-unchanged illiquid series, want 0", z)
	}
}

// TestMaxDriftZScore_BootstrapAndAttribution — window bookkeeping:
// nothing valid reports valid=false (callers must not read 0 as
// "no drift"), and a partially-bootstrapped multi-baseline
// attributes to the window that actually produced the max.
func TestMaxDriftZScore_BootstrapAndAttribution(t *testing.T) {
	if _, _, ok := (baseline.MultiBaseline{}).MaxDriftZScore(); ok {
		t.Error("empty MultiBaseline reported valid=true")
	}

	// All three windows present, but only the 7d carries a drift.
	quiet, err := baseline.FromReturns(driftedReturns(1, 0, 0, 0.0001))
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	drifting, err := baseline.FromReturns(driftedReturns(7, 7, 0.02, 0.0001))
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	mb := baseline.MultiBaseline{Day1: &quiet, Day7: &drifting, Day30: &quiet}
	z, window, ok := mb.MaxDriftZScore()
	if !ok {
		t.Fatal("valid=false")
	}
	if window != baseline.Window7d {
		t.Errorf("window = %v, want %v (the only drifting window)", window, baseline.Window7d)
	}
	if z < freezeThreshold {
		t.Errorf("z = %.2f, want >= %.1f", z, freezeThreshold)
	}

	// A window below the sample floor must be skipped, not counted.
	short := make([]float64, baseline.MinDriftSamples-1)
	for i := range short {
		short[i] = 0.05 // would be +Inf if the floor were ignored
	}
	sb, err := baseline.FromReturns(short)
	if err != nil {
		t.Fatalf("FromReturns: %v", err)
	}
	mb2 := baseline.MultiBaseline{Day1: &sb, Day30: &quiet}
	z2, window2, ok2 := mb2.MaxDriftZScore()
	if !ok2 {
		t.Fatal("valid=false with a valid 30d window")
	}
	if window2 != baseline.Window30d {
		t.Errorf("window = %v, want %v — the sub-floor 1d window must not vote", window2, baseline.Window30d)
	}
	if math.IsInf(z2, 1) {
		t.Error("z = +Inf: the sub-floor window voted despite being below MinDriftSamples")
	}
}
