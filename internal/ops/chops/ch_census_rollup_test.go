package chops

import (
	"testing"
	"time"
)

// The issue's sequence: day D holds a LiveSink hole, so the contiguous lake
// tip sits on D. The walk must stop at D — computing D+1 would make it the
// newest day present, and the next resume (max(day)) would never revisit D
// after ch-live-catchup heals it.
func TestCensusWalkEnd_StopsAtHoledDay(t *testing.T) {
	d := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	today := d.Add(24 * time.Hour)
	var logged int
	logf := func(string, ...any) { logged++ }

	if got := censusWalkEnd(d, today, d, true, logf); !got.Equal(d) {
		t.Fatalf("tip on %s, today %s: walk ends %s, want %s", d, today, got, d)
	}
	if logged == 0 {
		t.Error("clamped walk logged nothing; the stall must be visible in the journal")
	}

	// fromDay's own first ledger is missing: recompute nothing.
	if got := censusWalkEnd(d, today, time.Time{}, false, logf); !got.Before(d) {
		t.Fatalf("no contiguous ledger from %s: walk ends %s, want before it", d, got)
	}

	// A contiguous lake walks through today.
	if got := censusWalkEnd(d, today, today, true, logf); !got.Equal(today) {
		t.Fatalf("contiguous lake: walk ends %s, want today %s", got, today)
	}
}
