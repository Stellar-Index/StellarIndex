package baseline

import (
	"errors"
	"math"
	"sort"
	"time"
)

// MADScale (1 / Φ⁻¹(0.75)) maps MAD to σ-equivalent units, so z=5 is a 5-σ-equivalent
// anomaly whether or not the distribution is Gaussian.
const MADScale = 1.4826

// MinSamples is the floor below which Median + MAD aren't meaningful; the not-yet-trained case
// is ADR-0019's bootstrap policy, a layer up.
const MinSamples = 2

// MinDriftSamples is the floor below which [Baseline.DriftZScore] is not trustworthy: it divides
// by MAD, and a small-n MAD that comes in low manufactures a large drift z from noise.
//
// Measured null false-positive rate (20k trials of zero-drift
// Gaussian returns, P(driftZ >= 5)):
//
//	n=2     12%      n=30    0.15%
//	n=5    7.5%      n=60    0.035%
//	n=10   1.4%      n=1440  0.005%
//
// 60 (one hour of 1m buckets) is the smallest round window within an order of magnitude of the
// asymptote, well below the 1,440 a healthy 1d window carries.
const MinDriftSamples = 60

// MinMAD floors the MAD (σ-equivalent return units; 1e-3 = 10 bps per bucket) before it divides
// either z-statistic. A pegged or quiet pair has MAD 0 or near it, which scored every rounding wiggle
// z=+Inf; via MaxZScore's max-across-windows that pinned confidence at 0 and the freeze's `z > 5` leg
// on forever. At 5σ the floor makes a 0.5% move from flat the smallest anomaly: an order of magnitude
// outside a healthy peg's ±10 bps, still catching a depeg, fat-finger or manipulated bucket.
const MinMAD = 1e-3

// ErrNotEnoughSamples means fewer than [MinSamples] returns; callers apply the bootstrap policy.
var ErrNotEnoughSamples = errors.New("baseline: not enough samples (need >= 2)")

// Baseline is the robust-statistics summary of a rolling window of returns: Median, MAD
// (1.4826-scaled) and the sample count N. MAD == 0 is real (a quiet peg); [Baseline.ZScore] floors it.
type Baseline struct {
	Median float64
	MAD    float64
	N      int
}

// FromReturns computes the robust-stats summary of bucket-to-bucket returns without mutating them.
// The caller filters NaN / Inf: dropping them silently here would mask upstream pipeline bugs.
func FromReturns(returns []float64) (Baseline, error) {
	if len(returns) < MinSamples {
		return Baseline{}, ErrNotEnoughSamples
	}

	median := Median(returns)
	mad := MAD(returns) // already scaled by MADScale internally

	return Baseline{
		Median: median,
		MAD:    mad,
		N:      len(returns),
	}, nil
}

// ZScore returns |x - Median| / max(MAD, [MinMAD]), in σ-equivalent units; see [MinMAD] for why
// the floor. Callers compare against ADR-0019's threshold of 5.
func (b Baseline) ZScore(x float64) float64 {
	return math.Abs(x-b.Median) / b.scale()
}

// scale is [Baseline.ZScore]'s denominator: MAD floored at [MinMAD] (an unreachable negative MAD
// included). [Baseline.DriftZScore] deliberately does NOT use it.
func (b Baseline) scale() float64 {
	if b.MAD < MinMAD {
		return MinMAD
	}
	return b.MAD
}

// DriftZScore scores the window's own persistent directional drift — the frog-boiling signal
// [Baseline.ZScore] cannot see, since each drifted return hides in the noise and the Median tracks it.
//
//	driftZ = |Median| * sqrt(N) / MAD
//
// An honest walk wanders ~MAD*sqrt(N) with median ~0; a sustained push accumulates Median*N, so drift
// grows significant as sqrt(N) while volatility does not. The MEDIAN numerator means one spike cannot
// fake a drift. sqrt(N) amplifies a partly-shifted median (30d: a 0.5%/day push needs ~60% coverage,
// +50% over 7 days ~23%), which is why the three windows are non-redundant.
//
// A drift stays visible until it ages out, so never gate a per-bucket decision on this; see
// [MultiBaseline.MaxDriftZScore]. ok is false when N < [MinDriftSamples]. MAD == 0 deliberately skips
// the [MinMAD] floor: it would hide a drift under 5·MinMAD·sqrt(N) per window (a 15%-over-30d push
// drops from z≈9 to z≈0.7). Zero Median scores 0; nonzero Median over zero MAD is +Inf.
func (b Baseline) DriftZScore() (float64, bool) {
	if b.N < MinDriftSamples {
		return 0, false
	}
	drift := math.Abs(b.Median)
	if b.MAD == 0 {
		if drift == 0 {
			return 0, true
		}
		return math.Inf(1), true
	}
	return drift * math.Sqrt(float64(b.N)) / b.MAD, true
}

// Median returns the 50th percentile of xs from a sorted copy; empty input returns 0.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)

	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	// float64 suffices: the inputs are already float64.
	return (cp[n/2-1] + cp[n/2]) / 2
}

// MAD returns 1.4826 * median(|xs[i] - median(xs)|), σ-equivalent for normal data (see
// [MADScale]). Empty or single-element input returns 0.
func MAD(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	med := Median(xs)
	devs := make([]float64, len(xs))
	for i, x := range xs {
		devs[i] = math.Abs(x - med)
	}
	return MADScale * Median(devs)
}

// BucketWidth is the width of one baseline bucket (the prices_1m CAGG's
// grain), and so the unit every [BucketReturn] is expressed in.
const BucketWidth = time.Minute

// BucketReturn is the fractional change of one closed bucket's VWAP over
// the previous non-empty bucket's VWAP, scaled to one [BucketWidth]: the
// unit every [Baseline] is trained in (see [ReturnsFromVWAPs]), and so the
// only observation [MultiBaseline.MaxZScore] accepts. A tick-to-tick delta
// of an overlapping rolling window is damped by bucket/window and must
// never be scored against it.
type BucketReturn struct{ frac float64 }

// NewBucketReturn returns curr's return over prev, two bucket VWAPs elapsed apart; ok is false
// when prev is zero. A k-bucket return has sqrt(k) times a one-bucket spread, so dividing by
// sqrt(elapsed/BucketWidth) puts sparse pairs in the one-minute unit; elapsed <= one bucket is unscaled.
func NewBucketReturn(prev, curr float64, elapsed time.Duration) (r BucketReturn, ok bool) {
	if prev == 0 {
		return BucketReturn{}, false
	}
	frac := (curr - prev) / prev
	if elapsed > BucketWidth {
		frac /= math.Sqrt(float64(elapsed) / float64(BucketWidth))
	}
	return BucketReturn{frac: frac}, true
}

// Fraction is the one-bucket-scaled return as a fraction (0.1 = +10%).
func (r BucketReturn) Fraction() float64 { return r.frac }

// ReturnsFromVWAPs converts oldest-first bucket VWAPs into one scaled [BucketReturn] fraction per
// consecutive priced pair (see [NewBucketReturn]), the input to [FromReturns]. A zero-VWAP bucket is
// skipped, not a -100% move. Returns nil with fewer than two priced buckets.
func ReturnsFromVWAPs(timed []TimedVWAP) []float64 {
	var out []float64
	prev := -1
	for i := range timed {
		if timed[i].VWAP == 0 {
			continue
		}
		if prev >= 0 {
			elapsed := timed[i].BucketEnd.Sub(timed[prev].BucketEnd)
			if r, ok := NewBucketReturn(timed[prev].VWAP, timed[i].VWAP, elapsed); ok {
				out = append(out, r.Fraction())
			}
		}
		prev = i
	}
	return out
}
