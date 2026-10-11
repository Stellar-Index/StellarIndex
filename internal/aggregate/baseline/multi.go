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

// MultiBaseline is a pair's baseline at 1d / 7d / 30d (ADR-0019 multi-window safeguard); a nil window is still bootstrapping.
type MultiBaseline struct {
	Day1  *Baseline
	Day7  *Baseline
	Day30 *Baseline
}

// NewMultiBaseline builds a [MultiBaseline] from three caller-sliced series (see [SplitByLookback]); a window with too few samples is nil.
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

// HasAnyValid reports whether any window has a baseline; if none, the pair is in full bootstrap and must not be used for anomaly detection.
func (m MultiBaseline) HasAnyValid() bool {
	return m.Day1 != nil || m.Day7 != nil || m.Day30 != nil
}

// MinZScoreSamples is the floor before a window votes in [MultiBaseline.MaxZScore]: a small-n MAD is noise (null P(z>=5) 14% at n=2, 0.004% at n=60).
const MinZScoreSamples = 60

// MaxZScore returns the largest z for bucket return r across windows with at least [MinZScoreSamples] returns (else the best-sampled one), and which window; valid is false in full bootstrap.
func (m MultiBaseline) MaxZScore(r BucketReturn) (z float64, window time.Duration, valid bool) {
	// Spikes only: return series keep no level memory, so frog-boiling needs [MultiBaseline.MaxDriftZScore] too.
	// NaN/±Inf return +Inf on the smallest window: NaN > 5 is false and a bad price would otherwise bypass the freeze.
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

// MaxDriftZScore returns the largest [Baseline.DriftZScore] across windows and which window; valid is false when none cleared [MinDriftSamples].
func (m MultiBaseline) MaxDriftZScore() (z float64, window time.Duration, valid bool) {
	// It takes no observation, so no later good bucket clears it: MUST NOT gate a per-bucket decision (publish, freeze, serve).
	// Use it for graded signals such as the confidence score; 1d/7d/30d form a ladder over attack duration.
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

// SplitByLookback slices oldest-first VWAPs into the 1d/7d/30d windows ending at now; slices share the input's memory, so callers MUST NOT mutate.
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

// TimedVWAP is one bucket VWAP stamped with its bucket end, aliased from [domain.BaselineTimedVWAP].
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
