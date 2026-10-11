package baseline

import (
	"errors"
	"math"
	"sort"
	"time"
)

// MADScale (1 / Φ⁻¹(0.75)) maps MAD to σ-equivalent units, so z=5 is a 5-σ anomaly for any distribution.
const MADScale = 1.4826

// MinSamples is the floor below which Median + MAD aren't meaningful; bootstrap policy is ADR-0019's, a layer up.
const MinSamples = 2

// MinDriftSamples is the floor for a trustworthy [Baseline.DriftZScore]: a low small-n MAD fakes drift (null P(z>=5) 12% at n=2, 0.035% at n=60).
const MinDriftSamples = 60

// MinMAD floors the MAD (1e-3 = 10 bps per bucket): a pegged pair's MAD of 0 scored rounding noise as z=+Inf, pinning confidence at 0 and the freeze on.
const MinMAD = 1e-3

// ErrNotEnoughSamples means fewer than [MinSamples] returns; callers apply the bootstrap policy.
var ErrNotEnoughSamples = errors.New("baseline: not enough samples (need >= 2)")

// Baseline summarises a window of returns: Median, 1.4826-scaled MAD and sample count N; MAD == 0 is real (a quiet peg) and floored by ZScore.
type Baseline struct {
	Median float64
	MAD    float64
	N      int
}

// FromReturns computes the baseline of returns without mutating them; the caller filters NaN/Inf so upstream bugs are not masked.
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

// ZScore returns |x - Median| / max(MAD, [MinMAD]) in σ-equivalent units.
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

// DriftZScore scores the window's persistent directional drift, |Median|*sqrt(N)/MAD, which [Baseline.ZScore] cannot see; ok is false below [MinDriftSamples].
func (b Baseline) DriftZScore() (float64, bool) {
	// A drift stays visible until it ages out, so never gate a per-bucket decision on this; see [MultiBaseline.MaxDriftZScore].
	// MAD == 0 deliberately skips the [MinMAD] floor (it would hide real drift); zero Median scores 0, nonzero Median over zero MAD is +Inf.
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

// MAD returns 1.4826 * median(|xs[i] - median(xs)|); empty or single-element input returns 0.
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

// BucketWidth is the width of one baseline bucket (the prices_1m grain) and the unit of every [BucketReturn].
const BucketWidth = time.Minute

// BucketReturn is a bucket-to-bucket VWAP return scaled to one [BucketWidth]; a rolling-window delta must never be scored against a [Baseline].
type BucketReturn struct{ frac float64 }

// NewBucketReturn returns curr's return over prev, scaled by sqrt(elapsed/BucketWidth) to one-minute units; ok is false when prev is zero.
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

// ReturnsFromVWAPs converts oldest-first bucket VWAPs into scaled returns, skipping zero-VWAP buckets; nil with fewer than two priced buckets.
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
