package baseline

import (
	"math"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// Window-length constants used by the multi-window safeguard
// per ADR-0019. The aggregator's storage layer uses these as the
// query lookback for each per-window VWAP fetch.
const (
	Window1d  = 24 * time.Hour
	Window7d  = 7 * 24 * time.Hour
	Window30d = 30 * 24 * time.Hour
)

// MultiBaseline is the per-pair statistical baseline computed at
// three time scales (1d / 7d / 30d) per ADR-0019 §"Multi-window
// safeguard against frog-boiling".
//
// The defence has two halves, and they detect different things:
//
//   - [MultiBaseline.MaxZScore] scores one bucket's return against
//     each window. This catches sudden spikes.
//   - [MultiBaseline.MaxDriftZScore] scores each window's own
//     persistent drift. This catches frog-boiling.
//
// The second half is not optional. ADR-0019 originally reasoned that
// a slow drift "defeats the 1d window, but the 30d window still
// includes pre-attack data, so it still flags the drifted price".
// That holds for price LEVELS — but these baselines are built from
// bucket-to-bucket RETURNS, which keep no level memory. A drift of
// 0.5%/day is a per-bucket return of ~3e-6, far inside every
// window's return-MAD, so all three windows score it ~0 no matter
// how far the price has actually travelled, and the window medians
// drift along with the attack besides. Adding window lengths cannot
// recover a signal that is not in the return series; see
// [Baseline.DriftZScore] for the statistic that is.
//
// Conversely, a legitimate regime change (asset matures, gains
// liquidity) shows up proportionally across all three windows, so
// no single window fires a false positive.
//
// Each window field is a pointer so a "not enough data" outcome at
// one scale doesn't invalidate the whole struct — the 1d and 7d
// can be `nil` early in an asset's lifetime while the 30d is
// already valid (or vice versa during data-gap recovery).
type MultiBaseline struct {
	Day1  *Baseline
	Day7  *Baseline
	Day30 *Baseline
}

// NewMultiBaseline builds a [MultiBaseline] from three pre-sliced
// VWAP series — one per window. The caller is responsible for
// slicing to the right duration; this constructor just runs each
// slice through [ReturnsFromVWAPs] → [FromReturns] and stores the
// result (or `nil` when [ErrNotEnoughSamples] fires).
//
// Callers that have a single time-stamped VWAP series can use
// [SplitByLookback] to derive the three sub-slices.
func NewMultiBaseline(vwapsDay1, vwapsDay7, vwapsDay30 []float64) MultiBaseline {
	return MultiBaseline{
		Day1:  buildOrNil(vwapsDay1),
		Day7:  buildOrNil(vwapsDay7),
		Day30: buildOrNil(vwapsDay30),
	}
}

// buildOrNil runs FromReturns(ReturnsFromVWAPs(vwaps)) and returns
// either a pointer to the Baseline or nil if the window had too few
// samples. Used internally so each window's bootstrap state is
// observable on the wrapper struct.
func buildOrNil(vwaps []float64) *Baseline {
	b, err := FromReturns(ReturnsFromVWAPs(vwaps))
	if err != nil {
		return nil
	}
	return &b
}

// HasAnyValid reports whether at least one window had enough
// samples to compute a baseline. A multi-baseline with all three
// windows nil is in full bootstrap (ADR-0019 §"Bootstrap policy")
// and the caller should not consult it for anomaly detection.
func (m MultiBaseline) HasAnyValid() bool {
	return m.Day1 != nil || m.Day7 != nil || m.Day30 != nil
}

// MaxZScore returns the largest z-score for `x` across every
// window that has a valid baseline, plus the window's lookback
// duration so callers can attribute "which window detected this".
//
// "Fires when any window flags as anomalous" (ADR-0019) maps to
// `MaxZScore(x) >= threshold`. The 1d window dominates for
// sudden-spike detection (the longer windows average a single spike
// out among 30k samples).
//
// This method detects SPIKES only. It is structurally blind to a
// slow drift at every window length — the drifted returns are tiny
// and each window's median has moved with them. Callers that need
// the frog-boiling defence must also consult
// [MultiBaseline.MaxDriftZScore] and take the max; see
// [Baseline.DriftZScore].
//
// The third return value is `valid` — false when [HasAnyValid]
// would return false. Callers branch to the bootstrap policy on
// !valid rather than treating the zero z-score as a real reading.
//
// Pathological observations (NaN or ±Inf for `x`) return
// (+Inf, the smallest available window, true). Reasoning: such
// inputs indicate an upstream pipeline anomaly — almost certainly
// a bad price — and the orchestrator's freeze threshold check
// (`z > ZScoreMinFreeze`, default 5) MUST fire on them. Without
// this guard a NaN observation would silently bypass the Phase 2
// freeze (NaN > 5 is false in IEEE 754) and let an obviously-bad
// price through. We pick "smallest window" so attribution points
// to the most precise scale the caller has wired.
func (m MultiBaseline) MaxZScore(x float64) (z float64, window time.Duration, valid bool) {
	if !m.HasAnyValid() {
		return 0, 0, false
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// Treat pathological inputs as max-anomalous so downstream
		// threshold checks fire. Window attribution: pick the
		// smallest available — operators see "1d" (or fall back to
		// 7d / 30d) rather than 0, matching the "this window
		// detected it" doc contract for legitimate anomalies.
		switch {
		case m.Day1 != nil:
			return math.Inf(1), Window1d, true
		case m.Day7 != nil:
			return math.Inf(1), Window7d, true
		default:
			return math.Inf(1), Window30d, true
		}
	}
	if b := m.Day1; b != nil {
		zb := b.ZScore(x)
		if !valid || zb > z {
			z, window, valid = zb, Window1d, true
		}
	}
	if b := m.Day7; b != nil {
		zb := b.ZScore(x)
		if !valid || zb > z {
			z, window, valid = zb, Window7d, true
		}
	}
	if b := m.Day30; b != nil {
		zb := b.ZScore(x)
		if !valid || zb > z {
			z, window, valid = zb, Window30d, true
		}
	}
	return z, window, valid
}

// MaxDriftZScore returns the largest [Baseline.DriftZScore] across
// every window that has one, plus the window it came from. This is
// the working half of ADR-0019 §"Multi-window safeguard against
// frog-boiling": [MultiBaseline.MaxZScore] scores a single bucket's
// return and is blind to a slow drift at EVERY window length (see
// [Baseline.DriftZScore] for why more windows don't help there);
// this method scores the drift itself.
//
// Unlike MaxZScore it takes no observation — the signal is a property
// of the training window, not of the newest bucket.
//
// That difference is load-bearing, so read it before wiring this
// anywhere: MUST NOT gate a per-bucket decision (publication,
// freezing, serving). Because the result is a pure function of the
// persisted baseline row, a decision gated on it cannot be cleared by
// a subsequent good bucket — it stays engaged until the drift ages
// out of all three windows. Measured on the real path, one genuine
// +50%-over-7-days repricing of a quiet asset holds this above 5 for
// 30 days, 24 of them after the move already finished. Requiring the
// current bucket to corroborate does not rescue it either: that means
// MaxZScore is over the threshold too, which is just the spike check.
//
// Use it for graded, reversible signals — the confidence score, which
// is what the orchestrator does — where a stale-by-a-few-hours
// reading costs a slightly pessimistic number rather than a month of
// frozen price.
//
// The three window lengths are genuinely complementary here, which
// is what the return-space version only claimed to be. A window
// detects a drift once the drift covers most of it (the median needs
// a majority) and its sensitivity grows as sqrt(N), so the windows
// form a detection ladder over attack duration: 1d catches a fast
// ramp within hours, 7d a multi-day push, 30d the patient
// weeks-long drift that is invisible to both shorter scales. Taking
// the max means the attacker must evade all three.
//
// `valid` is false when no window cleared [MinDriftSamples] — the
// caller keeps whatever return-based signal it has rather than
// treating 0 as "no drift".
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

// SplitByLookback partitions a chronologically-ordered (oldest-first)
// series of (vwap, bucketEnd) pairs into three slices: one for each
// rolling lookback window ending at `now`.
//
// The 1d slice contains entries with bucketEnd >= now-1d; the 7d
// slice >= now-7d; the 30d slice >= now-30d. Slices share backing
// memory with the input — callers MUST NOT mutate. A bucket
// timestamp equal to the cutoff IS included (half-open `[start, now]`).
//
// This is a helper for callers who store the full 30d series and
// want to derive sub-window views without three round trips to the
// storage layer.
func SplitByLookback(timed []TimedVWAP, now time.Time) (day1, day7, day30 []float64) {
	cut1 := now.Add(-Window1d)
	cut7 := now.Add(-Window7d)
	cut30 := now.Add(-Window30d)

	// Find the first index >= each cutoff. Linear scans are fine —
	// the 30-day series is bounded at 43,200 entries (1m buckets) and
	// runs once per refresh cycle, not per request.
	idx1 := firstAtOrAfter(timed, cut1)
	idx7 := firstAtOrAfter(timed, cut7)
	idx30 := firstAtOrAfter(timed, cut30)

	day1 = vwapsFrom(timed, idx1)
	day7 = vwapsFrom(timed, idx7)
	day30 = vwapsFrom(timed, idx30)
	return day1, day7, day30
}

// TimedVWAP is one bucketed VWAP with its window-end timestamp. The
// timestamp is the bucket end (when the price was "as of"), so
// SplitByLookback's cutoffs are wall-clock comparable.
//
// Canonical definition lives in [domain.BaselineTimedVWAP] (D8 M0-1:
// internal/storage/timescale reads/writes this shape and must not
// import upward into this package to do so); this is a transparent
// alias so every existing caller of baseline.TimedVWAP is unaffected.
type TimedVWAP = domain.BaselineTimedVWAP

// firstAtOrAfter returns the index of the first entry in `timed`
// with BucketEnd >= cutoff. Returns len(timed) when every entry is
// older than the cutoff.
func firstAtOrAfter(timed []TimedVWAP, cutoff time.Time) int {
	for i := range timed {
		if !timed[i].BucketEnd.Before(cutoff) {
			return i
		}
	}
	return len(timed)
}

// vwapsFrom extracts the VWAP column from a TimedVWAP slice
// starting at `from`. Allocates — sub-windows aren't large enough
// to warrant unsafe slicing tricks.
func vwapsFrom(timed []TimedVWAP, from int) []float64 {
	if from >= len(timed) {
		return nil
	}
	out := make([]float64, len(timed)-from)
	for i := range out {
		out[i] = timed[from+i].VWAP
	}
	return out
}
