package clickhouse

import (
	"testing"
	"time"
)

// A Date the driver decoded at a server's local midnight names its calendar day, whatever the zone: east of UTC that
// midnight is the previous UTC day, west of it the same day's evening.
func TestDateDay_ServerZoneMidnightIsItsCalendarDay(t *testing.T) {
	want := time.Date(2024, 6, 4, 0, 0, 0, 0, time.UTC)
	for _, off := range []int{-11, -5, 0, 2, 9, 14} {
		loc := time.FixedZone("server", off*3600)
		if got := dateDay(time.Date(2024, 6, 4, 0, 0, 0, 0, loc)); got != want {
			t.Errorf("UTC%+d midnight: %s; want %s", off, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}
