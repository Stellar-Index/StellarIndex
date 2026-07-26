package supply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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

// fakeCursorReader is a DB-free cursorReader for the auto-resolve branch.
type fakeCursorReader struct {
	cursors []timescale.Cursor
	err     error
}

func (f *fakeCursorReader) ListCursors(context.Context) ([]timescale.Cursor, error) {
	return f.cursors, f.err
}

// TestAutoSnapshotLedger_PrefersTheChainCursor is the C4-033 regression.
//
// ingestion_cursors is a table of JOB positions, not chain positions.
// Taking MAX(last_ledger) over all of them let any ops job decide what
// ledger a supply snapshot claims to be as-of. The reachable case, encoded
// here: the live indexer is behind (restart / re-derive / maintenance) at
// ledger 63,000,000 while an operator backfills a range near the tip, whose
// cursor reaches 63,900,000. Every component balance summed into the
// snapshot was observed at the INDEXER's position — so 63,900,000 is a
// false attribution for the most-consumed number the product serves.
func TestAutoSnapshotLedger_PrefersTheChainCursor(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "backfill", Sub: "63500000-63900000", LastLedger: 63_900_000},
		{Source: "ledgerstream", LastLedger: 63_000_000},
		{Source: "projector", Sub: "soroswap", LastLedger: 62_800_000},
		{Source: "gap-detector-high-water", Sub: "trades:sdex", LastLedger: 63_100_000},
	}
	ledger, source := autoSnapshotLedger(cursors)
	if ledger != 63_000_000 {
		t.Errorf("ledger = %d, want 63000000 (the ledgerstream chain cursor); MAX over all cursors would pick the backfill job's 63900000 and stamp the snapshot at a ledger no component balance was observed at", ledger)
	}
	if source != "ledgerstream" {
		t.Errorf("source = %q, want \"ledgerstream\"", source)
	}
}

// TestAutoSnapshotLedger_FallsBackWhenNoChainCursor keeps the pre-first-run
// case working (a host seeding supply before the indexer has written its
// cursor) — and requires the fallback to NAME itself, so the run log states
// that a job cursor supplied the stamp rather than the chain.
func TestAutoSnapshotLedger_FallsBackWhenNoChainCursor(t *testing.T) {
	cursors := []timescale.Cursor{
		{Source: "backfill", Sub: "1-100", LastLedger: 100},
		{Source: "census-backfill", Sub: "sdex", LastLedger: 250},
	}
	ledger, source := autoSnapshotLedger(cursors)
	if ledger != 250 {
		t.Errorf("ledger = %d, want the MAX fallback 250", ledger)
	}
	if !strings.Contains(source, "census-backfill/sdex") || !strings.Contains(source, "FALLBACK") {
		t.Errorf("source = %q, want it to name the job cursor AND mark itself a fallback", source)
	}
}

// TestAutoSnapshotLedger_NoCursors — an empty table resolves to 0 so the
// caller can fail closed with its "pass -ledger explicitly" error.
func TestAutoSnapshotLedger_NoCursors(t *testing.T) {
	if ledger, _ := autoSnapshotLedger(nil); ledger != 0 {
		t.Errorf("ledger = %d, want 0", ledger)
	}
}

// TestResolveSnapshotLedger_AutoUsesChainCursorEndToEnd wires the same
// scenario through resolveSnapshotLedger itself (opLedger=0), proving the
// close-time lookup is performed for the CHAIN cursor's ledger and not the
// ops job's.
func TestResolveSnapshotLedger_AutoUsesChainCursorEndToEnd(t *testing.T) {
	closeTime := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	reader := &fakeCloseTimeReader{closeTime: closeTime, found: true}
	cursors := &fakeCursorReader{cursors: []timescale.Cursor{
		{Source: "backfill", Sub: "63500000-63900000", LastLedger: 63_900_000},
		{Source: "ledgerstream", LastLedger: 63_000_000},
	}}

	ledger, observedAt, err := resolveSnapshotLedger(context.Background(), cursors, reader, 0)
	if err != nil {
		t.Fatalf("resolveSnapshotLedger: %v", err)
	}
	if ledger != 63_000_000 {
		t.Errorf("ledger = %d, want 63000000", ledger)
	}
	if reader.gotLedger != 63_000_000 {
		t.Errorf("close time resolved for ledger %d, want 63000000", reader.gotLedger)
	}
	if !observedAt.Equal(closeTime) {
		t.Errorf("observedAt = %s, want the ledger close time %s", observedAt, closeTime)
	}
}
