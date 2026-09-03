// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// resolveStartLedger decides where every indexer restart begins reading
// (#340 item 4). Each of its three arms fails in a different and
// expensive way, and none of them fails loudly at the point of the
// mistake:
//
//   - resuming from the cursor ITSELF rather than cursor+1 re-ingests
//     one ledger every restart;
//   - preferring backfill_from_ledger OVER a live cursor rewinds the
//     indexer to a stale config value — on r1 that is a multi-million-
//     ledger replay that looks like a normal (very long) catch-up;
//   - defaulting to some ledger when neither is set is exactly the
//     "operators re-ingest genesis by accident" case the function's own
//     docstring says it exists to prevent.
//
// The failure is always "the indexer is busy", never "the indexer is
// wrong", which is why the precedence is pinned rather than inspected.

// fakeCursorStore is a cursorReader that returns a scripted answer. It
// is a probe on the precedence decision, not a store double: the values
// it returns are the two inputs the decision is made from.
type fakeCursorStore struct {
	cursor timescale.Cursor
	err    error
	calls  int
	gotSrc string
	gotSub string
}

func (f *fakeCursorStore) GetCursor(_ context.Context, source, sub string) (timescale.Cursor, error) {
	f.calls++
	f.gotSrc, f.gotSub = source, sub
	return f.cursor, f.err
}

// TestResolveStartLedger_PersistedCursorWinsAndResumesPastIt is the
// common case: a running indexer restarting.
func TestResolveStartLedger_PersistedCursorWinsAndResumesPastIt(t *testing.T) {
	t.Parallel()

	const last = uint32(63_332_650)
	store := &fakeCursorStore{cursor: timescale.Cursor{LastLedger: last}}

	// backfillFrom is deliberately a LOWER, stale value — the shape an
	// operator's config actually has months after bring-up. The cursor
	// must win regardless.
	got, err := resolveStartLedger(context.Background(), store, 50_000_000)
	if err != nil {
		t.Fatalf("resolveStartLedger: %v", err)
	}
	if got != last+1 {
		t.Errorf("resume ledger = %d, want %d (cursor+1).\n"+
			"  == cursor   → every restart re-ingests ledger %d\n"+
			"  == backfill → the indexer rewinds %d ledgers to a stale config value, which "+
			"looks like a slow catch-up rather than a fault",
			got, last+1, last, int64(last)-50_000_000)
	}
}

// TestResolveStartLedger_CursorPlusOneAtTheLowBoundary isolates the
// off-by-one. A cursor at 1 must resume at 2.
func TestResolveStartLedger_CursorPlusOneAtTheLowBoundary(t *testing.T) {
	t.Parallel()

	store := &fakeCursorStore{cursor: timescale.Cursor{LastLedger: 1}}
	got, err := resolveStartLedger(context.Background(), store, 0)
	if err != nil {
		t.Fatalf("resolveStartLedger: %v", err)
	}
	if got != 2 {
		t.Errorf("resume ledger = %d, want 2 — internal/ledgerstream/seamed.go documents "+
			"that it relies on resolveStartLedger guaranteeing from >= 1", got)
	}
}

// TestResolveStartLedger_NoCursorFallsBackToBackfillFrom — first boot
// on a configured deployment.
func TestResolveStartLedger_NoCursorFallsBackToBackfillFrom(t *testing.T) {
	t.Parallel()

	const configured = uint32(63_000_000)
	store := &fakeCursorStore{err: timescale.ErrNotFound}

	got, err := resolveStartLedger(context.Background(), store, configured)
	if err != nil {
		t.Fatalf("resolveStartLedger: %v", err)
	}
	// NOT configured+1: the +1 belongs to the cursor arm only. A cursor
	// records the last ledger PROCESSED; backfill_from_ledger is the
	// first ledger to process, so adding one here would skip it.
	if got != configured {
		t.Errorf("resume ledger = %d, want exactly backfill_from_ledger (%d) — the +1 belongs "+
			"to the cursor arm only; a cursor names the last ledger DONE, while "+
			"backfill_from_ledger names the first ledger TO DO, so a +1 here silently "+
			"skips ledger %d", got, configured, configured)
	}
}

// TestResolveStartLedger_NeitherSetRefusesToGuess is the guard the
// docstring names. Returning 0 (or genesis, or the tip) instead of an
// error is the accidental-genesis-replay bug.
func TestResolveStartLedger_NeitherSetRefusesToGuess(t *testing.T) {
	t.Parallel()

	store := &fakeCursorStore{err: timescale.ErrNotFound}

	got, err := resolveStartLedger(context.Background(), store, 0)
	if err == nil {
		t.Fatalf("no cursor and backfill_from_ledger=0 returned %d with no error — the "+
			"function must refuse to pick a start ledger, which is how operators end up "+
			"re-ingesting from genesis by accident", got)
	}
	if got != 0 {
		t.Errorf("error path returned ledger %d, want 0 — a non-zero value alongside an error "+
			"invites a caller that ignores err to start there", got)
	}
	// The message has to tell the operator what to set; this is the only
	// output they get before the process exits.
	for _, want := range []string{"backfill_from_ledger", "cursor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; it is the operator's only instruction "+
				"before startup aborts", err, want)
		}
	}
}

// TestResolveStartLedger_StoreErrorIsPropagatedNotTreatedAsAbsent is the
// dangerous confusion. A transient Postgres failure must NOT be read as
// "no cursor" — that would silently rewind a live indexer to
// backfill_from_ledger, or abort, depending on config. Only the typed
// ErrNotFound means "no cursor row".
func TestResolveStartLedger_StoreErrorIsPropagatedNotTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")
	store := &fakeCursorStore{err: boom}

	got, err := resolveStartLedger(context.Background(), store, 63_000_000)
	if err == nil {
		t.Fatalf("a store error was swallowed and resolveStartLedger returned %d — a transient "+
			"Postgres failure would then rewind a live indexer to backfill_from_ledger", got)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the store error; the operator needs the cause", err)
	}
	if got != 0 {
		t.Errorf("error path returned ledger %d, want 0", got)
	}
}

// TestResolveStartLedger_ReadsTheIndexerCursorRow pins WHICH cursor is
// consulted. The store keys cursors by (source, sub_source); reading a
// different source's row would resume from an unrelated pipeline's
// position, and the value returned would still look plausible.
func TestResolveStartLedger_ReadsTheIndexerCursorRow(t *testing.T) {
	t.Parallel()

	store := &fakeCursorStore{cursor: timescale.Cursor{LastLedger: 100}}
	if _, err := resolveStartLedger(context.Background(), store, 0); err != nil {
		t.Fatalf("resolveStartLedger: %v", err)
	}
	if store.calls != 1 {
		t.Errorf("GetCursor called %d times, want exactly 1", store.calls)
	}
	if store.gotSrc != cursorSource {
		t.Errorf("read cursor for source %q, want %q — another source's row would resume from "+
			"an unrelated pipeline's position and still look plausible",
			store.gotSrc, cursorSource)
	}
	if store.gotSub != "" {
		t.Errorf("read sub_source %q, want the empty (whole-source) row", store.gotSub)
	}
}
