package projector

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Gives the in-memory store the same compare-and-swap contract as [timescale.Store.AdvanceCursorFrom], including shapes
// a happy-path fake never produces: a row moved since the read, one that appeared since a not-found read, a non-
// advancing write. The real statement is proven in test/integration/projector_test.go.
func (f *fakeStore) AdvanceCursorFrom(_ context.Context, _, _ string, expected timescale.CursorRead, newLast uint32) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	if !expected.Exists {
		if f.haveCursor {
			return false, nil // a row appeared since the read — DO NOTHING
		}
		f.haveCursor, f.projectorCursor = true, newLast
		return true, nil
	}
	if newLast <= expected.LastLedger {
		return false, fmt.Errorf("fake: %d is not an advance over %d", newLast, expected.LastLedger)
	}
	if !f.haveCursor || f.projectorCursor != expected.LastLedger {
		return false, nil // moved (or vanished) since the read
	}
	f.projectorCursor = newLast
	return true, nil
}

// rewind mirrors [timescale.Store.RewindCursor]: strictly backward, on an
// existing row only.
func (f *fakeStore) rewind(t *testing.T, to uint32) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.haveCursor || f.projectorCursor <= to {
		t.Fatalf("fake rewind to %d refused: have=%v cursor=%d", to, f.haveCursor, f.projectorCursor)
	}
	f.projectorCursor = to
}
