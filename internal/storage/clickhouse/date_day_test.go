package clickhouse

import (
	"testing"
	"time"
	_ "time/tzdata" // the zones below, whatever the host has installed
)

// A Date decoded at local midnight in any zone names its calendar day: east and west of UTC, on a day the clocks
// fall back at midnight, and on one they jump forward at midnight, where that midnight does not exist.
func TestDateDay_LocalMidnightIsItsCalendarDay(t *testing.T) {
	for _, c := range []struct {
		zone    string
		y, m, d int
	}{
		{"UTC", 2024, 6, 4},
		{"Europe/Berlin", 2024, 6, 4},
		{"Europe/Berlin", 2026, 10, 25},    // falls back at 03:00
		{"America/Santiago", 2026, 4, 5},   // falls back at midnight: it happens twice
		{"America/Santiago", 2026, 9, 6},   // jumps forward at midnight: it never happens
		{"Pacific/Kiritimati", 2024, 6, 4}, // UTC+14
		{"Pacific/Pago_Pago", 2024, 6, 4},  // UTC-11
	} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Fatal(err)
		}
		decoded := time.Date(c.y, time.Month(c.m), c.d, 0, 0, 0, 0, loc) // as clickhouse-go builds a Date
		want := time.Date(c.y, time.Month(c.m), c.d, 0, 0, 0, 0, time.UTC)
		if got := dateDay(decoded); got != want {
			t.Errorf("%s %s: %s; want %s", c.zone, want.Format(time.DateOnly), got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

// The gap case above is real: Go moves the missing midnight into the previous day, so reading the day straight off
// the decoded value would name the wrong one.
func TestDateDay_SantiagoGapMidnightIsThePreviousDay(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Date(2026, 9, 6, 0, 0, 0, 0, loc); d.Day() == 6 {
		t.Fatalf("2026-09-06 00:00 Santiago decoded as %s; the gap case tests nothing", d)
	}
}
