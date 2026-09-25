package chops

import (
	"testing"
	"time"
)

// TestClosedMonthEdge_ExcludesTheInProgressMonth is the GH-1058
// regression: the cycle must read MonthlyUSDVWAPs up to the START of
// now's calendar month, never up to `now` itself — otherwise the
// current, still-accumulating prices_1mo bucket is admitted as a
// settled monthly VWAP.
func TestClosedMonthEdge_ExcludesTheInProgressMonth(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "mid-month",
			now:  time.Date(2026, time.September, 24, 18, 45, 0, 0, time.UTC),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "exactly on the month boundary",
			now:  time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "year boundary",
			now:  time.Date(2026, time.January, 3, 5, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "non-UTC input is normalized",
			now:  time.Date(2026, time.September, 24, 23, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60)),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := closedMonthEdge(tc.now)
			if !got.Equal(tc.want) {
				t.Errorf("closedMonthEdge(%s) = %s, want %s",
					tc.now.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			// The in-progress month's own bucket timestamp (its start)
			// must be excluded by `bucket < edge` — i.e. the edge is
			// never AFTER the current month's start.
			monthStart := time.Date(tc.now.UTC().Year(), tc.now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
			if got.After(monthStart) {
				t.Errorf("closedMonthEdge(%s) = %s, is after the current month's own bucket start %s — would admit the open month",
					tc.now.Format(time.RFC3339), got.Format(time.RFC3339), monthStart.Format(time.RFC3339))
			}
		})
	}
}
