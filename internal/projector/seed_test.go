package projector

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// newSeedHarness wires a projector over a store with NO projector cursor — a
// newly-registered source's first cycles — and records every ledger the sink
// receives.
func newSeedHarness(t *testing.T, name string, rows []sorobanevents.Row, tip uint32) (*wedgeHarness, func() []uint32) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []uint32
	)
	h := newWedgeHarness(t, name, rows, tip, func(ev consumer.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if le, ok := ev.(ledgerEvent); ok {
			seen = append(seen, le.ledger)
		}
		return nil
	})
	h.store.haveCursor, h.store.projectorCursor = false, 0
	return h, func() []uint32 {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// A never-run source starts at its first event, not ledger 0: one cycle
// projects the events and commits to the tip, instead of crawling the empty
// pre-history BatchLimit ledgers per Interval (~89 h on mainnet).
func TestCycle_NoCursorSeedsAtFirstEvent(t *testing.T) {
	const first, second, tip = 50_000_000, 50_000_010, 50_000_020
	h, seen := newSeedHarness(t, "seed-first-event", []sorobanevents.Row{lakeRow(first, 1), lakeRow(second, 2)}, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d (scan from the first event through the tip)", got, tip)
	}
	if got, want := seen(), []uint32{first, second}; !slices.Equal(got, want) {
		t.Fatalf("sink saw ledgers %v, want %v", got, want)
	}
}

// With no matching event at or below the tip there is nothing to project:
// the cursor lands on the tip in one cycle.
func TestCycle_NoCursorNoEventsSeedsAtTip(t *testing.T) {
	const tip = 50_000_000
	h, seen := newSeedHarness(t, "seed-no-events", nil, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d", got, tip)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("sink saw ledgers %v, want none", got)
	}
}

// An event beyond the durable tip is not a seed: the seek is bounded by the
// same tip the scan is, so the source waits at the tip rather than jumping
// past ledgers that are not yet durable.
func TestCycle_NoCursorSeekBoundedByTip(t *testing.T) {
	const tip = 50_000_000
	h, seen := newSeedHarness(t, "seed-beyond-tip", []sorobanevents.Row{lakeRow(tip+5, 1)}, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d", got, tip)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("sink saw ledgers %v, want none (event is beyond the tip)", got)
	}
}

// A failed seek degrades to the lossless crawl from ledger 0, and is not
// retried every cycle.
func TestCycle_NoCursorSeekFailureFallsBackToCrawl(t *testing.T) {
	h, _ := newSeedHarness(t, "seed-seek-fails", []sorobanevents.Row{lakeRow(50_000_000, 1)}, 50_000_010)
	h.store.seekErr = errors.New("seek unavailable")

	h.cycle()

	if got := h.store.cursor(); got != BatchLimit {
		t.Fatalf("cursor after failed seek = %d, want %d (crawl from 0)", got, BatchLimit)
	}
	h.store.haveCursor = false
	h.cycle()
	if h.store.seeks != 1 {
		t.Fatalf("seeks = %d, want 1 (fallback is remembered)", h.store.seeks)
	}
}

// The seek runs once per source; later cursor-less cycles (e.g. one whose
// first event's write is being retried) reuse the seed.
func TestSeedFromLedger_SeeksOnce(t *testing.T) {
	const first = 50_000_000
	h, _ := newSeedHarness(t, "seed-once", []sorobanevents.Row{lakeRow(first, 1)}, first+10)

	for i := 0; i < 3; i++ {
		if got := h.proj.seedFromLedger(context.Background(), h.src, h.lake); got != first {
			t.Fatalf("seed #%d = %d, want %d", i, got, first)
		}
	}
	if h.store.seeks != 1 {
		t.Fatalf("seeks = %d, want 1", h.store.seeks)
	}
}
