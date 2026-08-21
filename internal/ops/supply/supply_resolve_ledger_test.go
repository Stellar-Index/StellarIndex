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
// recording the ledger so the plumbing can be asserted. The exact-lookup
// knobs serve the operator -ledger branch; the lake* knobs serve the auto
// branch's LatestLedgerAtOrBefore clamp.
type fakeCloseTimeReader struct {
	closeTime time.Time
	found     bool
	err       error
	gotLedger uint32
	calls     int

	lakeLedger  uint32
	lakeClose   time.Time
	lakeFound   bool
	lakeErr     error
	gotMaxSeq   uint32
	latestCalls int
}

func (f *fakeCloseTimeReader) CloseTimeForLedger(_ context.Context, ledger uint32) (time.Time, bool, error) {
	f.calls++
	f.gotLedger = ledger
	return f.closeTime, f.found, f.err
}

func (f *fakeCloseTimeReader) LatestLedgerAtOrBefore(_ context.Context, maxSeq uint32) (uint32, time.Time, bool, error) {
	f.latestCalls++
	f.gotMaxSeq = maxSeq
	return f.lakeLedger, f.lakeClose, f.lakeFound, f.lakeErr
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
// lake lookup is bounded by the CHAIN cursor's ledger and not the ops
// job's, and that the lake's landed row supplies both position and stamp.
func TestResolveSnapshotLedger_AutoUsesChainCursorEndToEnd(t *testing.T) {
	closeTime := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	reader := &fakeCloseTimeReader{lakeLedger: 63_000_000, lakeClose: closeTime, lakeFound: true}
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
	if reader.gotMaxSeq != 63_000_000 {
		t.Errorf("lake lookup bounded by ledger %d, want 63000000 (the chain cursor, not the backfill job's)", reader.gotMaxSeq)
	}
	if !observedAt.Equal(closeTime) {
		t.Errorf("observedAt = %s, want the ledger close time %s", observedAt, closeTime)
	}
}

// TestResolveSnapshotLedger_AutoClampsToLandedLakeTip is the 2026-08-22
// r1 regression: the ledgerstream cursor (Postgres, realtime) leads the
// lake's stellar.ledgers (CH sink, lands seconds later), so at the moment
// a timer-driven snapshot fires the cursor's own row is routinely absent
// — the daily unit failed EVERY run. The auto path must clamp to the
// newest landed ledger at or before the cursor and stamp THAT ledger's
// close time.
func TestResolveSnapshotLedger_AutoClampsToLandedLakeTip(t *testing.T) {
	lakeClose := time.Date(2026, 8, 22, 1, 15, 0, 0, time.UTC)
	reader := &fakeCloseTimeReader{lakeLedger: 64_063_584, lakeClose: lakeClose, lakeFound: true}
	cursors := &fakeCursorReader{cursors: []timescale.Cursor{
		{Source: "ledgerstream", LastLedger: 64_063_590}, // 6 ledgers ahead of the lake
	}}

	ledger, observedAt, err := resolveSnapshotLedger(context.Background(), cursors, reader, 0)
	if err != nil {
		t.Fatalf("resolveSnapshotLedger: %v (the landing race must clamp, not fail)", err)
	}
	if ledger != 64_063_584 {
		t.Errorf("ledger = %d, want the lake's landed tip 64063584", ledger)
	}
	if !observedAt.Equal(lakeClose) {
		t.Errorf("observedAt = %s, want the landed ledger's close time %s", observedAt, lakeClose)
	}
	if reader.calls != 0 {
		t.Errorf("auto path made %d exact CloseTimeForLedger lookups, want 0 (the clamp owns the auto path)", reader.calls)
	}
}

// TestResolveSnapshotLedger_AutoFailsClosedOnStalledLake bounds the clamp:
// a lake trailing the cursor by more than maxAutoSnapshotClampLedgers is a
// stalled sink, not a landing race, and stamping a snapshot that far
// behind the chain would hide the stall.
func TestResolveSnapshotLedger_AutoFailsClosedOnStalledLake(t *testing.T) {
	reader := &fakeCloseTimeReader{
		lakeLedger: 64_000_000 - maxAutoSnapshotClampLedgers - 1,
		lakeClose:  time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
		lakeFound:  true,
	}
	cursors := &fakeCursorReader{cursors: []timescale.Cursor{
		{Source: "ledgerstream", LastLedger: 64_000_000},
	}}
	if _, _, err := resolveSnapshotLedger(context.Background(), cursors, reader, 0); err == nil {
		t.Fatal("expected an error when the lake trails the cursor beyond the clamp bound — a silent clamp would hide a stalled lake")
	}
}

// TestResolveSnapshotLedger_AutoFailsClosedOnEmptyLake — no landed ledger
// at or before the cursor means the lake is empty or wholly gapped; never
// fall back to wall-clock.
func TestResolveSnapshotLedger_AutoFailsClosedOnEmptyLake(t *testing.T) {
	reader := &fakeCloseTimeReader{lakeFound: false}
	cursors := &fakeCursorReader{cursors: []timescale.Cursor{
		{Source: "ledgerstream", LastLedger: 64_000_000},
	}}
	if _, _, err := resolveSnapshotLedger(context.Background(), cursors, reader, 0); err == nil {
		t.Fatal("expected an error when the lake has no row at or before the cursor")
	}
}

// TestResolveSnapshotLedger_OperatorLedgerStaysExact — the clamp is an
// AUTO-path affordance only. An operator-named -ledger absent from the
// lake is a genuine gap and must fail exactly as before, never shift.
func TestResolveSnapshotLedger_OperatorLedgerStaysExact(t *testing.T) {
	reader := &fakeCloseTimeReader{found: false, lakeLedger: 39_999_000, lakeFound: true}
	if _, _, err := resolveSnapshotLedger(context.Background(), nil, reader, 40_000_000); err == nil {
		t.Fatal("expected fail-closed for an operator-named ledger absent from the lake — clamping an explicit position would silently rewrite the operator's request")
	}
	if reader.latestCalls != 0 {
		t.Errorf("operator branch consulted LatestLedgerAtOrBefore %d times, want 0", reader.latestCalls)
	}
}
