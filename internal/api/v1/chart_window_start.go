// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "time"

// fxNoSessionMaxRun is the longest run of UTC days fx_quotes can hold no row for: a weekend joined to holidays on both sides
// (Good Friday to Easter Monday is four days), with margin.
const fxNoSessionMaxRun = 7 * 24 * time.Hour

// fxWindowStartMaxGap is the longest a kept window-start session may end before `from` on a live feed: the longest no-session
// run (Good Friday to Easter Monday, four days) plus the current day, whose row is not closed yet.
const fxWindowStartMaxGap = 5 * 24 * time.Hour

// windowStartOutlived reports a series whose only bucket in force at `from` ended more than [fxWindowStartMaxGap] before it: a
// dead feed's last rate, served as the window's start but not current. Call after [ChartSeries.markWindowStart].
func (c *ChartSeries) windowStartOutlived(from time.Time) bool {
	if !c.StartsBeforeWindow || len(c.Points) == 0 {
		return false
	}
	end := chartBucketStep(c.Points[0].T.Time(), c.Granularity)
	if end.IsZero() {
		end = c.Points[0].T.Time()
	}
	return from.Sub(end) > fxWindowStartMaxGap
}

// dropPointsBeforeWindowStart keeps, of the points at or before from, only the newest: the bucket in force at the window's
// start. A read widened back for a late or absent bucket must not serve the older ones as part of the window.
func (c *ChartSeries) dropPointsBeforeWindowStart(from time.Time) {
	keep := -1
	for i, p := range c.Points {
		if p.T.Time().After(from) {
			break
		}
		keep = i
	}
	if keep > 0 {
		c.Points = c.Points[keep:]
	}
}
