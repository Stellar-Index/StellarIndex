//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Findings F159 / K013 (audit 2026-09-02): the projector's cycle is a
// read-modify-write up to PerSourceTimeout long, and its commit was a
// never-regress UPSERT of a position derived from the cycle-start read. A
// projector-replay RewindCursor landing inside that gap wrote a LOWER
// value, so the in-flight cycle's forward write passed the guard and put
// the cursor back at tip — the replay printed success and re-projected
// nothing.
//
// RED on the unfixed behaviour: make AdvanceCursorFrom delegate to
// UpsertCursor (the old commit) and all three tests below fail.

const casSource = "sep41_supply"

func casCursor(t *testing.T, ctx context.Context, store *timescale.Store) timescale.Cursor { //nolint:revive // t-first matches the package's other helpers.
	t.Helper()
	c, err := store.GetCursor(ctx, "projector", casSource)
	if err != nil {
		t.Fatalf("read projector cursor: %v", err)
	}
	return c
}

// TestAdvanceCursorFrom_RewindHeldOpenWinsTheRace interleaves the two
// writers on two connections, both ways round.
func TestAdvanceCursorFrom_RewindHeldOpenWinsTheRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)
	racer := openVerdictStore(t, ctx, dsn) // second pool → second connection

	const (
		readAt   = uint32(63_700_000) // what the cycle read at its start
		commitTo = uint32(63_700_500) // what it derived from that read
		rewindTo = uint32(62_999_999) // projector-replay -from 63000000
	)

	// Seed plainly and go STRAIGHT to the interleave: the sequential contract
	// lives in TestAdvanceCursorFrom_SequentialContract, so that a failure
	// (or a pass) here is a statement about the race and nothing else.
	if err := store.UpsertCursor(ctx, "projector", casSource, readAt); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	// ── Interleave 1: the rewind is in flight when the cycle commits ─────
	tx, err := racer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("racer begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Byte-for-byte RewindCursor's statement, held uncommitted.
	if _, err := tx.ExecContext(ctx, `
        UPDATE ingestion_cursors
           SET last_ledger = $3, last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger > $3`,
		"projector", casSource, int64(rewindTo)); err != nil {
		t.Fatalf("racer rewind: %v", err)
	}

	type result struct {
		advanced bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		ok, aerr := store.AdvanceCursorFrom(ctx, "projector", casSource,
			timescale.CursorRead{Exists: true, LastLedger: readAt}, commitTo)
		done <- result{ok, aerr}
	}()

	// Non-vacuity: the advance must be parked in its cursor-row WRITE behind
	// the rewind's row lock; anything else means the interleave never armed.
	// Deliberately not pinned to the fix's exact statement text — the
	// property is "the cycle's commit is waiting on the rewind", and pinning
	// the text would make a reverted commit fail HERE instead of on the
	// clobber below, which is the assertion that matters.
	parked := waitForVerdictLockWait(t, ctx, racer.DB(), done2finished(done))
	t.Logf("cycle commit parked behind the rewind in: %s", strings.Join(strings.Fields(parked), " "))
	if !strings.Contains(parked, "ingestion_cursors") {
		t.Fatalf("advance parked in the wrong statement — interleave not armed.\nparked in: %s", parked)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("racer commit: %v", err)
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("advance did not return within 30s of the rewind's commit")
	}
	if res.err != nil {
		t.Fatalf("advance: %v", res.err)
	}
	if res.advanced {
		t.Errorf("advanced=true although the cursor was rewound under the cycle")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != rewindTo {
		t.Fatalf("cursor = %d, want the rewind point %d — the in-flight cycle's stale commit clobbered the rewind (F159)", c.LastLedger, rewindTo)
	}

	// ── Interleave 2: the cycle's commit is in flight when the rewind lands
	// Re-arm: cursor back at readAt via a legitimate advance.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: rewindTo}, readAt); err != nil || !ok {
		t.Fatalf("re-arm advance: advanced=%v err=%v", ok, err)
	}
	tx2, err := racer.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("racer begin 2: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	if _, err := tx2.ExecContext(ctx, `
        UPDATE ingestion_cursors SET last_ledger = $3, last_updated = now()
         WHERE source = $1 AND sub_source = $2 AND last_ledger = $4`,
		"projector", casSource, int64(commitTo), int64(readAt)); err != nil {
		t.Fatalf("racer advance: %v", err)
	}
	rdone := make(chan error, 1)
	go func() { rdone <- store.RewindCursor(ctx, "projector", casSource, rewindTo) }()
	parked = waitForVerdictLockWait(t, ctx, racer.DB(), done2finished(rdone))
	if !strings.Contains(parked, "UPDATE ingestion_cursors") || !strings.Contains(parked, "last_ledger > $3") {
		t.Fatalf("rewind parked in the wrong statement — interleave not armed.\nparked in: %s", parked)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("racer commit 2: %v", err)
	}
	select {
	case rerr := <-rdone:
		if rerr != nil {
			t.Fatalf("rewind behind an in-flight advance: %v", rerr)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("rewind did not return within 30s of the advance's commit")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != rewindTo {
		t.Fatalf("cursor = %d, want the rewind point %d — the rewind must win whichever writer holds the row first", c.LastLedger, rewindTo)
	}
}

// TestAdvanceCursorFrom_SequentialContract pins the statement's contract
// with no concurrency involved: the not-found seed (and its DO NOTHING arm),
// the refusal of a non-advancing write, the refusal of a stale read, and
// the half of UpsertCursor's contract the projector's cursor still relies
// on — first_ledger is set on insert and never moved by an advance.
func TestAdvanceCursorFrom_SequentialContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{}, 100); err != nil || !ok {
		t.Fatalf("seed: advanced=%v err=%v", ok, err)
	}
	// A second not-found-read seed must leave the existing row alone.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{}, 5); err != nil || ok {
		t.Fatalf("seed over an existing row: advanced=%v err=%v, want false/nil", ok, err)
	}
	// A write that is not an advance is a caller bug, not a silent no-op.
	if _, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 100); err == nil {
		t.Fatalf("a non-advancing write returned nil error")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != 100 {
		t.Fatalf("cursor = %d after the refused writes, want 100 untouched", c.LastLedger)
	}
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 250); err != nil || !ok {
		t.Fatalf("advance: advanced=%v err=%v", ok, err)
	}
	var first, last int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT first_ledger, last_ledger FROM ingestion_cursors WHERE source = 'projector' AND sub_source = $1`,
		casSource).Scan(&first, &last); err != nil {
		t.Fatalf("read cursor row: %v", err)
	}
	if first != 100 || last != 250 {
		t.Fatalf("first_ledger=%d last_ledger=%d, want 100/250", first, last)
	}
	// A stale read is refused and changes nothing.
	if ok, err := store.AdvanceCursorFrom(ctx, "projector", casSource, timescale.CursorRead{Exists: true, LastLedger: 100}, 300); err != nil || ok {
		t.Fatalf("stale read: advanced=%v err=%v, want false/nil", ok, err)
	}
	if c := casCursor(t, ctx, store); c.LastLedger != 250 {
		t.Fatalf("cursor = %d after a refused stale advance, want 250", c.LastLedger)
	}
}

// casSink records every ledger the projector sinks and parks the FIRST
// call until released — holding a real cycle open mid-flight, after its
// cursor read and before its commit.
type casSink struct {
	mu       sync.Mutex
	sunk     []uint32
	parkOnce sync.Once
	inFlight chan uint32
	release  chan struct{}
}

func (s *casSink) handle(ctx context.Context, ev consumer.Event) error {
	se, ok := ev.(sep41_supply.Event)
	if !ok {
		return nil
	}
	s.mu.Lock()
	s.sunk = append(s.sunk, se.Ledger)
	s.mu.Unlock()
	var park bool
	s.parkOnce.Do(func() { park = true })
	if park {
		s.inFlight <- se.Ledger
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *casSink) count(ledger uint32) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.sunk {
		if l == ledger {
			n++
		}
	}
	return n
}

// TestProjectorReplayRewind_SurvivesAnInFlightCycle is the finding end to
// end: the REAL projector ([projector.New] + Run) over the REAL store, a
// cycle held open mid-flight, and the REAL [timescale.Store.RewindCursor]
// — the call projector-replay makes — landing from a second connection.
// The repair must actually happen: the rewound ledger is sunk again.
func TestProjectorReplayRewind_SurvivesAnInFlightCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)
	replay := openVerdictStore(t, ctx, dsn) // the ops command's own connection

	const (
		rewoundLedger  = uint32(50_000_010) // already projected; the replay wants it re-driven
		inFlightLedger = uint32(50_000_020) // what the live cycle is sinking when the rewind lands
	)
	rowA := mkReconstructableRow(t, rewoundLedger)
	rowB := mkReconstructableRow(t, inFlightLedger)
	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{rowA, rowB}); err != nil {
		t.Fatalf("seed soroban_events: %v", err)
	}
	if err := store.UpsertCursor(ctx, "ledgerstream", "", inFlightLedger); err != nil {
		t.Fatalf("seed ledgerstream cursor: %v", err)
	}
	// The projector has already walked past rewoundLedger.
	if err := store.UpsertCursor(ctx, "projector", casSource, inFlightLedger-1); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	sink := &casSink{inFlight: make(chan uint32, 1), release: make(chan struct{})}
	reg := projector.Registry{Sources: []projector.Source{{Name: casSource, Decoder: &fakeSupplyDecoder{contractID: rowA.ContractID}}}}
	p := projector.New(store, reg, sink.handle, slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = p.Run(runCtx)
	}()
	var releaseOnce sync.Once
	releaseSink := func() { releaseOnce.Do(func() { close(sink.release) }) }
	t.Cleanup(func() {
		releaseSink()
		runCancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("projector Run did not exit within 30s of cancel")
		}
	})

	// The cycle is now provably mid-flight: it read cursor = inFlight-1 and
	// is inside its sink call for inFlightLedger.
	select {
	case got := <-sink.inFlight:
		if got != inFlightLedger {
			t.Fatalf("the parked cycle is sinking ledger %d, want %d — interleave not armed", got, inFlightLedger)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("projector never reached the sink")
	}
	if c := casCursor(t, ctx, store); c.LastLedger != inFlightLedger-1 {
		t.Fatalf("cursor = %d while the cycle is parked, want %d (uncommitted)", c.LastLedger, inFlightLedger-1)
	}

	// projector-replay -from rewoundLedger.
	if err := replay.RewindCursor(ctx, "projector", casSource, rewoundLedger-1); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	releaseSink() // the in-flight cycle now runs to its commit

	// The re-projection must happen. In the fixed code the re-walk sinks
	// rewoundLedger BEFORE the cursor can read inFlightLedger again, so a
	// cursor at inFlightLedger with rewoundLedger never sunk is the clobber.
	deadline := time.Now().Add(2*projectorSettle + 30*time.Second)
	for sink.count(rewoundLedger) == 0 {
		cur := casCursor(t, ctx, store).LastLedger
		if cur == inFlightLedger && sink.count(rewoundLedger) == 0 {
			t.Fatalf("the cursor is back at %d and ledger %d was never re-sunk — the in-flight cycle's stale commit reverted projector-replay's rewind, so the repair no-oped (F159)", cur, rewoundLedger)
		}
		if time.Now().After(deadline) {
			t.Fatalf("ledger %d was never re-projected after the rewind (cursor=%d)", rewoundLedger, cur)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// And the projector then catches back up, re-driving the in-flight
	// ledger too (idempotent downstream).
	for casCursor(t, ctx, store).LastLedger != inFlightLedger {
		if time.Now().After(deadline) {
			t.Fatalf("cursor never returned to %d after the re-walk", inFlightLedger)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := sink.count(inFlightLedger); n < 2 {
		t.Errorf("ledger %d sunk %d time(s), want ≥2 (once in flight, once in the re-walk)", inFlightLedger, n)
	}
}
