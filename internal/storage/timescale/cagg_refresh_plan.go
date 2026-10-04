// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CAGGRefreshStep is one view's refresh in a plan; [RunCAGGRefreshStep]
// issues it as the CALLs [RefreshPieces] cuts it into.
type CAGGRefreshStep struct {
	View      string
	From, To  time.Time
	Force     bool
	Bucket    time.Duration
	MinWindow time.Duration
}

// prices1mView is the minute rung the [CAGGsOnPrices1m] views read.
const prices1mView = "prices_1m"

// PlanCAGGRefresh orders a refresh of views, each over window(view).
// prices_1m is the input of [CAGGsOnPrices1m]: it is forced over the hull
// of their windows, and they are forced after it, so none of them
// recomputes a bucket from minute rows a retention drop removed
// (migration 0156). views must list prices_1m before them.
func PlanCAGGRefresh(views []CAGGSpec, window func(CAGGSpec) (time.Time, time.Time)) []CAGGRefreshStep {
	onMinute := make(map[string]bool, len(CAGGsOnPrices1m))
	for _, v := range CAGGsOnPrices1m {
		onMinute[v] = true
	}
	plan := make([]CAGGRefreshStep, 0, len(views))
	minute := -1
	for _, c := range views {
		f, t := window(c)
		st := CAGGRefreshStep{View: c.Name, From: f, To: t, Force: onMinute[c.Name], Bucket: c.Bucket, MinWindow: c.MinWindow}
		switch {
		case c.Name == prices1mView:
			st.Force, minute = true, len(plan)
		case st.Force && minute < 0:
			panic("PlanCAGGRefresh: " + c.Name + " is listed before prices_1m, which it is built on")
		}
		plan = append(plan, st)
	}
	for _, st := range plan {
		if !onMinute[st.View] {
			continue
		}
		if st.From.Before(plan[minute].From) {
			plan[minute].From = st.From
		}
		if st.To.After(plan[minute].To) {
			plan[minute].To = st.To
		}
	}
	return plan
}

// CAGGStepRefresher is the slice of *Store [RunCAGGRefreshStep] needs.
type CAGGStepRefresher interface {
	RefreshContinuousAggregate(ctx context.Context, viewName string, from, to time.Time) error
	RefreshContinuousAggregateForced(ctx context.Context, viewName string, from, to time.Time) error
}

// CAGGRefreshPieceSpan is the longest window one refresh CALL covers,
// unless the view's MinWindow is longer. A CALL materialises its whole
// window in one transaction and holds AccessShare on every source chunk
// the window touches until it commits; one CALL over a replayed range
// held the trades chunks for 80+ min, blocking their compression.
const CAGGRefreshPieceSpan = 24 * time.Hour

// bucketOrigin is time_bucket's default origin for fixed-width buckets.
var bucketOrigin = time.Date(2000, 1, 3, 0, 0, 0, 0, time.UTC)

// bucketCeil returns the first boundary of the bucket grid at or after t.
func bucketCeil(t time.Time, bucket time.Duration) time.Time {
	t = t.UTC()
	if bucket == MonthBucket {
		m := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		if m.Before(t) {
			m = m.AddDate(0, 1, 0)
		}
		return m
	}
	switch off := t.Sub(bucketOrigin) % bucket; {
	case off < 0:
		return t.Add(-off)
	case off > 0:
		return t.Add(bucket - off)
	}
	return t
}

// RefreshPieces cuts st into consecutive windows of at least
// max([CAGGRefreshPieceSpan], MinWindow) that together cover exactly
// [st.From, st.To). Every cut sits on the view's bucket grid, so no bucket
// straddles two pieces and the pieces refresh the same buckets one CALL
// over the whole window would. A step with no known Bucket, or too short
// to cut, is returned whole.
func RefreshPieces(st CAGGRefreshStep) []CAGGRefreshStep {
	var out []CAGGRefreshStep
	if (st.Bucket > 0 || st.Bucket == MonthBucket) && st.MinWindow > 0 {
		span := max(CAGGRefreshPieceSpan, st.MinWindow)
		for {
			cut := bucketCeil(st.From.Add(span), st.Bucket)
			if st.To.Sub(cut) < st.MinWindow {
				break
			}
			p := st
			p.To = cut
			out = append(out, p)
			st.From = cut
		}
	}
	return append(out, st)
}

// RunCAGGRefreshStep runs one planned refresh as the CALLs [RefreshPieces]
// cuts it into, so each commits and releases its chunk locks before the
// next starts. A failed piece does not stop the rest; the error names
// every window left unrefreshed. A view built on prices_1m is refused
// while that view's retention policy is armed: it could drop the minute
// rows this run just rebuilt before the view reads them, and migration
// 0156 requires the policy disarmed for this refresh.
func RunCAGGRefreshStep(ctx context.Context, s CAGGStepRefresher, st CAGGRefreshStep, prices1mRetentionArmed bool) error {
	if st.Force && st.View != prices1mView && prices1mRetentionArmed {
		return fmt.Errorf("refused: prices_1m's retention policy is armed, and %s is materialised from prices_1m; "+
			"disarm it as migrations/0156_prices_1m_retention.up.sql states, confirm, and re-run", st.View)
	}
	refresh := s.RefreshContinuousAggregate
	if st.Force {
		refresh = s.RefreshContinuousAggregateForced
	}
	pieces := RefreshPieces(st)
	if len(pieces) == 1 {
		return refresh(ctx, st.View, st.From, st.To)
	}
	var errs []error
	var missed []string
	for i, p := range pieces {
		err := refresh(ctx, p.View, p.From, p.To)
		if err == nil {
			continue
		}
		errs = append(errs, err)
		missed = append(missed, refreshWindowString(p))
		if ctx.Err() != nil {
			for _, q := range pieces[i+1:] {
				missed = append(missed, refreshWindowString(q))
			}
			break
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("refresh %s: %d of %d window(s) left unrefreshed (%s): %w",
		st.View, len(missed), len(pieces), strings.Join(missed, ", "), errors.Join(errs...))
}

func refreshWindowString(p CAGGRefreshStep) string {
	return "[" + p.From.UTC().Format(time.RFC3339) + ", " + p.To.UTC().Format(time.RFC3339) + ")"
}
