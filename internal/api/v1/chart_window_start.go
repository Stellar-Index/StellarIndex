// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "time"

// fxNoSessionMaxRun is the longest run of UTC days fx_quotes can hold no row for: a weekend joined to holidays on both sides
// (Good Friday to Easter Monday is four days), with margin.
const fxNoSessionMaxRun = 7 * 24 * time.Hour

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
