package clickhouse

import (
	"testing"
	"time"
)

// A Date decoded as local midnight in any zone, including a midnight the clocks skip, names its calendar day.
func TestDateDay_DecodedMidnightIsItsCalendarDay(t *testing.T) {
	cases := []struct {
		zone string
		day  time.Time
	}{
		{"UTC", time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)},  // spring forward
		{"Europe/Berlin", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}, // fall back
		{"America/Los_Angeles", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"America/Santiago", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)}, // midnight does not exist
		{"Pacific/Kiritimati", time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)},
		{"Pacific/Pago_Pago", time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)}, // Date32 range
	}
	for _, c := range cases {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatalf("load %s: %v", c.zone, err)
		}
		decoded := time.Date(c.day.Year(), c.day.Month(), c.day.Day(), 0, 0, 0, 0, loc)
		if got := dateDay(decoded); got != c.day {
			t.Errorf("%s midnight of %s (decoded %s): dateDay = %s", c.zone, c.day.Format(time.DateOnly),
				decoded.Format(time.RFC3339), got.Format(time.RFC3339))
		}
	}
}
