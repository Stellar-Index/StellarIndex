// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "strings"

// OHLCRoute is how one `/v1/ohlc?interval=` value is served: either
// straight from the continuous aggregate of that width (Native), or
// by folding a finer aggregate into it at query time (Source + Fold,
// see [Store.OHLCSeriesReBucketed]). Exactly one of the two shapes is
// set; [OHLCRoute.Folded] tells them apart.
type OHLCRoute struct {
	// Interval is the wire value the API accepts (`2h`).
	Interval string
	// Native is the granularity whose prices_<Native> view is read
	// as-is. Empty on a folded route.
	Native HistoryGranularity
	// Source is the finer granularity whose prices_<Source> rows are
	// folded, and Fold the Postgres INTERVAL literal they are folded
	// by (`2 hours`). Fold MUST be an integer multiple of Source's
	// bucket — TestOHLCRoutesFoldIsAMultipleOfItsSource pins it. Both
	// empty on a native route.
	Source HistoryGranularity
	Fold   string
}

// Folded reports whether the route re-buckets a finer aggregate
// rather than reading its own.
func (r OHLCRoute) Folded() bool { return r.Native == "" }

// OHLCRoutes is the ONE declaration of the /v1/ohlc interval ladder,
// finest first. Three things derive from it and so cannot disagree:
// the API's `interval` validation and its 400 body (through
// [OHLCRouteFor] / [OHLCIntervalList]), the serving reader's choice
// of view (cmd/stellarindex-api's storeHistoryReader.OHLCSeries),
// and the fold allow-list inside [Store.OHLCSeriesReBucketed] — the
// literal that composes into that query is accepted precisely when a
// folded row here declares it.
//
// It used to be two hand-kept lists in two packages: the reader's
// switch learnt 2h/12h/3d/2w in #213, the allow-list did not, and
// every request at those four widths answered 500 for the whole time
// between (launch plan W8 items 17 and 20).
//
// The fold pairings are query-time derivations over views that
// already exist — nothing here needs a backfill. 3d folds the DAILY
// view rather than the hourly one: 3 source rows instead of 72, and
// prices_1d is already the closed-bucket authority for day
// boundaries. 4h folds prices_1h although prices_4h exists — that is
// how it was routed before this table, and moving it onto the native
// view changes which rows the extremes' notional floor (migration
// 0147) admits, so it is a served-value decision to take on its own.
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
	// Calendar-month view (prices_1mo, migration 0002) — the RFP's
	// suggested-granularity ladder tops out at 1 month (board #43).
	{Interval: "1mo", Native: Granularity1mo},
}

// OHLCRouteFor resolves a wire interval to its route. ok=false for
// anything [OHLCRoutes] does not declare — the API's 400, and the
// serving reader's ErrUnknownGranularity-shaped refusal.
func OHLCRouteFor(interval string) (OHLCRoute, bool) {
	for _, r := range OHLCRoutes {
		if r.Interval == interval {
			return r, true
		}
	}
	return OHLCRoute{}, false
}

// OHLCIntervalList renders the accepted intervals as the enumeration
// the API's 400 body carries — generated, like [HistoryGranularityList],
// so a new rung reaches the message without anyone editing prose.
func OHLCIntervalList() string {
	parts := make([]string, len(OHLCRoutes))
	for i, r := range OHLCRoutes {
		parts[i] = r.Interval
	}
	return strings.Join(parts, historyGranularitySep)
}

// ohlcFoldDeclared reports whether some folded route re-buckets
// `source` by exactly `fold`. It is the allow-list
// [Store.OHLCSeriesReBucketed] applies before `fold` composes into
// SQL: a pairing is safe to interpolate precisely because it is a
// literal in this file, never a request value.
func ohlcFoldDeclared(source HistoryGranularity, fold string) bool {
	for _, r := range OHLCRoutes {
		if r.Folded() && r.Source == source && r.Fold == fold {
			return true
		}
	}
	return false
}
