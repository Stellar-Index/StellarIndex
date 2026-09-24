package clickhouse

import (
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestWalkSACSeedWindowsRewidensAfterDenseRange drives the full-history SAC
// seed's window walk through one dense stretch that only fits at the bisection
// floor. The walk must cover the range exactly once and recover its full width
// afterwards; a monotonically-narrowing walk crawls the remaining ~10M ledgers
// at the floor (640 round-trips instead of ~50).
func TestWalkSACSeedWindowsRewidensAfterDenseRange(t *testing.T) {
	const (
		minLedger = 40_000_000
		maxLedger = 49_999_999
		denseEnd  = minLedger + 2*sacSeedMinLedgerWindow - 1
	)
	oom := &clickhouse.Exception{Code: chMemoryLimitExceeded, Name: "MEMORY_LIMIT_EXCEEDED"}

	type span struct{ from, to uint32 }
	var ok []span
	err := walkSACSeedWindows(minLedger, maxLedger, func(from, to uint32) error {
		if from <= denseEnd && to-from+1 > sacSeedMinLedgerWindow {
			return oom
		}
		ok = append(ok, span{from, to})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	next := uint32(minLedger)
	for i, s := range ok {
		if s.from != next || s.to < s.from {
			t.Fatalf("window %d = [%d,%d], want it to start at %d (gap or overlap)", i, s.from, s.to, next)
		}
		next = s.to + 1
	}
	if next != maxLedger+1 {
		t.Fatalf("walk stopped at %d, want %d", next-1, maxLedger)
	}

	var widest uint32
	for _, s := range ok {
		if s.from > denseEnd {
			widest = max(widest, s.to-s.from+1)
		}
	}
	if widest != sacSeedLedgerWindow {
		t.Errorf("widest window after the dense stretch = %d, want it re-widened to %d", widest, sacSeedLedgerWindow)
	}
	if len(ok) > 80 {
		t.Errorf("walk took %d clean windows over %d ledgers — the window never recovered from the dense stretch",
			len(ok), maxLedger-minLedger+1)
	}
}

// TestWalkSACSeedWindowsSurfacesFloorAndForeignErrors pins the two exits the
// adaptive policy must not swallow: a memory-limit error at the floor, and any
// non-memory error at any width.
func TestWalkSACSeedWindowsSurfacesFloorAndForeignErrors(t *testing.T) {
	oom := &clickhouse.Exception{Code: chMemoryLimitExceeded}
	var widths []uint32
	err := walkSACSeedWindows(1, 10_000_000, func(from, to uint32) error {
		widths = append(widths, to-from+1)
		return oom
	})
	if !errors.Is(err, oom) {
		t.Fatalf("err = %v, want the floor's memory-limit error surfaced", err)
	}
	if got := widths[len(widths)-1]; got != sacSeedMinLedgerWindow {
		t.Fatalf("last attempt width = %d, want the floor %d", got, sacSeedMinLedgerWindow)
	}

	boom := errors.New("boom")
	calls := 0
	err = walkSACSeedWindows(1, 10_000_000, func(uint32, uint32) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want boom surfaced on the first call", err, calls)
	}
}
