package projector

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AdvanceCursorFrom gives the in-memory store the same compare-and-swap
// contract as [timescale.Store.AdvanceCursorFrom], INCLUDING the shapes a
// happy-path fake would never produce: a row that moved since the read, a
// row that appeared since a not-found read, and a non-advancing write. The
// real statement is proven on TimescaleDB in
// test/integration/projector_cursor_cas_test.go; this only has to be
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

// TestCycle_ReplayRewindMidCycleIsNotClobbered is finding F159 at the unit
// layer: projector-replay's rewind lands while a cycle is in flight (here:
// from inside that cycle's own sink call, which is as mid-cycle as it
// gets). The cycle's commit is derived from the cursor it read at its
// start; it must NOT put the cursor back at tip, and the next cycle must
// re-walk the rewound range.
//
// RED on the unfixed code (commit = never-regress upsert of commitTo): the
// cursor reads 120 after cycle 1 and ledger 60 is never sunk again.
func TestCycle_ReplayRewindMidCycleIsNotClobbered(t *testing.T) {
	const (
		rewoundLedger  = uint32(60)  // below the cursor: already projected, to be re-driven
		inFlightLedger = uint32(110) // what cycle 1 is sinking when the rewind lands
		tip            = uint32(120)
		rewindTo       = uint32(49)
	)
	var (
		mu     sync.Mutex
		sunk   []uint32
		rewind func()
	)
	h := newWedgeHarness(t, "cas", []sorobanevents.Row{lakeRow(rewoundLedger, 1), lakeRow(inFlightLedger, 2)}, tip,
		func(ev consumer.Event) error {
			mu.Lock()
			sunk = append(sunk, ev.(ledgerEvent).ledger)
			r := rewind
			rewind = nil // once
			mu.Unlock()
			if r != nil {
				r()
			}
			return nil
		})
	rewind = func() { h.store.rewind(t, rewindTo) }

	// Cycle 1 — reads cursor=100, sinks ledger 110, rewind lands, commit.
	h.cycle()
	if got := h.store.cursor(); got != rewindTo {
		t.Fatalf("after the in-flight cycle the cursor = %d, want the replay's rewind point %d — the cycle's stale commit clobbered the rewind (F159)", got, rewindTo)
	}
	mu.Lock()
	if len(sunk) != 1 || sunk[0] != inFlightLedger {
		t.Fatalf("cycle 1 sunk %v, want exactly [%d] — the interleave did not arm", sunk, inFlightLedger)
	}
	sunk = nil
	mu.Unlock()

	// Cycle 2 — re-reads the rewound cursor and re-walks [50, 120].
	h.cycle()
	mu.Lock()
	defer mu.Unlock()
	if len(sunk) != 2 || sunk[0] != rewoundLedger || sunk[1] != inFlightLedger {
		t.Fatalf("cycle 2 sunk %v, want [%d %d] — the rewound range was not re-projected", sunk, rewoundLedger, inFlightLedger)
	}
	if got := h.store.cursor(); got != tip {
		t.Fatalf("after the re-walk the cursor = %d, want tip %d", got, tip)
	}
}

// TestCycle_FirstCycleSeedDoesNotOverwriteARowThatAppeared covers the
// not-found arm: a cycle that read "no cursor" must not install its commit
// over a row someone else created meanwhile.
func TestCycle_FirstCycleSeedDoesNotOverwriteARowThatAppeared(t *testing.T) {
	var appear func()
	h := newWedgeHarness(t, "cas-seed", []sorobanevents.Row{lakeRow(110, 1)}, 120,
		func(consumer.Event) error {
			if appear != nil {
				appear()
				appear = nil
			}
			return nil
		})
	h.store.haveCursor, h.store.projectorCursor = false, 0
	appear = func() {
		h.store.mu.Lock()
		h.store.haveCursor, h.store.projectorCursor = true, 30
		h.store.mu.Unlock()
	}
	h.cycle()
	if got := h.store.cursor(); got != 30 {
		t.Fatalf("cursor = %d, want 30 — a not-found read must seed with DO NOTHING semantics, not overwrite", got)
	}

	// And the plain seed still works when nothing races it.
	h2 := newWedgeHarness(t, "cas-seed-plain", []sorobanevents.Row{lakeRow(110, 1)}, 120,
		func(consumer.Event) error { return nil })
	h2.store.haveCursor, h2.store.projectorCursor = false, 0
	h2.window = 200 // [0, 120] in one cycle
	h2.cycle()
	if got := h2.store.cursor(); got != 120 {
		t.Fatalf("plain first-cycle seed: cursor = %d, want 120", got)
	}
}

// ctxStore refuses a cursor write on a done context, as pgx's ExecContext
// does; the base fake ignores ctx and so cannot see which one the write used.
type ctxStore struct{ *fakeStore }

func (s ctxStore) AdvanceCursorFrom(ctx context.Context, source, sub string, expected timescale.CursorRead, newLast uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return false, fmt.Errorf("fake: cursor write carries no deadline")
	}
	return s.fakeStore.AdvanceCursorFrom(ctx, source, sub, expected, newLast)
}

// TestCycle_SpentBudgetStillCommitsDurableProgress is finding T066: a cycle
// whose sink writes commit ledger 101 and then exhaust PerSourceTimeout (102
// fails with DeadlineExceeded) must still advance the cursor to 101. The
// write used to run on the expired cycle context, so it failed and the
// committed work was re-projected, identically, on every later cycle.
func TestCycle_SpentBudgetStillCommitsDurableProgress(t *testing.T) {
	const source = "t066-spent-budget-commit"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	h := newWedgeHarness(t, source, rows, 2000, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return nil // landed before the budget ran out
		}
		return context.DeadlineExceeded
	})
	h.proj.store = ctxStore{h.store}

	// A parent already past its deadline gives a cycleCtx born expired; the
	// fake stream ignores ctx, so the scan completes and only the sink and
	// the cursor write can observe the spent budget.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	h.cycleCtx(expired)

	if got := h.store.cursor(); got != 101 {
		t.Fatalf("cursor = %d, want 101 (ledger 101 committed; a spent cycle budget must not discard it)", got)
	}
	if want := uint32(BatchLimit / 2); h.window != want {
		t.Fatalf("window = %d, want %d (a budget-exhausted cycle keeps its sink-side shrink even when it commits)", h.window, want)
	}
}
