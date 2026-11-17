package chops

import (
	"testing"
	"time"
)

// A walk stopped by a lake hole is fine for a day (ingest crossing midnight, a catch-up in flight) and a failure after that:
// exiting 0 behind a persistent hole is what left the census silently stale.
func TestCensusWalkStalled(t *testing.T) {
	today := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return today.AddDate(0, 0, n) }
	for _, tc := range []struct {
		name     string
		last     time.Time
		wantFail bool
	}{
		{"walked through today", day(0), false},
		{"tip on yesterday", day(-1), false},
		{"tip two days back", day(-2), true},
		{"nothing recomputed from a stale resume day", censusWalkEnd(day(-5), today, time.Time{}, false, func(string, ...any) {}), true},
		{"nothing recomputed from today", censusWalkEnd(day(0), today, time.Time{}, false, func(string, ...any) {}), false},
	} {
		if err := censusWalkStalled(tc.last, today); (err != nil) != tc.wantFail {
			t.Errorf("%s: censusWalkStalled(%s) = %v, want failure=%v", tc.name, tc.last.Format("2006-01-02"), err, tc.wantFail)
		}
	}
}
