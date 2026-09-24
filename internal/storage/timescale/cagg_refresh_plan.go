// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"time"
)

// CAGGRefreshStep is one refresh_continuous_aggregate call of a plan.
type CAGGRefreshStep struct {
	View     string
	From, To time.Time
	Force    bool
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
		st := CAGGRefreshStep{View: c.Name, From: f, To: t, Force: onMinute[c.Name]}
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

// RunCAGGRefreshStep runs one planned refresh. A view built on prices_1m
// is refused while that view's retention policy is armed: it could drop
// the minute rows this run just rebuilt before the view reads them, and
// migration 0156 requires the policy disarmed for this refresh.
func RunCAGGRefreshStep(ctx context.Context, s CAGGStepRefresher, st CAGGRefreshStep, prices1mRetentionArmed bool) error {
	if !st.Force {
		return s.RefreshContinuousAggregate(ctx, st.View, st.From, st.To)
	}
	if st.View != prices1mView && prices1mRetentionArmed {
		return fmt.Errorf("refused: prices_1m's retention policy is armed, and %s is materialised from prices_1m; "+
			"disarm it as migrations/0156_prices_1m_retention.up.sql states, confirm, and re-run", st.View)
	}
	return s.RefreshContinuousAggregateForced(ctx, st.View, st.From, st.To)
}
