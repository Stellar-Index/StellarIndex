// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "strings"

// OHLCRoute is how one `/v1/ohlc?interval=` value is served: from the continuous aggregate of that width (Native) or by folding a finer one (Source + Fold); exactly one shape is set, see [OHLCRoute.Folded].
type OHLCRoute struct {
	// Interval is the wire value the API accepts (`2h`).
	Interval string
	// Native is the granularity whose prices_<Native> view is read as-is; empty on a folded route.
	Native HistoryGranularity
	// Source is the finer granularity whose rows are folded by the Postgres INTERVAL literal Fold
	// (`2 hours`). Fold MUST be an integer multiple of Source's bucket
	// (TestOHLCRoutesFoldIsAMultipleOfItsSource). Both are empty on a native route.
	Source HistoryGranularity
	Fold   string
}

// Folded reports whether the route re-buckets a finer aggregate.
func (r OHLCRoute) Folded() bool { return r.Native == "" }

// OHLCRoutes is the single declaration of the /v1/ohlc interval ladder, finest first; validation, the 400 body, view choice and the fold allow-list all derive from it so they cannot drift.
var OHLCRoutes = []OHLCRoute{
	{Interval: "1m", Native: Granularity1m},
	{Interval: "5m", Source: Granularity1m, Fold: "5 minutes"},
	{Interval: "15m", Native: Granularity15m},
	{Interval: "30m", Source: Granularity1m, Fold: "30 minutes"},
	{Interval: "1h", Native: Granularity1h},
	{Interval: "2h", Source: Granularity1h, Fold: "2 hours"},
	{Interval: "4h", Source: Granularity1h, Fold: "4 hours"},
	{Interval: "12h", Source: Granularity1h, Fold: "12 hours"},
	{Interval: "1d", Native: Granularity1d},
	{Interval: "3d", Source: Granularity1d, Fold: "3 days"},
	{Interval: "1w", Native: Granularity1w},
	{Interval: "2w", Source: Granularity1w, Fold: "2 weeks"},
	// Calendar-month view (prices_1mo); the suggested ladder tops out at 1 month.
	{Interval: "1mo", Native: Granularity1mo},
}

// OHLCRouteFor resolves a wire interval to its route; ok=false for anything [OHLCRoutes] does not declare (the API's 400).
func OHLCRouteFor(interval string) (OHLCRoute, bool) {
	for _, r := range OHLCRoutes {
		if r.Interval == interval {
			return r, true
		}
	}
	return OHLCRoute{}, false
}

// OHLCIntervalList renders the accepted intervals for the API's 400 body, generated so a new rung reaches the message without prose edits.
func OHLCIntervalList() string {
	parts := make([]string, len(OHLCRoutes))
	for i, r := range OHLCRoutes {
		parts[i] = r.Interval
	}
	return strings.Join(parts, historyGranularitySep)
}

// ohlcFoldDeclared reports whether some folded route re-buckets `source` by exactly `fold`. It is
// the allow-list [Store.OHLCSeriesReBucketed] applies before `fold` is interpolated into SQL: the
// pairing is safe because it is a literal in this file, never a request value.
func ohlcFoldDeclared(source HistoryGranularity, fold string) bool {
	for _, r := range OHLCRoutes {
		if r.Folded() && r.Source == source && r.Fold == fold {
			return true
		}
	}
	return false
}
