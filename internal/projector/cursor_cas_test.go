package projector

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AdvanceCursorFrom gives the in-memory store the same compare-and-swap
// contract as [timescale.Store.AdvanceCursorFrom], INCLUDING the shapes a
// happy-path fake would never produce: a row that moved since the read, a
// row that appeared since a not-found read, and a non-advancing write. The
// real statement is proven on TimescaleDB in
// test/integration/projector_test.go; this only has to be
// faithful enough that the projector's use of it is what is under test.
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
