package orchestrator

import (
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// marginalBuckets is what one (pair, window) decision scores: each non-empty closed minute since the previous decision's newest, measured against the previous NON-EMPTY minute.
// Both freeze phases score minute-on-minute returns, never the rolling window's tick-to-tick delta, which overlap damps (ADR-0019). Phase 2 scales by minutes between; Phase 1 uses the raw move.
// Scoring every minute since the last decision keeps a batched move measured against the pre-batch level.
type marginalBuckets struct {
	pred    *minuteVWAP  // the minute before minutes[0]; nil when none is known
	minutes []minuteVWAP // oldest first; empty only when no minute can be priced
	rolling *big.Rat     // the window's VWAP: the observation when minutes is empty
}

type minuteVWAP struct {
	at   time.Time
	vwap *big.Rat
}

// priceStep is one scored move: curr against its comparator prev, elapsed
// apart (0 when prev's minute is unknown, i.e. the published fallback).
type priceStep struct {
	prev, curr *big.Rat
	elapsed    time.Duration
}

// scoreMinutes prices the minutes this decision scores from the window's own
// (already filtered) trades through the served VWAP path, and records the
// newest as the next decision's starting minute. Unpriceable minutes are
// skipped, so the newest PRICEABLE minute is always scored.
func (o *Orchestrator) scoreMinutes(trades []canonical.Trade, pair canonical.Pair, stateKey string, rolling *big.Rat) marginalBuckets {
	byMinute := make(map[int64][]canonical.Trade)
	for _, tr := range trades {
		m := tr.Timestamp.Truncate(closedBucket).UnixNano()
		byMinute[m] = append(byMinute[m], tr)
	}
	keys := make([]int64, 0, len(byMinute))
	for m := range byMinute {
		keys = append(keys, m)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })

	mb := marginalBuckets{rolling: rolling}
	last, haveLast := o.scoredMinutes[stateKey]
	var newestFirst []minuteVWAP
	for _, k := range keys {
		v := o.bucketVWAP(byMinute[k], pair)
		if v == nil {
			continue
		}
		at := time.Unix(0, k).UTC()
		if len(newestFirst) > 0 && (!haveLast || at.Before(last.at)) {
			mb.pred = &minuteVWAP{at: at, vwap: v}
			break
		}
		newestFirst = append(newestFirst, minuteVWAP{at: at, vwap: v})
	}
	if len(newestFirst) == 0 {
		return mb
	}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		mb.minutes = append(mb.minutes, newestFirst[i])
	}
	if mb.pred == nil && haveLast && last.at.Before(mb.minutes[0].at) {
		// The previous decision's minute has left the window (or lost its
		// trades to the filters); it is still the previous non-empty minute.
		mb.pred = &last
	}
	if o.scoredMinutes == nil {
		o.scoredMinutes = make(map[string]minuteVWAP)
	}
	o.scoredMinutes[stateKey] = newestFirst[0]
	return mb
}

func (o *Orchestrator) bucketVWAP(trades []canonical.Trade, pair canonical.Pair) *big.Rat {
	v, err := o.computeNormalizedVWAP(trades, pair)
	if err != nil {
		return nil
	}
	return v
}

// steps returns every scored move. The first minute is measured against
// fallback — the last published VWAP — only when no earlier minute is known;
// when no minute could be priced the rolling move against fallback stands in.
// A fallback comparison has no known minute, so its elapsed is 0.
func (mb marginalBuckets) steps(fallback *big.Rat) []priceStep {
	if len(mb.minutes) == 0 {
		return []priceStep{{prev: fallback, curr: mb.rolling}}
	}
	prev := minuteVWAP{vwap: fallback}
	if mb.pred != nil {
		prev = *mb.pred
	}
	out := make([]priceStep, 0, len(mb.minutes))
	for _, m := range mb.minutes {
		s := priceStep{prev: prev.vwap, curr: m.vwap}
		if !prev.at.IsZero() {
			s.elapsed = m.at.Sub(prev.at)
		}
		out = append(out, s)
		prev = m
	}
	return out
}

// worstStep is Phase 1's observation: the largest absolute move among
// steps(fallback), by the evaluator's own |curr-prev|/|prev| measure. With no
// comparable step it is the newest, whose nil prev reads as "no prior bucket".
func (mb marginalBuckets) worstStep(fallback *big.Rat) priceStep {
	steps := mb.steps(fallback)
	worst := steps[len(steps)-1]
	var worstMove *big.Rat
	for _, s := range steps {
		if s.prev == nil || s.curr == nil {
			continue
		}
		if s.prev.Sign() == 0 {
			if s.curr.Sign() != 0 {
				return s // the evaluator's unbounded deviation
			}
			continue
		}
		move := new(big.Rat).Sub(s.curr, s.prev)
		move.Quo(move, s.prev)
		move.Abs(move)
		if worstMove == nil || move.Cmp(worstMove) > 0 {
			worst, worstMove = s, move
		}
	}
	return worst
}

// returns is Phase 2's observation set: every comparable step as a bucket
// return, scaled by its elapsed minutes exactly as the baseline's training
// returns are. Empty when nothing is comparable.
func (mb marginalBuckets) returns(fallback *big.Rat) []baseline.BucketReturn {
	var out []baseline.BucketReturn
	for _, s := range mb.steps(fallback) {
		if s.prev == nil || s.curr == nil {
			continue
		}
		p, _ := s.prev.Float64() // i128:ok price ratio for the z-score observation, not an amount
		c, _ := s.curr.Float64() // i128:ok price ratio for the z-score observation, not an amount
		if r, ok := baseline.NewBucketReturn(p, c, s.elapsed); ok {
			out = append(out, r)
		}
	}
	return out
}
