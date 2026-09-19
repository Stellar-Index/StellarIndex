//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// commitSEP41ProjectorCursor commits the sep41_supply projection's cursor
// through the exact pair of calls internal/projector makes at the end of a
// cycle — GetCursor → [timescale.CursorRead] → AdvanceCursorFrom, i.e.
// Projector.commitCursor, whose compare-and-swap replaced UpsertCursor so a
// `projector-replay` rewind landing mid-cycle is not reverted (F159).
//
// The storage layer hard-codes the ("projector", "sep41_supply") pair the
// rollup fold's settled bound reads (sep41SupplyCursorSource /
// sep41SupplyCursorSub); restating that pair in a test pins nothing. Driving
// it through the projector's OWN writer is what ties the fold's bound to the
// row the projector actually writes — if either side is renamed, or the
// projector's commit stops landing on that row, the fold falls back to its
// fail-closed 0 and the assertions below go red.
func commitSEP41ProjectorCursor(t *testing.T, ctx context.Context, store *timescale.Store, commitTo uint32) { //nolint:revive // t-first matches the package's other helpers (startTimescale).
	t.Helper()
	var read timescale.CursorRead
	cur, err := store.GetCursor(ctx, "projector", sep41supply.SourceName)
	switch {
	case err == nil:
		read = timescale.CursorRead{Exists: true, LastLedger: cur.LastLedger}
	case errors.Is(err, timescale.ErrNotFound):
		read = timescale.CursorRead{} // the source's first cycle
	default:
		t.Fatalf("GetCursor(projector, %s): %v", sep41supply.SourceName, err)
	}
	advanced, err := store.AdvanceCursorFrom(ctx, "projector", sep41supply.SourceName, read, commitTo)
	if err != nil {
		t.Fatalf("AdvanceCursorFrom(projector, %s, %+v, %d): %v", sep41supply.SourceName, read, commitTo, err)
	}
	if !advanced {
		t.Fatalf("AdvanceCursorFrom(projector, %s, %+v, %d) did not advance the cursor", sep41supply.SourceName, read, commitTo)
	}
}

// TestSEP41SupplyRollup_HoldAndAdvanceInterleavedWithAFold pins audit-2026-09-02
// F109 — the sibling of F118 stated from the guard's own claim rather than from
// the undercount it produced.
//
// The claim under test: the fold's `< max(ledger)` guard alone does NOT mean
// "everything below is settled". The projector does not abort a cycle on a
// transient sink fault. It appends the row to `held`, logs "holding cursor for
// retry (NOT advancing past this ledger)" and RETURNS from the per-event
// closure — the stream callback carries on and the rest of the window's rows
// are written, durably, above the held one; only the CURSOR is capped, at
// (lowest held ledger − 1). So max(ledger) runs ahead of settlement by the
// whole remainder of the window, and a fold bounded by max(ledger) folds
// straight past the held ledger. When the retry finally writes it, the row is
// below last_ledger — no later fold looks there (the incremental watermark only
// scans above it) and the reader's live delta only adds `ledger > last_ledger`
// — so its amount is permanently absent from served SEP-41 supply.
//
// Unlike TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor, which walks the
// same states sequentially on one connection, this test pins the INTERLEAVE the
// finding names, on two connections and without a race: the rollup pass is held
// parked in its row-locking statement (asserted via pg_stat_activity, so the
// window is provably armed and the test cannot pass vacuously), the
// hold-and-advance writes land on another connection while it is parked, and
// only then is it released — so its folding statement's snapshot sees exactly
// the production state, max(ledger) far above the projector's cursor. It also
// drives the cursor through the projector's real commit path
// ([commitSEP41ProjectorCursor]) rather than restating it.
//
// The assertion is the money one: served lifetime supply after the retry lands.
func TestSEP41SupplyRollup_HoldAndAdvanceInterleavedWithAFold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		sorobanContract = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" // synthetic

		lEarly uint32 = 900  // folded by the first, uncontended pass
		lPrior uint32 = 950  // settled, but the tip when the first pass runs
		lHeld  uint32 = 1000 // the sink write that faults; written only on retry
		lAfter uint32 = 1003 // the last row the cycle writes ABOVE the held one

		mintEarly int64 = 700_000
		burnPrior int64 = 100_000
		mintHeld  int64 = 5_000_000 // the amount the pre-fix bound loses
		mintAfter int64 = 1_000     // each of the three post-hold ledgers

		readAt uint32 = 5000
	)
	// The projector caps its cursor at (lowest held ledger − 1) and re-reads
	// from there next cycle.
	const cursorWhileHolding = lHeld - 1

	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	insert := func(ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: sorobanContract, Ledger: ledger, TxHash: fmt.Sprintf("%064x", tx), OpIndex: 0,
			ObservedAt: t0.Add(time.Duration(ledger) * time.Second),
			Kind:       kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s@%d: %v", kind, ledger, err)
		}
	}

	// ─── Clean cycles up to lPrior; the projector has committed through 999.
	insert(lEarly, timescale.SEP41EventMint, mintEarly, 1)
	insert(lPrior, timescale.SEP41EventBurn, burnPrior, 2)
	commitSEP41ProjectorCursor(t, ctx, store, cursorWhileHolding)

	// An uncontended pass, so the rollup row exists for the blocker to lock.
	// lPrior is max(ledger) here, so only lEarly folds.
	first, err := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract)
	if err != nil {
		t.Fatalf("first advance: %v", err)
	}
	if first.ToLedger != lEarly {
		t.Fatalf("first advance ToLedger = %d; want %d (the tip %d is deferred by the < max(ledger) guard)",
			first.ToLedger, lEarly, lPrior)
	}

	// ─── Park the next pass inside its row-locking statement ──────────────
	blockerConn, err := rawdb.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	defer func() { _ = blockerConn.Close() }()
	blockerTx, err := blockerConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = blockerTx.Rollback()
		}
	}()
	var one int
	if err := blockerTx.QueryRowContext(ctx, `
            SELECT 1 FROM sep41_supply_rollup WHERE contract_id = $1 FOR UPDATE
    `, sorobanContract).Scan(&one); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	type advResult struct {
		res timescale.SEP41RollupAdvance
		err error
	}
	done := make(chan advResult, 1)
	go func() {
		res, aerr := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract)
		done <- advResult{res, aerr}
	}()
	parked := waitForSEP41PassLockWait(t, ctx, rawdb, func() (string, bool) {
		select {
		case r := <-done:
			return fmt.Sprintf("res=%+v err=%v", r.res, r.err), true
		default:
			return "", false
		}
	})
	// Non-vacuity: the pass must be parked where it takes the rollup row's
	// lock. If it had already returned, or parked earlier, the folding
	// statement's snapshot would predate the writes below and the interleave
	// this test exists for would never have armed.
	if !strings.Contains(parked, "FOR UPDATE") {
		t.Fatalf("the rollup pass is parked in %q, not in its row-locking statement — the interleave never armed", parked)
	}

	// ─── Hold-and-advance, while the pass is parked ───────────────────────
	// The sink write for lHeld faults transiently, so that row is NOT
	// written. The projector keeps going: every later ledger in the window is
	// written and committed, and the cursor stays at lHeld-1.
	for i, ledger := 0, lHeld+1; ledger <= lAfter; i, ledger = i+1, ledger+1 {
		insert(ledger, timescale.SEP41EventMint, mintAfter, 10+i)
	}

	if err := blockerTx.Commit(); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}
	released = true

	// ─── The pass's folding statement now runs against that state ─────────
	var adv timescale.SEP41RollupAdvance
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("advance across the hold: %v", r.err)
		}
		adv = r.res
	case <-time.After(60 * time.Second):
		t.Fatal("rollup pass did not finish after the blocker released")
	}
	// max(ledger) now says lAfter; the projector's cursor says only ledgers
	// ≤ cursorWhileHolding have committed. The fold must believe the cursor.
	// Errorf, not Fatalf: an over-folded checkpoint is the CAUSE, and the
	// served undercount below is the consequence the finding is about. Both
	// must be reported from one run, or a regression reads as a lone
	// bookkeeping nit rather than as money missing from the served answer.
	if adv.ToLedger != lPrior {
		t.Errorf("advance across the hold ToLedger = %d; want %d — the fold must not pass ledger %d, which the projector is holding for retry (cursor=%d)",
			adv.ToLedger, lPrior, lHeld, cursorWhileHolding)
	}
	var lastLedger int64
	if err := rawdb.QueryRowContext(ctx,
		`SELECT last_ledger FROM sep41_supply_rollup WHERE contract_id = $1`,
		sorobanContract).Scan(&lastLedger); err != nil {
		t.Fatalf("read rollup row: %v", err)
	}
	if lastLedger != int64(lPrior) {
		t.Errorf("checkpoint last_ledger = %d; want %d — a checkpoint above %d strands the held row for good",
			lastLedger, lPrior, lHeld)
	}

	// ─── The retry succeeds and a clean cycle commits through lAfter ──────
	insert(lHeld, timescale.SEP41EventMint, mintHeld, 3)
	commitSEP41ProjectorCursor(t, ctx, store, lAfter)
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract); err != nil {
		t.Fatalf("advance after the retry: %v", err)
	}

	// ─── The money assertion ──────────────────────────────────────────────
	const wantMint = mintEarly + mintHeld + 3*mintAfter
	check := func(stage string) {
		t.Helper()
		got, terr := store.SEP41KindTotalsAtOrBefore(ctx, sorobanContract, readAt)
		if terr != nil {
			t.Fatalf("%s: SEP41KindTotalsAtOrBefore: %v", stage, terr)
		}
		if got.Mint.Cmp(big.NewInt(wantMint)) != 0 || got.Burn.Cmp(big.NewInt(burnPrior)) != 0 {
			t.Errorf("%s: served totals @%d = mint=%s burn=%s; want mint=%d burn=%d (the retried mint at ledger %d is %d)",
				stage, readAt, got.Mint, got.Burn, wantMint, burnPrior, lHeld, mintHeld)
		}
		// Independent oracle: SEP41NetMintAtOrBefore never consults the
		// rollup, so the fast path and the authoritative full aggregate must
		// agree — the invariant the whole checkpoint design rests on.
		full, ferr := store.SEP41NetMintAtOrBefore(ctx, sorobanContract, readAt)
		if ferr != nil {
			t.Fatalf("%s: SEP41NetMintAtOrBefore: %v", stage, ferr)
		}
		net := new(big.Int).Sub(got.Mint, new(big.Int).Add(got.Burn, got.Clawback))
		if net.Cmp(full) != 0 {
			t.Errorf("%s: rollup fast path net = %s, authoritative full sum = %s — the checkpoint dropped %s",
				stage, net, full, new(big.Int).Sub(full, net))
		}
	}
	check("after the retry landed")

	// Permanence: the incremental watermark only looks ABOVE last_ledger, so
	// a row stranded below it is never repaired by a later cadence — the
	// undercount would be silent and forever.
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract); err != nil {
		t.Fatalf("follow-up advance: %v", err)
	}
	check("after the next cadence")
}
