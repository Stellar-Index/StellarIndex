package baseline

import (
	"errors"
	"math"
	"sort"
)

// MADScale is the consistency factor that maps MAD to σ-equivalent
// units for normally-distributed data. Multiplying raw MAD by
// 1.4826 means a 5-σ-equivalent anomaly is at z=5 (matching the
// industry convention) regardless of whether the underlying
// distribution is actually Gaussian.
//
// Source: 1 / Φ⁻¹(0.75) ≈ 1.4826, where Φ⁻¹ is the inverse standard
// normal CDF.
const MADScale = 1.4826

// MinSamples is the floor below which Median + MAD aren't meaningful.
// At n=1 there's no variance to measure; at n=0 there's nothing at
// all. ADR-0019 §"Bootstrap (warmup) policy" handles the
// not-yet-trained case at a higher layer; this package just refuses
// to compute.
const MinSamples = 2

// MinDriftSamples is the floor below which [Baseline.DriftZScore] is
// not trustworthy. The drift statistic divides by MAD, so it inherits
// MAD's estimation error — and at small n a MAD that happens to come
// in low manufactures a large drift z out of pure noise.
//
// Measured null false-positive rate (20k trials of zero-drift
// Gaussian returns, P(driftZ >= 5)):
//
//	n=2     12%      n=30    0.15%
//	n=5    7.5%      n=60    0.035%
//	n=10   1.4%      n=1440  0.005%
//
// 60 is the smallest round window whose null rate is within an order
// of magnitude of the asymptote, and it carries a natural domain
// meaning: 60 one-minute buckets = one hour of continuous trading.
// Well below the 1,440 a healthy 1d window carries, so this gates
// only genuinely sparse windows — never a real one.
const MinDriftSamples = 60

// MinMAD is the floor placed under the MAD before it is used as the
// denominator of either z-statistic (COR-01). It is expressed in the
// same σ-equivalent RETURN units as [Baseline.MAD]: 1e-3 = 10 basis
// points of per-bucket return.
//
// Why a floor is required. MAD is 0 whenever a strict MAJORITY of the
// window's bucket-to-bucket returns are exactly 0 — which is the
// NORMAL state of a tightly-pegged stablecoin, and of any pair quiet
// enough that most 1-minute buckets reprint the same VWAP. Dividing
// by that 0 made every nonzero move, down to a 1e-9 rounding wiggle,
// score z=+Inf. [MultiBaseline.MaxZScore] then takes the max ACROSS
// windows, so one quiet window was enough to pin the pair at +Inf,
// which drives ZScoreFactor to 0, the whole confidence score to 0 (a
// geometric mean), and satisfies the Phase 2 freeze's `z > 5` leg
// forever. A near-zero-but-nonzero MAD produces the same failure less
// visibly (MAD=1e-9 turns a 1e-6 move into z=1000), so the floor
// applies to any MAD below it, not just to exactly zero.
//
// Why 1e-3. The floor is the spread we are willing to assert a quiet
// window really has, and ADR-0019 triggers at 5σ — so the floor is
// equivalent to choosing the smallest single-bucket move that counts
// as anomalous for an asset that has been perfectly flat. At 1e-3
// that move is 0.5%, which is an order of magnitude outside the
// ±10 bps band a healthy USD peg trades in (so a quiet peg stops
// self-triggering) while still catching a genuine depeg, a fat-finger
// print, or a manipulated bucket. Tighter values keep the false
// positive; looser ones start hiding real moves, so this errs toward
// the sensitive side of the trade-off.
const MinMAD = 1e-3

// ErrNotEnoughSamples is what [FromReturns] returns when the input
// has fewer than [MinSamples] elements. Callers translate this into
// "use the bootstrap policy" rather than treating it as an error.
var ErrNotEnoughSamples = errors.New("baseline: not enough samples (need >= 2)")

// Baseline is the robust-statistics summary of a rolling window of
// returns, ready to score a new observation against.
//
// Population semantics: Median is the 50th percentile; MAD is the
// 1.4826-scaled median absolute deviation. N is the count that fed
// the computation — useful for downstream baseline_quality factors
// (ADR-0019 §"Multi-factor confidence score").
//
// MAD == 0 is a real outcome — it means every observation in the
// window was identical (think a tightly-pegged stablecoin during a
// quiet period). [Baseline.ZScore] handles that explicitly.
type Baseline struct {
	Median float64
	MAD    float64
	N      int
}

// FromReturns computes the robust-stats summary of a slice of
// bucket-to-bucket percent changes. The input is NOT mutated.
//
// Returns [ErrNotEnoughSamples] when len(returns) < [MinSamples].
// Caller is responsible for filtering NaN / Inf out of the input
// — this function does not silently drop pathological inputs
// because that would mask data-pipeline bugs upstream.
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

// ZScore returns the standardised distance from the baseline
// median, in σ-equivalent units:
//
//	z = |x - Median| / max(MAD, MinMAD)
//
// The denominator is floored at [MinMAD] (COR-01). A quiet or pegged
// window reports MAD at or near 0, and the unfloored ratio scored a
// sub-basis-point wiggle as +Inf — see [MinMAD] for why that is a
// false positive rather than a detection. x == Median still returns
// 0, and a genuinely large move from a flat baseline still scores far
// above any sane threshold; only the noise band changed.
//
// Callers compare against threshold = 5 per ADR-0019 §"5σ trigger"
// to gate confidence-score factors and freeze decisions.
func (b Baseline) ZScore(x float64) float64 {
	return math.Abs(x-b.Median) / b.scale()
}

// scale is the MAD actually used as [Baseline.ZScore]'s denominator:
// the measured MAD, floored at [MinMAD]. [Baseline.DriftZScore]
// deliberately does NOT use it — see that method for why. A negative
// MAD is not reachable (MAD is a median of absolute deviations) but
// is folded into the same floor rather than trusted.
func (b Baseline) scale() float64 {
	if b.MAD < MinMAD {
		return MinMAD
	}
	return b.MAD
}

// DriftZScore scores the window's own *persistent directional drift*
// — the frog-boiling signal that [Baseline.ZScore] structurally
// cannot see.
//
//	driftZ = |Median| * sqrt(N) / MAD
//
// Why the per-observation z-score is blind to a slow drift: ZScore
// asks "is THIS bucket's return unusual for this window?", and a
// frog-boiling attacker's whole method is to keep every individual
// return well inside the noise. Worse, the window's own Median
// tracks the drift, so the attacker's returns are scored against a
// baseline the attacker is moving — the drift is normalised away and
// z stays ~0 no matter how far the price has travelled. Adding more
// window lengths does not help: every window scores the same small
// per-bucket return (ADR-0019's "the 30d window still sees pre-attack
// data" only holds for price LEVELS; in return space there is no
// level memory to preserve).
//
// The drift statistic escapes that trap by never comparing the price
// to a baseline at all. It asks a question the attacker cannot
// normalise away: is the drift PERSISTENT relative to the window's
// own noise? Under an honest random walk the price wanders ~MAD*sqrt(N)
// over N buckets in no particular direction, and the median return is
// ~0. A sustained one-directional push instead accumulates Median*N.
// The ratio of the two is the classic drift-vs-diffusion statistic
// above, which grows as sqrt(N) — so a persistent drift becomes
// arbitrarily significant given enough buckets, while honest
// volatility does not.
//
// This puts the attacker in a squeeze with no escape: suppressing
// per-bucket moves to stay under ZScore keeps MAD small, which is the
// denominator here. Concretely, the largest cumulative displacement
// that can hide under a threshold T is ~T*MAD*sqrt(N) per window —
// a bound that scales with the asset's own volatility rather than an
// operator-chosen percentage, matching ADR-0019 §Consequences
// ("no operator picks the right percentage").
//
// Robustness: the numerator is the MEDIAN return, not the mean, so a
// single spike cannot fake a drift (ADR-0019 §Alternatives-considered
// rejects mean/stddev for exactly this reason).
//
// Partial coverage does NOT simply dilute away. The median shifts
// roughly in proportion to the fraction of the window the drift
// covers, and sqrt(N) (208 at the 30d scale) then amplifies what is
// left, so how much coverage is needed depends on the drift's
// strength rather than on a 50% majority. Measured on the 30d window:
// a weak 0.5%/day push needs ~60% coverage to reach z=5, while a
// stronger +50%-over-7-days move is still detected at ~23% coverage.
// Treat "it must cover most of the window" as false.
//
// This is also why [MultiBaseline.MaxDriftZScore]'s three scales are
// genuinely non-redundant rather than three views of the same number:
// they trade coverage against sqrt(N).
//
// Consequence for callers: because a drift stays visible long after
// it ends (it must age out of the window, not merely stop), this
// statistic is unsuitable for gating a per-bucket decision. See
// [MultiBaseline.MaxDriftZScore].
//
// Returns (_, false) when N < [MinDriftSamples] — see that constant
// for the measured small-sample false-positive rates.
//
// MAD == 0 keeps its own convention here — it deliberately does NOT
// take [Baseline.ZScore]'s [MinMAD] floor. The drift statistic's
// whole squeeze is that suppressing per-bucket moves to stay under
// ZScore keeps MAD small, and MAD is this denominator: flooring it
// would let an attacker buy immunity by simply drifting below
// 5·[MinMAD]·sqrt(N) per window, which is exactly the frog-boiling
// case this exists to catch (measured: a real 15%-over-30d push
// drops from z≈9 to z≈0.7 under a 1e-3 floor). The false positive
// the floor removes from ZScore is also absent here — the realistic
// quiet shape (price sits still in most buckets) has Median == 0 and
// scores 0. So: a zero Median is 0 (a flat, never-moving price — the
// common illiquid case, and NOT anomalous), a nonzero Median with
// zero MAD is +Inf (every return in the window identical and nonzero
// is a perfectly linear ramp — no real market does that).
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

// Median returns the 50th percentile of `xs`. The input is NOT
// mutated; an internal copy is sorted. Empty input returns 0
// (callers should use [FromReturns] which checks [MinSamples]
// before delegating here).
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
	// Even count: arithmetic mean of the two middle values. Done
	// in float64 because the inputs are already float64 — adding
	// big.Rat would be precision theatre at this layer.
	return (cp[n/2-1] + cp[n/2]) / 2
}

// MAD returns the 1.4826-scaled median absolute deviation:
//
//	MAD = 1.4826 * median( |xs[i] - median(xs)| )
//
// The 1.4826 factor (see [MADScale]) converts to σ-equivalent for
// normal data — so [Baseline.ZScore] reads as "σ-equivalents from
// median" without further conversion.
//
// Empty input returns 0. Single-element input also returns 0
// (no spread to measure — degenerate but well-defined).
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

// ReturnsFromVWAPs converts a chronologically-ordered slice of
// bucket VWAP values into bucket-to-bucket percent changes:
//
//	returns[i] = (vwaps[i+1] - vwaps[i]) / vwaps[i]
//
// Returns nil when len(vwaps) < 2 — there's no return to compute
// for a single observation. Buckets with vwaps[i] == 0 are skipped
// (division by zero); the resulting slice may be shorter than
// len(vwaps)-1.
//
// Used as the input to [FromReturns] when building a baseline from
// a stored window of 1m VWAP buckets.
func ReturnsFromVWAPs(vwaps []float64) []float64 {
	if len(vwaps) < 2 {
		return nil
	}
	out := make([]float64, 0, len(vwaps)-1)
	for i := 0; i < len(vwaps)-1; i++ {
		prev := vwaps[i]
		if prev == 0 {
			continue
		}
		out = append(out, (vwaps[i+1]-prev)/prev)
	}
	return out
}
