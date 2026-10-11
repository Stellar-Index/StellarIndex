package supply

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeCloseTimeReader is a DB-free ledgerCloseTimeReader serving a preset close time (or not-found / error) and recording the ledger asked; the exact-lookup knobs serve the operator -ledger branch, the lake* knobs the auto clamp.
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

// TestResolveSnapshotLedger_StampsLedgerCloseTimeNotWallClock: ObservedAt must be the chosen ledger's REAL close_time, never time.Now() (the fixture is ~2.5y stale so a wall-clock stamp is unmistakable); nil store since the operator branch never touches it.
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

// TestResolveSnapshotLedger_FailsClosedOnMissingLedgerRow: a ledger with no stellar.ledgers row errors rather than falling back to time.Now().
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

// TestAutoSnapshotLedger_PrefersTheChainCursor guards against MAX(last_ledger) over ingestion_cursors, which holds JOB positions: an ops backfill cursor at 63,900,000 must not override the indexer's 63,000,000, where the component balances were observed.
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

// TestAutoSnapshotLedger_FallsBackWhenNoChainCursor: the fallback works before the indexer's first cursor and NAMES itself in the run log.
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

// TestAutoSnapshotLedger_NoCursors: an empty table resolves to 0 so the caller fails closed.
func TestAutoSnapshotLedger_NoCursors(t *testing.T) {
	if ledger, _ := autoSnapshotLedger(nil); ledger != 0 {
		t.Errorf("ledger = %d, want 0", ledger)
	}
}

// TestResolveSnapshotLedger_AutoUsesChainCursorEndToEnd: the lake lookup is bounded by the CHAIN cursor's ledger, and the landed row supplies both position and stamp.
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

// TestResolveSnapshotLedger_AutoClampsToLandedLakeTip: the realtime cursor leads the lake by seconds, so the auto path clamps to the newest landed ledger at or before it and stamps that ledger's close time.
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

// TestResolveSnapshotLedger_AutoFailsClosedOnStalledLake: a lake behind the cursor by more than maxAutoSnapshotClampLedgers is a stalled sink, not a race.
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

// TestResolveSnapshotLedger_AutoFailsClosedOnEmptyLake: no landed ledger at or before the cursor never falls back to wall-clock.
func TestResolveSnapshotLedger_AutoFailsClosedOnEmptyLake(t *testing.T) {
	reader := &fakeCloseTimeReader{lakeFound: false}
	cursors := &fakeCursorReader{cursors: []timescale.Cursor{
		{Source: "ledgerstream", LastLedger: 64_000_000},
	}}
	if _, _, err := resolveSnapshotLedger(context.Background(), cursors, reader, 0); err == nil {
		t.Fatal("expected an error when the lake has no row at or before the cursor")
	}
}

// TestResolveSnapshotLedger_OperatorLedgerStaysExact: the clamp is auto-only; an operator-named -ledger absent from the lake fails, never shifts.
func TestResolveSnapshotLedger_OperatorLedgerStaysExact(t *testing.T) {
	reader := &fakeCloseTimeReader{found: false, lakeLedger: 39_999_000, lakeFound: true}
	if _, _, err := resolveSnapshotLedger(context.Background(), nil, reader, 40_000_000); err == nil {
		t.Fatal("expected fail-closed for an operator-named ledger absent from the lake — clamping an explicit position would silently rewrite the operator's request")
	}
	if reader.latestCalls != 0 {
		t.Errorf("operator branch consulted LatestLedgerAtOrBefore %d times, want 0", reader.latestCalls)
	}
}

// TestAutoSnapshotClampMatchesLookupWindow: the ledgers the resolver refuses past equal the ledgers the lake lookup reads (lockstep with cmd/stellarindex-aggregator/supply_snapshot_ledger_test.go).
func TestAutoSnapshotClampMatchesLookupWindow(t *testing.T) {
	if maxAutoSnapshotClampLedgers != clickhouse.LatestLedgerLookbackLedgers {
		t.Errorf("maxAutoSnapshotClampLedgers = %d but the lookup scans %d ledgers below the cursor — the refusal bound and the scan window must be the same constant",
			maxAutoSnapshotClampLedgers, clickhouse.LatestLedgerLookbackLedgers)
	}
}
