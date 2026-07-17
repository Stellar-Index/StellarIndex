package supply

import (
	"context"
	"testing"
	"time"
)

// fakeCloseTimeReader is a DB-free ledgerCloseTimeReader: it serves a preset
// close time (or a not-found / error) for whatever ledger it's asked about,
// recording the ledger so the plumbing can be asserted.
type fakeCloseTimeReader struct {
	closeTime time.Time
	found     bool
	err       error
	gotLedger uint32
	calls     int
}

func (f *fakeCloseTimeReader) CloseTimeForLedger(_ context.Context, ledger uint32) (time.Time, bool, error) {
	f.calls++
	f.gotLedger = ledger
	return f.closeTime, f.found, f.err
}

// TestResolveSnapshotLedger_StampsLedgerCloseTimeNotWallClock is the
// M4-callers proof for the ops-supply caller. resolveSnapshotLedger must
// stamp ObservedAt with the chosen ledger's REAL close_time (resolved from
// stellar.ledgers via the close-time reader) — NEVER time.Now(). The fixture
// close time is deliberately ~2.5y stale, so a wall-clock stamp (the pre-fix
// bug: both branches returned time.Now().UTC()) is unmistakable. A re-derived
// historical supply snapshot stamped with the write-time corrupts every
// point-in-time supply/observation query.
//
// The operator -ledger branch is exercised (opLedger>0), which never touches
// the store, so a nil store keeps the test DB-free.
func TestResolveSnapshotLedger_StampsLedgerCloseTimeNotWallClock(t *testing.T) {
	const opLedger = 40_000_000
	closeTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	reader := &fakeCloseTimeReader{closeTime: closeTime, found: true}

	ledger, observedAt, err := resolveSnapshotLedger(context.Background(), nil, reader, opLedger)
	if err != nil {
		t.Fatalf("resolveSnapshotLedger: %v", err)
	}
	if ledger != opLedger {
		t.Errorf("ledger = %d, want %d", ledger, opLedger)
	}
	if reader.gotLedger != opLedger {
		t.Errorf("close-time lookup asked for ledger %d, want %d", reader.gotLedger, opLedger)
	}
	if !observedAt.Equal(closeTime) {
		t.Errorf("ObservedAt = %v, want the ledger close time %v (wall-clock stamp regression)", observedAt, closeTime)
	}
	if time.Since(observedAt) < 365*24*time.Hour {
		t.Errorf("ObservedAt %v is suspiciously close to now — resolver stamped wall-clock instead of the ledger close time", observedAt)
	}
}

// TestResolveSnapshotLedger_FailsClosedOnMissingLedgerRow proves the
// fail-closed fallback choice: when the chosen ledger has no stellar.ledgers
// row, the resolver returns an error rather than silently falling back to
// time.Now() (which would reintroduce the M4-callers corruption).
func TestResolveSnapshotLedger_FailsClosedOnMissingLedgerRow(t *testing.T) {
	reader := &fakeCloseTimeReader{found: false}
	if _, _, err := resolveSnapshotLedger(context.Background(), nil, reader, 40_000_000); err == nil {
		t.Fatal("expected an error when the ledger has no stellar.ledgers row, got nil — a silent wall-clock fallback would reintroduce the point-in-time corruption")
	}
}
