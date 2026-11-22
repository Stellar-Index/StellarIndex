// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"testing"
	"time"
)

// A CAGG materializes a closed bucket only on a refresh after its end_offset, so for a while after a bucket closes the store
// holds only the one before it. A window shorter than one bucket must serve that one, flagged, not an empty 200; and once the
// last closed bucket lands it alone is served.
func TestChart_ShortWindowServesTheBucketBeforeAnUnmaterializedOne(t *testing.T) {
	now := holedClock().UTC()
	lastHour := now.Truncate(time.Hour).Add(-time.Hour)
	lastDay := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for _, tc := range []struct {
		name, query string
		rows        []time.Time
		want        time.Time
		flagged     bool // the served bucket ends before the window opens
	}{
		{"1h unmaterialized", "timeframe=1h&granularity=1h", []time.Time{lastHour.Add(-time.Hour)}, lastHour.Add(-time.Hour), true},
		{"1d unmaterialized", "timeframe=24h&granularity=1d", []time.Time{lastDay.AddDate(0, 0, -1)}, lastDay.AddDate(0, 0, -1), true},
		{"1h materialized", "timeframe=1h&granularity=1h", []time.Time{lastHour.Add(-time.Hour), lastHour}, lastHour, false},
	} {
		rows := map[time.Time]string{}
		for _, r := range tc.rows {
			rows[r] = holedCEXPrice
		}
		ts := holedServer(t, newChartOHLCStore(map[string]map[time.Time]string{"crypto:XLM/fiat:USD": rows}))
		env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&"+tc.query)
		if len(env.Data.Points) != 1 || !env.Data.Points[0].T.Time().Equal(tc.want) {
			t.Errorf("%s: points = %+v, want only the bucket at %s", tc.name, env.Data.Points, tc.want)
			continue
		}
		if env.Data.StartsBeforeWindow != tc.flagged {
			t.Errorf("%s: starts_before_window = %v, want %v", tc.name, env.Data.StartsBeforeWindow, tc.flagged)
		}
	}
}
