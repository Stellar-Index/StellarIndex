// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// twapCoverStore is the slice of *timescale.Store the twap read check needs.
type twapCoverStore interface {
	CAGGInvalidatedRanges(ctx context.Context, view string, from, to time.Time) ([][2]time.Time, error)
	tradesDriftStore
}

// twapCoverPiece caps each trades-vs-prices_1m comparison, so a long
// scope is read a day at a time, newest first, and stops at the first gap.
const twapCoverPiece = 24 * time.Hour

// newestUncoveredTwapRead returns the newest piece of the minutes a
// non-forced run of plan feeds into a twap bucket where prices_1m still
// disagrees with trades once its own invalidated minutes are rebuilt, with
// the drifting pairs; ok is false when every such minute agrees. A twap
// bucket is recomputed when its own log or prices_1m's names it, and reads
// every minute of the bucket, so a minute dropped by retention above
// prices_1m's earliest bucket would be recomputed into it as nothing.
// Minutes at or after cutoff are left out: the live policies own them.
func newestUncoveredTwapRead(ctx context.Context, s twapCoverStore, plan []timescale.CAGGRefreshStep, cutoff time.Time) (
	[2]time.Time, []timescale.TradesPrices1mDrift, bool, error,
) {
	check, err := twapCoverScope(ctx, s, plan)
	if err != nil {
		return [2]time.Time{}, nil, false, err
	}
	check = clipRanges(check, [2]time.Time{{}, cutoff.Truncate(time.Minute)})
	for i := len(check) - 1; i >= 0; i-- {
		for hi := check[i][1]; hi.After(check[i][0]); {
			lo := hi.Add(-time.Nanosecond).Truncate(twapCoverPiece)
			if lo.Before(check[i][0]) {
				lo = check[i][0]
			}
			rows, err := s.TradesPrices1mDrift(ctx, lo, hi)
			if err != nil {
				return [2]time.Time{}, nil, false, fmt.Errorf("compare trades with prices_1m over %s: %w", fmtDriftWindow([2]time.Time{lo, hi}), err)
			}
			if len(rows) > 0 {
				return [2]time.Time{lo, hi}, rows, true, nil
			}
			hi = lo
		}
	}
	return [2]time.Time{}, nil, false, nil
}

// twapCoverScope is every minute a twap step of plan would read into a
// bucket it recomputes, less the minutes the plan's prices_1m step rebuilds
// from trades first. Sorted, disjoint, whole minutes.
func twapCoverScope(ctx context.Context, s twapCoverStore, plan []timescale.CAGGRefreshStep) ([][2]time.Time, error) {
	i := slices.IndexFunc(plan, func(st timescale.CAGGRefreshStep) bool { return st.View == "prices_1m" })
	if i < 0 {
		return nil, nil
	}
	rebuilt, err := s.CAGGInvalidatedRanges(ctx, plan[i].View, plan[i].From, plan[i].To)
	if err != nil {
		return nil, err
	}
	rebuilt = alignRanges(rebuilt, time.Minute)
	var read [][2]time.Time
	for _, st := range plan {
		if !slices.Contains(timescale.CAGGsOnPrices1m, st.View) {
			continue
		}
		// A refresh materialises only the buckets wholly inside its window.
		inner := [2]time.Time{ceilTime(st.From, st.Bucket), st.To.Truncate(st.Bucket)}
		if !inner[0].Before(inner[1]) {
			continue
		}
		pending, err := s.CAGGInvalidatedRanges(ctx, st.View, inner[0], inner[1])
		if err != nil {
			return nil, err
		}
		read = append(read, clipRanges(alignRanges(append(pending, rebuilt...), st.Bucket), inner)...)
	}
	return subtractRanges(mergeRanges(read), rebuilt), nil
}

// refuseUncoveredTwapReads refuses a non-forced plan whose twap steps
// would recompute a bucket from minutes prices_1m does not hold
// ([newestUncoveredTwapRead]), printing the drifting pairs to out.
func refuseUncoveredTwapReads(ctx context.Context, s twapCoverStore, plan []timescale.CAGGRefreshStep, from, to uint32, now time.Time, out io.Writer) error {
	w, rows, ok, err := newestUncoveredTwapRead(ctx, s, plan, now.Add(-prices1mSettle))
	if err != nil || !ok {
		return err
	}
	if err := printTradesDrift(out, w, rows); err != nil {
		return err
	}
	return fmt.Errorf("refused: -force=false over ledgers [%d,%d]: prices_1m disagrees with trades over %s (pairs above), "+
		"minutes a recomputed %s bucket reads and no pending invalidation rebuilds, so it would be recomputed without them; "+
		"re-run with -force=true, which rebuilds prices_1m from trades over the range first",
		from, to, fmtDriftWindow(w), strings.Join(timescale.CAGGsOnPrices1m, "/"))
}

func ceilTime(t time.Time, d time.Duration) time.Time {
	if c := t.Truncate(d); c.Before(t) {
		return c.Add(d)
	}
	return t
}

// alignRanges widens each range outward to whole d.
func alignRanges(rs [][2]time.Time, d time.Duration) [][2]time.Time {
	out := make([][2]time.Time, len(rs))
	for i, r := range rs {
		out[i] = [2]time.Time{r[0].Truncate(d), ceilTime(r[1], d)}
	}
	return mergeRanges(out)
}

// mergeRanges sorts half-open ranges and joins those that overlap or touch.
func mergeRanges(rs [][2]time.Time) [][2]time.Time {
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	var out [][2]time.Time
	for _, r := range rs {
		if !r[0].Before(r[1]) {
			continue
		}
		if n := len(out); n > 0 && !r[0].After(out[n-1][1]) {
			if r[1].After(out[n-1][1]) {
				out[n-1][1] = r[1]
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// clipRanges intersects each range with w; a zero w[0] is unbounded below.
func clipRanges(rs [][2]time.Time, w [2]time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, r := range rs {
		if !w[0].IsZero() && r[0].Before(w[0]) {
			r[0] = w[0]
		}
		if r[1].After(w[1]) {
			r[1] = w[1]
		}
		if r[0].Before(r[1]) {
			out = append(out, r)
		}
	}
	return out
}

// subtractRanges removes the merged ranges cut from the merged ranges rs.
func subtractRanges(rs, cut [][2]time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, r := range rs {
		for _, c := range cut {
			if !c[1].After(r[0]) || !c[0].Before(r[1]) {
				continue
			}
			if c[0].After(r[0]) {
				out = append(out, [2]time.Time{r[0], c[0]})
			}
			r[0] = c[1]
		}
		if r[0].Before(r[1]) {
			out = append(out, r)
		}
	}
	return out
}
