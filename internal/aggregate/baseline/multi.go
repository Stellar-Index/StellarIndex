package baseline

import (
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// Window lengths of the ADR-0019 multi-window safeguard, also each window's query lookback.
const (
	Window1d  = 24 * time.Hour
	Window7d  = 7 * 24 * time.Hour
	Window30d = 30 * 24 * time.Hour
)

// MultiBaseline is a pair's baseline at 1d / 7d / 30d (ADR-0019 multi-window safeguard).
// [MultiBaseline.MaxZScore] catches spikes; [MultiBaseline.MaxDriftZScore] catches frog-boiling, and is
// not optional: return series keep no level memory, so no window length makes MaxZScore see a drift.
// A nil window is still bootstrapping, without invalidating the others.
type MultiBaseline struct {
	Day1  *Baseline
	Day7  *Baseline
	Day30 *Baseline
}

// NewMultiBaseline builds a [MultiBaseline] from three caller-sliced VWAP series (see
// [SplitByLookback]); a window with too few samples is nil.
func NewMultiBaseline(vwapsDay1, vwapsDay7, vwapsDay30 []TimedVWAP) MultiBaseline {
	return MultiBaseline{
		Day1:  buildOrNil(vwapsDay1),
		Day7:  buildOrNil(vwapsDay7),
		Day30: buildOrNil(vwapsDay30),
	}
}

// buildOrNil returns FromReturns(ReturnsFromVWAPs(vwaps)), or nil on too few samples.
func buildOrNil(vwaps []TimedVWAP) *Baseline {
	b, err := FromReturns(ReturnsFromVWAPs(vwaps))
	if err != nil {
		return nil
	}
	return &b
}

// HasAnyValid reports whether any window has a baseline; if none does, the pair is in full
// bootstrap and must not be used for anomaly detection.
func (m MultiBaseline) HasAnyValid() bool {
	return m.Day1 != nil || m.Day7 != nil || m.Day30 != nil
}

// MinZScoreSamples is the sample floor before a window's [Baseline.ZScore] votes in
// [MultiBaseline.MaxZScore]: a MAD from a handful of returns is noise, and a low draw flags ordinary moves.
//
// Measured null false-positive rate (200k trials: baseline from n
// Gaussian returns, one fresh draw from the same distribution, MAD well
// above [MinMAD], P(z >= 5)):
//
//	n=2    14%       n=20   0.13%
//	n=3    19%       n=30   0.041%
//	n=5    6.8%      n=60   0.004%
//	n=10   0.97%     n=100  <0.005% (20k trials, none)
//
// 60 matches [MinDriftSamples] (one hour of 1m buckets) and is the first
// round window within an order of magnitude of zero.
const MinZScoreSamples = 60

// MaxZScore returns the largest z for bucket return r across windows with at least
// [MinZScoreSamples] returns (else the best-sampled one alone), and which window it was. SPIKES only:
// callers needing the frog-boiling defence also take [MultiBaseline.MaxDriftZScore]. valid is false in
// full bootstrap. NaN/±Inf return +Inf on the smallest window: NaN > 5 is false, so a bad price would
// otherwise bypass the freeze.
func (m MultiBaseline) MaxZScore(r BucketReturn) (z float64, window time.Duration, valid bool) {
	if !m.HasAnyValid() {
		return 0, 0, false
	}
	x := r.Fraction()
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// Max-anomalous so threshold checks fire, attributed to the smallest available window.
		switch {
		case m.Day1 != nil:
			return math.Inf(1), Window1d, true
		case m.Day7 != nil:
			return math.Inf(1), Window7d, true
		default:
			return math.Inf(1), Window30d, true
		}
	}
	for _, v := range m.zScoreVoters() {
		zb := v.b.ZScore(x)
		if !valid || zb > z {
			z, window, valid = zb, v.window, true
		}
	}
	return z, window, valid
}

// windowBaseline is one window's baseline tagged with its lookback.
type windowBaseline struct {
	b      *Baseline
	window time.Duration
}

// zScoreVoters returns the windows whose sample count clears
// [MinZScoreSamples], or, when none does, the one with the most samples
// (the longer window on a tie) so a young pair is still scored.
func (m MultiBaseline) zScoreVoters() []windowBaseline {
	var voters []windowBaseline
	var largest windowBaseline
	for _, c := range []windowBaseline{{m.Day1, Window1d}, {m.Day7, Window7d}, {m.Day30, Window30d}} {
		if c.b == nil {
			continue
		}
		if c.b.N >= MinZScoreSamples {
			voters = append(voters, c)
		}
		if largest.b == nil || c.b.N >= largest.b.N {
			largest = c
		}
	}
	if len(voters) == 0 && largest.b != nil {
		return []windowBaseline{largest}
	}
	return voters
}

// MaxDriftZScore returns the largest [Baseline.DriftZScore] across windows, and which window; it
// takes no observation because drift is a property of the training window. MUST NOT gate a per-bucket
// decision (publish, freeze, serve): no later good bucket clears it, so one real +50%-over-7-days
// repricing held it above 5 for 30 days. Use it for graded signals such as the confidence score.
// Drift must cover most of a window, so 1d/7d/30d form a ladder over attack duration and an attacker
// must evade all three. valid is false when no window cleared [MinDriftSamples].
func (m MultiBaseline) MaxDriftZScore() (z float64, window time.Duration, valid bool) {
	consider := func(b *Baseline, w time.Duration) {
		if b == nil {
			return
		}
		zb, ok := b.DriftZScore()
		if !ok {
			return
		}
		if !valid || zb > z {
			z, window, valid = zb, w, true
		}
	}
	consider(m.Day1, Window1d)
	consider(m.Day7, Window7d)
	consider(m.Day30, Window30d)
	return z, window, valid
}

// SplitByLookback slices oldest-first timed VWAPs into the 1d, 7d and 30d windows ending at now
// (bucketEnd >= cutoff, inclusive), so one storage read serves all three. Slices share the input's
// memory — callers MUST NOT mutate.
func SplitByLookback(timed []TimedVWAP, now time.Time) (day1, day7, day30 []TimedVWAP) {
	cut1 := now.Add(-Window1d)
	cut7 := now.Add(-Window7d)
	cut30 := now.Add(-Window30d)

	// Linear scans: at most 43,200 entries, once per refresh cycle.
	day1 = timed[firstAtOrAfter(timed, cut1):]
	day7 = timed[firstAtOrAfter(timed, cut7):]
	day30 = timed[firstAtOrAfter(timed, cut30):]
	return day1, day7, day30
}

// TimedVWAP is one bucket VWAP stamped with its bucket end. Canonical in
// [domain.BaselineTimedVWAP] so internal/storage/timescale need not import upward.
type TimedVWAP = domain.BaselineTimedVWAP

// firstAtOrAfter returns the first index with BucketEnd >= cutoff, or len(timed).
func firstAtOrAfter(timed []TimedVWAP, cutoff time.Time) int {
	for i := range timed {
		if !timed[i].BucketEnd.Before(cutoff) {
			return i
		}
	}
	return len(timed)
}
