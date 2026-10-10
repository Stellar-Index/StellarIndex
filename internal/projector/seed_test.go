package projector

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// newSeedHarness wires a projector over a store with NO projector cursor — a
// newly-registered source's first cycles — and records every ledger the sink
// receives.
func newSeedHarness(t *testing.T, name string, evs []events.Event, tip uint32) (*wedgeHarness, func() []uint32) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []uint32
	)
	h := newWedgeHarness(t, name, evs, tip, func(ev consumer.Event) error {
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

// The seek runs once per source; later cursor-less cycles (e.g. one whose
// first event's write is being retried) reuse the seed.
func TestSeedFromLedger_SeeksOnce(t *testing.T) {
	const first = 50_000_000
	h, _ := newSeedHarness(t, "seed-once", []events.Event{lakeEvent(first, 1)}, first+10)

	for i := 0; i < 3; i++ {
		if got := h.proj.seedFromLedger(context.Background(), h.src, h.lake); got != first {
			t.Fatalf("seed #%d = %d, want %d", i, got, first)
		}
	}
	if h.events.seeks != 1 {
		t.Fatalf("seeks = %d, want 1", h.events.seeks)
	}
}
