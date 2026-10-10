//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// commitSEP41ProjectorCursor commits the sep41_supply projection's cursor
// through the exact pair of calls internal/projector makes at the end of a
// cycle — GetCursor → [timescale.CursorRead] → AdvanceCursorFrom, i.e.
// Projector.commitCursor, whose compare-and-swap replaced UpsertCursor so a
// `projector-replay` rewind landing mid-cycle is not reverted.
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

// TestSEP41SupplyRollup_HoldAndAdvanceInterleavedWithAFold pins the sibling
// of the settled-bound test, stated from the guard's own claim rather than
// from the undercount it produces.
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
		mintHeld  int64 = 5_000_000 // the amount the unsettled bound loses
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

// TestSEP41SupplyRollupReset_ContendedRowFallsBackToZero pins the failure
// path of the in-place reset: a contract whose re-fold cannot take its row
// lock within the lock timeout must not keep the fold the caller's rewrite
// invalidated. It is zeroed instead (the reader's exact full-sum path), the
// call reports the fallback as an error, and an uncontended contract in the
// same full reset is still re-folded in place.
func TestSEP41SupplyRollupReset_ContendedRowFallsBackToZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contended = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		free      = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"

		lSettled1 uint32 = 50457500
		lRecover  uint32 = 50457600 // below the checkpoint
		lSettled2 uint32 = 50458000
		lTip      uint32 = 50460000
		readAt    uint32 = 50460000

		wantLifetime int64 = 1_000_000 + 700_000 + 500_000 + 1
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	insert := func(c string, ledger uint32, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: c, Ledger: ledger, TxHash: fmt.Sprintf("%064x", tx), OpIndex: 0,
			ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s mint@%d: %v", c, ledger, err)
		}
	}
	readRow := func(c string) (string, int64) {
		t.Helper()
		var mint string
		var last int64
		if err := rawdb.QueryRowContext(ctx,
			`SELECT mint_total::text, last_ledger FROM sep41_supply_rollup WHERE contract_id = $1`, c).
			Scan(&mint, &last); err != nil {
			t.Fatalf("read rollup %s: %v", c, err)
		}
		return mint, last
	}

	for _, c := range []string{contended, free} {
		insert(c, lSettled1, 1_000_000, 1)
		insert(c, lSettled2, 500_000, 2)
		insert(c, lTip, 1, 3)
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance %s: %v", c, err)
		}
		insert(c, lRecover, 700_000, 4) // the re-derive's below-checkpoint row
	}

	// Hold the contended row past the reset's lock timeout, then release it
	// once the zeroing fallback is queued behind it.
	blocker, err := rawdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = blocker.Rollback()
		}
	}()
	var one int
	if err := blocker.QueryRowContext(ctx,
		`SELECT 1 FROM sep41_supply_rollup WHERE contract_id = $1 FOR UPDATE`, contended).Scan(&one); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	done := make(chan sep41ResetResult, 1)
	go func() {
		n, rerr := store.ResetSEP41SupplyRollupFold(ctx, nil)
		done <- sep41ResetResult{n, rerr}
	}()
	waitForSEP41FallbackLockWait(t, ctx, rawdb, done)
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("blocker release: %v", err)
	}
	released = true

	var r sep41ResetResult
	select {
	case r = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("reset did not return after the blocker released")
	}
	if r.err == nil {
		t.Fatal("reset returned nil error; want the zeroing fallback reported")
	}
	if r.n != 2 {
		t.Errorf("reset touched %d rows; want 2 (one re-folded, one zeroed)", r.n)
	}

	if mint, last := readRow(free); mint != "2200000" || last != int64(lSettled2) {
		t.Errorf("uncontended fold = mint %s last_ledger %d; want 2200000 / %d (re-folded in place)", mint, last, lSettled2)
	}
	if mint, last := readRow(contended); mint != "0" || last != 0 {
		t.Errorf("contended fold = mint %s last_ledger %d; want 0 / 0 (stale fold must not survive a failed re-fold)", mint, last)
	}
	for _, c := range []string{contended, free} {
		if got := lifetimeSEP41Mint(t, ctx, store, c, readAt); got != wantLifetime {
			t.Errorf("%s served lifetime mint = %d; want %d", c, got, wantLifetime)
		}
	}
}

type sep41ResetResult struct {
	n   int64
	err error
}

// waitForSEP41FallbackLockWait blocks until the reset's zeroing fallback (the
// multi-contract ANY($1) statement) is parked on a row lock, failing if the
// reset returns first.
func waitForSEP41FallbackLockWait(t *testing.T, ctx context.Context, db *sql.DB, done <-chan sep41ResetResult) { //nolint:revive // t-first matches the package's other helpers (startTimescale).
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND query LIKE '%ANY($1)%'`).Scan(&n)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		select {
		case r := <-done:
			t.Fatalf("reset returned (n=%d err=%v) before its fallback waited on the held row", r.n, r.err)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("zeroing fallback never waited on the held row within 60s")
}

// TestSEP41SupplyRollup_ResetSeenButRewriteUnseenIsNotStranded pins the
// residual half of the reset race that the row lock alone does not
// close: the fold must not pair a POST-reset boundary with a PRE-rewrite
// view of sep41_supply_events.
//
// `ch-rebuild -sep41 -write` and `projector-replay -source sep41_supply`
// both rewrite event history BELOW the checkpoint and THEN reset the fold,
// against a live aggregator. Under READ COMMITTED a statement's snapshot is
// taken when the statement starts, but a `SELECT … FOR UPDATE` inside it
// returns the LATEST committed version of the row it locks (it follows the
// update chain / re-checks after a lock wait). So when the lock is taken
// inside the folding statement, a pass whose statement began just before the
// rewrite's last commit reads last_ledger = 0 from the reset — and sums the
// events as they stood BEFORE the rewrite. It folds "from zero" over the old
// set and moves last_ledger back above the corrected row, which is then
// below the checkpoint (no later pass looks there) and below the reader's
// live delta: a served undercount that nothing repairs, reported as a clean
// advance. That is the exact outcome the reset exists to prevent.
//
// The visibility state is pinned, not raced. The reset is held open on its
// own connection so the pass is provably parked INSIDE its locking statement
// (pg_stat_activity), the re-derived row then commits, and only then does
// the reset commit. From the pass's side this is indistinguishable from the
// production order (rewrite commits, reset commits, both between the pass's
// statement start and its lock acquisition): the reset is visible to it and
// the rewrite is not.
func TestSEP41SupplyRollup_ResetSeenButRewriteUnseenIsNotStranded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		sorobanContract = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"

		lSettled1  uint32 = 50457500
		lRederived uint32 = 50457800 // written by the re-derive, BELOW the checkpoint
		lSettled2  uint32 = 50458000 // the checkpoint before the reset
		lTip       uint32 = 50460000 // deferred by the < max(ledger) guard
		readAt     uint32 = 50460000

		settled1  int64 = 1_000_000
		rederived int64 = 250_000
		settled2  int64 = 500_000
		tipMint   int64 = 1

		wantFold     = "1750000" // settled1 + rederived + settled2
		wantLifetime = settled1 + rederived + settled2 + tipMint
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	insert := func(ledger uint32, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: sorobanContract, Ledger: ledger, TxHash: fmt.Sprintf("%064x", tx), OpIndex: 0,
			ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert mint@%d: %v", ledger, err)
		}
	}

	insert(lSettled1, settled1, 1)
	insert(lSettled2, settled2, 2)
	insert(lTip, tipMint, 3)
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract); err != nil {
		t.Fatalf("first advance: %v", err)
	}

	// ─── The reset, held open so the pass parks inside its lock ───────────
	blockerConn, err := rawdb.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	defer func() { _ = blockerConn.Close() }()
	blockerTx, err := blockerConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = blockerTx.Rollback()
		}
	}()
	// A LOCKER-ONLY hold on the rollup row. It is the pause instrument, not
	// part of the scenario: the pass's row-materialising INSERT … ON CONFLICT
	// DO NOTHING does not wait on a locker-only xmax (it DOES wait on an
	// in-progress UPDATE), so the pass runs on and parks where it takes the
	// row lock — which is the statement whose snapshot this test is about.
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
	// Non-vacuity: the pass must be parked where it LOCKS the rollup row. If
	// it parked anywhere earlier (the row-materialising INSERT), the folding
	// statement has not started and the window under test was never opened.
	if !strings.Contains(parked, "FOR UPDATE") {
		t.Fatalf("the rollup pass is parked in %q, not in its row-locking statement — the interleave never armed", parked)
	}

	// Production order, inside the pass's parked statement: the re-derive's
	// row commits FIRST …
	insert(lRederived, rederived, 4)
	// … and THEN the reset runs — byte-for-byte the scoped statement
	// ResetSEP41SupplyRollupFold issues — and commits.
	if _, err := blockerTx.ExecContext(ctx, `
            UPDATE sep41_supply_rollup
               SET mint_total = 0, burn_total = 0, clawback_total = 0,
                   last_ledger = 0, updated_at = now()
             WHERE contract_id = ANY($1)
    `, []string{sorobanContract}); err != nil {
		t.Fatalf("blocker reset: %v", err)
	}

	if err := blockerTx.Commit(); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}
	committed = true

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("advance across the reset: %v", r.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("rollup pass did not finish after the reset committed")
	}

	check := func(stage string) {
		t.Helper()
		var mint string
		var lastLedger int64
		if err := rawdb.QueryRowContext(ctx, `
			SELECT mint_total::text, last_ledger
			  FROM sep41_supply_rollup WHERE contract_id = $1`, sorobanContract).
			Scan(&mint, &lastLedger); err != nil {
			t.Fatalf("read rollup row: %v", err)
		}
		if mint != wantFold || lastLedger != int64(lSettled2) {
			t.Errorf("%s: fold = mint %s last_ledger %d; want %s / %d (a pass that sees the reset must also see the rewrite the reset was issued for)",
				stage, mint, lastLedger, wantFold, lSettled2)
		}
		if got := lifetimeSEP41Mint(t, ctx, store, sorobanContract, readAt); got != wantLifetime {
			t.Errorf("%s: served lifetime mint = %d; want %d (the re-derived row at ledger %d is stranded below the checkpoint)",
				stage, got, wantLifetime, lRederived)
		}
	}
	check("after the pass that crossed the reset")

	// Permanence: the incremental watermark only looks ABOVE last_ledger, so
	// a stranded row is never repaired by a later cadence.
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sorobanContract); err != nil {
		t.Fatalf("follow-up advance: %v", err)
	}
	check("after the next cadence")
}

// waitForSEP41PassLockWait blocks until a backend in the test database is
// waiting on a heavyweight lock — i.e. the rollup pass is parked behind the
// blocker — and returns the text of the statement it is parked in, so the
// caller can assert WHERE it parked. finished reports whether the pass already
// returned; if it did, the interleave never armed and the test must fail, not
// pass vacuously.
func waitForSEP41PassLockWait(t *testing.T, ctx context.Context, db *sql.DB, finished func() (string, bool)) string { //nolint:revive // t-first matches the package's other helpers (startTimescale).
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var parked string
		err := db.QueryRowContext(ctx, `
			SELECT query FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			 LIMIT 1`).Scan(&parked)
		switch {
		case err == nil:
			return parked
		case !errors.Is(err, sql.ErrNoRows):
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if detail, ok := finished(); ok {
			t.Fatalf("the rollup pass finished without ever waiting on the row lock (%s) — the interleave never armed, so this test would prove nothing", detail)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("rollup pass never blocked on the rollup row lock within 30s")
	return ""
}

// 0085's stored comments, restored verbatim by 0194 down.
const (
	sep41RollupTableComment0085 = "Incremental per-contract mint/burn/clawback checkpoint for SEP-41 " +
		"Algorithm-3 supply. Advanced by the aggregator rollup worker; read " +
		"as rollup + sargable delta by SEP41KindTotalsAtOrBefore. Prevents " +
		"the full-history per-tick aggregate over sep41_supply_events " +
		"(incident 2026-07-06)."
	sep41RollupLastLedgerComment0085 = "Highest SETTLED ledger folded into the totals; the reader adds the " +
		"live delta above it up to the request ledger."
)

// TestSEP41SupplyRollupComment executes 0194 up and down against a real
// database: only pg_description can show whether the stored text an operator
// reads in \d+ still describes a single writer and a TRUNCATE recovery.
func TestSEP41SupplyRollupComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	table, lastLedger := sep41RollupComments(t, ctx, db)
	for _, want := range []string{"seed-sep41-genesis", "ResetSEP41SupplyRollupFold", "Never TRUNCATE"} {
		if !strings.Contains(table, want) {
			t.Errorf("sep41_supply_rollup comment missing %q; got %q", want, table)
		}
	}
	if strings.Contains(table, "disjoint") {
		t.Errorf("sep41_supply_rollup comment claims disjoint writer columns; the fold columns are shared: %q", table)
	}
	if !strings.Contains(lastLedger, "reset") {
		t.Errorf("last_ledger comment does not explain a reset row; got %q", lastLedger)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	// Migrate to 193, not Steps(-1): the latter rolls back whichever
	// migration is newest, so every later migration would turn this red.
	if err := m.Migrate(193); err != nil {
		t.Fatalf("0194 down: %v", err)
	}
	table, lastLedger = sep41RollupComments(t, ctx, db)
	if table != sep41RollupTableComment0085 {
		t.Errorf("0194 down did not restore 0085's table comment verbatim:\n got %q\nwant %q", table, sep41RollupTableComment0085)
	}
	if lastLedger != sep41RollupLastLedgerComment0085 {
		t.Errorf("0194 down did not restore 0085's last_ledger comment verbatim:\n got %q\nwant %q", lastLedger, sep41RollupLastLedgerComment0085)
	}
}

func sep41RollupComments(t *testing.T, ctx context.Context, db *sql.DB) (table, lastLedger string) {
	t.Helper()
	var tc, lc sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('sep41_supply_rollup'::regclass, 'pg_class'),
		        col_description('sep41_supply_rollup'::regclass,
		            (SELECT attnum FROM pg_attribute WHERE attrelid = 'sep41_supply_rollup'::regclass AND attname = 'last_ledger'))`,
	).Scan(&tc, &lc); err != nil {
		t.Fatalf("read sep41_supply_rollup comments: %v", err)
	}
	if !tc.Valid || !lc.Valid {
		t.Fatal("sep41_supply_rollup or last_ledger has no catalog comment")
	}
	return tc.String, lc.String
}

// settleSEP41Cursor marks the sep41_supply projection DURABLE through
// `ledger` by writing the projector's ingestion cursor on the exact
// (source, sub) pair internal/projector commits to — `"projector"` /
// `src.Name` — so the pairing the storage layer hard-codes
// (sep41SupplyCursorSource / sep41SupplyCursorSub) is pinned end-to-end
// rather than restated.
//
// The pair is the production shape; the CALL is not, deliberately. The
// projector's only cursor write is `AdvanceCursorFrom` — a compare-and-swap
// against the position the cycle read, so that an
// in-flight cycle cannot clobber a `projector-replay` rewind. Seeding a
// starting position has nothing to compare against, so this uses the
// unconditional `UpsertCursor`. A helper that claimed to seed "through the
// exact call the projector makes" would send a reader to a call the projector
// no longer has.
//
// AdvanceSEP41SupplyRollup folds no further than this watermark,
// so a rollup test that wants "everything below the tip has settled"
// must say so explicitly.
func settleSEP41Cursor(t *testing.T, ctx context.Context, store *timescale.Store, ledger uint32) { //nolint:revive // t-first matches the file's other helpers (startTimescale).
	t.Helper()
	if err := store.UpsertCursor(ctx, "projector", sep41supply.SourceName, ledger); err != nil {
		t.Fatalf("UpsertCursor(projector, %s, %d): %v", sep41supply.SourceName, ledger, err)
	}
}

// sep41SettledTestCursorLedger is far above every ledger the rollup tests
// use, so seeding it leaves `< max(ledger)` as the binding half of the
// settled bound and those tests keep pinning the tip-deferral guard,
// not the durability guard.
const sep41SettledTestCursorLedger = 200_000_000

// TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor pins that
// AdvanceSEP41SupplyRollup must not fold past the ledger the
// projector is still holding for retry, or the retried row lands
// permanently between the two halves of the served read and the token's
// supply is silently under-counted.
//
// The scenario is the production one. A supply-affecting event at ledger L
// hits a transient sink fault (deadlock / statement_timeout). The projector
// does NOT abort the cycle: it writes the rest of the window (L+1 …) and
// caps its cursor at L-1 so L is re-read next cycle. Meanwhile the 5-minute
// rollup pass runs. Bounded by max(ledger) alone it folded L+1 … into the
// checkpoint and pushed last_ledger ABOVE L; when the retry finally wrote
// L, no later fold could see it (folds look only above last_ledger) and the
// reader's live delta could not see it either (it adds only
// `ledger > last_ledger`) — the amount vanished from served supply with the
// pass reporting a normal advance.
//
// Also pins the fail-closed default: with no projector cursor at all there
// is no evidence of settlement, so the pass folds NOTHING and the reader
// still answers exactly (from the full-sum path, at a higher query cost).
func TestSEP41SupplyRollup_SettledBoundIsTheDurableCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" // synthetic
	const uncursored = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6" // synthetic
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(contract string, ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contract, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: t0.Add(time.Duration(ledger) * time.Second),
			Kind:       kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s %s@%d: %v", contract, kind, ledger, err)
		}
	}
	totals := func(label, contract string, asOf uint32) timescale.SEP41KindTotals {
		t.Helper()
		got, terr := store.SEP41KindTotalsAtOrBefore(ctx, contract, asOf)
		if terr != nil {
			t.Fatalf("%s: SEP41KindTotalsAtOrBefore(%s@%d): %v", label, contract, asOf, terr)
		}
		return got
	}
	assertTotals := func(label, contract string, asOf uint32, mint, burn int64) {
		t.Helper()
		got := totals(label, contract, asOf)
		if got.Mint.Cmp(big.NewInt(mint)) != 0 || got.Burn.Cmp(big.NewInt(burn)) != 0 {
			t.Errorf("%s: served totals @%d = mint=%s burn=%s; want mint=%d burn=%d",
				label, asOf, got.Mint, got.Burn, mint, burn)
		}
	}

	// ─── Fail-closed: no projector cursor yet → nothing is provably
	//     settled, so the pass folds nothing and the reader stays exact.
	insert(uncursored, 100, timescale.SEP41EventMint, 5, 90)
	insert(uncursored, 200, timescale.SEP41EventMint, 7, 91)
	advNoCursor, err := store.AdvanceSEP41SupplyRollup(ctx, uncursored)
	if err != nil {
		t.Fatalf("advance (no cursor): %v", err)
	}
	if advNoCursor.Advanced || advNoCursor.ToLedger != 0 {
		t.Errorf("advance with NO projector cursor = {Advanced:%v To:%d}; want {false 0} — nothing has provably settled",
			advNoCursor.Advanced, advNoCursor.ToLedger)
	}
	if !advNoCursor.CursorAbsent {
		t.Error("advance with NO projector cursor reported CursorAbsent=false; the pinned fold must be distinguishable from a steady-state no-op")
	}
	assertTotals("no-cursor reader", uncursored, 1000, 12, 0)

	// ─── Clean cycles: the projector has committed through ledger 999. ──
	insert(contractID, 900, timescale.SEP41EventMint, 700_000, 1)
	insert(contractID, 950, timescale.SEP41EventBurn, 100_000, 2)
	settleSEP41Cursor(t, ctx, store, 999)

	adv1, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if adv1.ToLedger != 900 {
		t.Errorf("advance 1 ToLedger = %d; want 900 (tip 950 deferred by the < max(ledger) guard)", adv1.ToLedger)
	}
	if adv1.CursorAbsent {
		t.Error("advance 1 reported CursorAbsent=true with the projector cursor committed through 999")
	}
	assertTotals("after clean fold", contractID, 5000, 700_000, 100_000)

	// ─── The fault. The mint at ledger 1000 fails its sink write with a
	//     transient fault, so it is NOT written; the projector keeps
	//     writing the rest of the window and holds its cursor at 999.
	insert(contractID, 1001, timescale.SEP41EventMint, 2_000_000, 3)
	insert(contractID, 1002, timescale.SEP41EventMint, 3_000_000, 4)
	insert(contractID, 1003, timescale.SEP41EventMint, 5_000_000, 5)

	// The rollup pass fires in that gap. It must stop BELOW the held
	// ledger — max(ledger) says 1003 is settled, the cursor says only
	// ledgers ≤ 999 are.
	adv2, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if adv2.ToLedger != 950 {
		t.Errorf("advance 2 ToLedger = %d; want 950 — the fold must not pass ledger 1000, which the projector is holding for retry (cursor=999)", adv2.ToLedger)
	}
	// Whatever the checkpoint, the reader is still whole at this point:
	// everything above it is covered by the live delta.
	assertTotals("during hold", contractID, 5000, 10_700_000, 100_000)

	// ─── The retry succeeds: the ledger-1000 mint lands, and the
	//     projector's next clean cycle commits through 1003.
	insert(contractID, 1000, timescale.SEP41EventMint, 1_000_000, 6)
	settleSEP41Cursor(t, ctx, store, 1003)

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance 3: %v", err)
	}

	// The money assertion: the retried mint is part of served supply.
	// Folding past it (the unsettled bound) loses exactly its 1,000,000.
	const wantMint = 700_000 + 1_000_000 + 2_000_000 + 3_000_000 + 5_000_000
	assertTotals("after retry landed", contractID, 5000, wantMint, 100_000)

	// Independent oracle: SEP41NetMintAtOrBefore never consults the
	// rollup, so the fast path and the authoritative full aggregate must
	// agree — the invariant the whole checkpoint design rests on.
	full, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 5000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	got := totals("oracle", contractID, 5000)
	net := new(big.Int).Sub(got.Mint, new(big.Int).Add(got.Burn, got.Clawback))
	if net.Cmp(full) != 0 {
		t.Errorf("rollup fast path net = %s, authoritative full sum = %s — the checkpoint dropped %s",
			net, full, new(big.Int).Sub(full, net))
	}
	if full.Cmp(big.NewInt(wantMint-100_000)) != 0 {
		t.Errorf("full-sum net mint = %s; want %d", full, wantMint-100_000)
	}
}

// sep41StoreAdapter projects *timescale.Store onto supply.SEP41SupplyStore
// (the supply package defines its own SEP41KindTotals to avoid a cyclic
// import). Mirrors cmd/stellarindex-aggregator's supplyAggregatorSEP41Store so
// the integration test exercises the exact production reader → computer path.
type sep41StoreAdapter struct{ s *timescale.Store }

func (a sep41StoreAdapter) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (supply.SEP41KindTotals, error) {
	t, err := a.s.SEP41KindTotalsAtOrBefore(ctx, contractID, asOfLedger)
	if err != nil {
		return supply.SEP41KindTotals{}, err
	}
	return supply.SEP41KindTotals{Mint: t.Mint, Burn: t.Burn, Clawback: t.Clawback}, nil
}

func (a sep41StoreAdapter) SACBalanceForContractAtOrBefore(ctx context.Context, holder, assetKey string, asOfLedger uint32) (*big.Int, error) {
	return a.s.SACBalanceForContractAtOrBefore(ctx, holder, assetKey, asOfLedger)
}

func (a sep41StoreAdapter) TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error) {
	return a.s.TrustlineBalanceForAccountAtOrBefore(ctx, accountID, assetKey, asOfLedger)
}

func (a sep41StoreAdapter) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	return a.s.MinSEP41ComponentLedger(ctx, contractID, asOfLedger)
}

func (a sep41StoreAdapter) SEP41GenesisBaselineSeeded(ctx context.Context, contractID string) (bool, error) {
	return a.s.SEP41GenesisBaselineSeeded(ctx, contractID)
}

// TestSEP41SupplyEventsRoundTrip exercises the
// InsertSEP41SupplyEvent → SEP41NetMintAtOrBefore →
// SEP41KindTotalsAtOrBefore paths through real TimescaleDB.
// Per ADR-0023 + ADR-0011 Algorithm 3, the running net mint
// (mint - burn - clawback) IS the SEP-41 total supply; if the
// SQL CASE-WHEN sign-flip or DISTINCT ON / FILTER aggregations
// regress, total supply silently goes wrong. The decoder unit
// tests cover defensive guards but can't validate the SQL — this
// test does.
func TestSEP41SupplyEventsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC" // synthetic
	const otherContract = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// ─── Empty state: net mint = 0; kind totals all zero ─────────
	got, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 1)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (empty): %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("empty net mint = %s, want 0", got)
	}
	totals, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, 1)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore (empty): %v", err)
	}
	if totals.Mint.Sign() != 0 || totals.Burn.Sign() != 0 || totals.Clawback.Sign() != 0 {
		t.Errorf("empty totals: mint=%s burn=%s clawback=%s, want all 0",
			totals.Mint, totals.Burn, totals.Clawback)
	}

	// ─── Insert a mint event at ledger 1000 ──────────────────────
	mintEvent := timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       1000,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000001",
		OpIndex:      0,
		ObservedAt:   t0,
		Kind:         timescale.SEP41EventMint,
		Amount:       big.NewInt(1_000_000),
		Counterparty: "GA1",
	}
	if err := store.InsertSEP41SupplyEvent(ctx, mintEvent); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (mint): %v", err)
	}

	// Idempotent re-insert — same PK is a no-op.
	if err := store.InsertSEP41SupplyEvent(ctx, mintEvent); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (mint dup): %v", err)
	}

	// ─── Insert a burn at ledger 2000 ────────────────────────────
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       2000,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000002",
		OpIndex:      0,
		ObservedAt:   t0.Add(time.Hour),
		Kind:         timescale.SEP41EventBurn,
		Amount:       big.NewInt(300_000),
		Counterparty: "GA1",
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (burn): %v", err)
	}

	// ─── Insert a clawback at ledger 2500 ────────────────────────
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID:   contractID,
		Ledger:       2500,
		TxHash:       "1100000000000000000000000000000000000000000000000000000000000003",
		OpIndex:      0,
		ObservedAt:   t0.Add(2 * time.Hour),
		Kind:         timescale.SEP41EventClawback,
		Amount:       big.NewInt(100_000),
		Counterparty: "GA2",
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (clawback): %v", err)
	}

	// ─── Net mint = 1_000_000 − 300_000 − 100_000 = 600_000 ──────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 3000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	if got.Cmp(big.NewInt(600_000)) != 0 {
		t.Errorf("net mint at ledger 3000 = %s, want 600000", got)
	}

	// ─── Kind totals split out cleanly ───────────────────────────
	totals, err = store.SEP41KindTotalsAtOrBefore(ctx, contractID, 3000)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore: %v", err)
	}
	if totals.Mint.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("Mint = %s, want 1000000", totals.Mint)
	}
	if totals.Burn.Cmp(big.NewInt(300_000)) != 0 {
		t.Errorf("Burn = %s, want 300000", totals.Burn)
	}
	if totals.Clawback.Cmp(big.NewInt(100_000)) != 0 {
		t.Errorf("Clawback = %s, want 100000", totals.Clawback)
	}

	// ─── At-or-before ledger 1500: only the mint counts ──────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 1500)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (1500): %v", err)
	}
	if got.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Errorf("net mint at ledger 1500 = %s, want 1000000 (burn+clawback excluded)", got)
	}

	// ─── At-or-before ledger 2000: mint + burn ───────────────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, contractID, 2000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (2000): %v", err)
	}
	if got.Cmp(big.NewInt(700_000)) != 0 {
		t.Errorf("net mint at ledger 2000 = %s, want 700000 (1M − 300K, clawback at 2500 excluded)", got)
	}

	// ─── Other contract is isolated — its totals stay 0 ──────────
	got, err = store.SEP41NetMintAtOrBefore(ctx, otherContract, 5000)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore (otherContract): %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("isolated contract net mint = %s, want 0 — contract_id filter is broken",
			got)
	}
}

// TestSEP41SupplyEvents_LargeI128 verifies the SQL preserves
// values that exceed int64. SEP-41 amounts are i128 in the wire
// protocol; Algorithm 3's running sum must not silently truncate.
func TestSEP41SupplyEvents_LargeI128(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)

	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: contractID,
		Ledger:     1,
		TxHash:     "2200000000000000000000000000000000000000000000000000000000000001",
		OpIndex:    0,
		ObservedAt: time.Now().UTC(),
		Kind:       timescale.SEP41EventMint,
		Amount:     huge,
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent (huge): %v", err)
	}

	got, err := store.SEP41NetMintAtOrBefore(ctx, contractID, 100)
	if err != nil {
		t.Fatalf("SEP41NetMintAtOrBefore: %v", err)
	}
	if got.Cmp(huge) != 0 {
		t.Errorf("got %s, want %s — i128 / NUMERIC round-trip lost precision", got, huge)
	}
}

// TestSEP41SupplyRollup_AdvanceDeltaAndFallback exercises the
// migration-0085 rollup path end-to-end against real TimescaleDB
// (the 0085 incident). It pins that:
//
//   - the reader returns the FULL correct totals via the fallback
//     full-sum when no checkpoint exists yet;
//   - AdvanceSEP41SupplyRollup folds only SETTLED ledgers — the current
//     tip ledger is deferred (the `< max(ledger)` watermark guard) so a
//     mid-write ledger is never half-folded;
//   - after an advance the reader returns the SAME totals via
//     rollup ⊕ live delta as the full sum would (the core correctness
//     invariant the fast path relies on);
//   - a historical read below the checkpoint falls back to the full sum;
//   - re-advancing with nothing newly settled is a monotonic no-op.
func TestSEP41SupplyRollup_AdvanceDeltaAndFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	const otherContract = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(ledger uint32, kind timescale.SEP41EventKind, amount int64, at time.Time, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: at, Kind: kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s@%d: %v", kind, ledger, err)
		}
	}

	assertTotals := func(label string, asOf uint32, mint, burn, claw int64) {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, asOf)
		if err != nil {
			t.Fatalf("%s: SEP41KindTotalsAtOrBefore: %v", label, err)
		}
		if got.Mint.Cmp(big.NewInt(mint)) != 0 || got.Burn.Cmp(big.NewInt(burn)) != 0 || got.Clawback.Cmp(big.NewInt(claw)) != 0 {
			t.Errorf("%s @%d = mint=%s burn=%s clawback=%s; want %d/%d/%d",
				label, asOf, got.Mint, got.Burn, got.Clawback, mint, burn, claw)
		}
	}

	insert(1000, timescale.SEP41EventMint, 1_000_000, t0, 1)
	insert(2000, timescale.SEP41EventBurn, 300_000, t0.Add(time.Hour), 2)
	insert(2500, timescale.SEP41EventClawback, 100_000, t0.Add(2*time.Hour), 3)

	// ─── No checkpoint yet — fallback full-sum path ──────────────
	assertTotals("fallback", 3000, 1_000_000, 300_000, 100_000)

	// ─── First advance: tip (2500) deferred, last_ledger = 2000 ──
	adv, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if adv.ToLedger != 2000 {
		t.Errorf("advance 1 ToLedger = %d; want 2000 (tip 2500 deferred by the < max guard)", adv.ToLedger)
	}
	if !adv.Advanced {
		t.Errorf("advance 1 should report Advanced=true")
	}

	// ─── Reader now uses rollup(≤2000) + delta(2000,asOf] ─────────
	assertTotals("rollup+delta", 3000, 1_000_000, 300_000, 100_000)    // delta covers deferred 2500
	assertTotals("at-checkpoint", 2000, 1_000_000, 300_000, 0)         // empty delta, pure rollup
	assertTotals("historical-below-checkpoint", 1500, 1_000_000, 0, 0) // fallback full-sum ≤1500

	// ─── Idempotent re-advance: nothing new settled → no-op ──────
	adv2, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if adv2.Advanced || adv2.ToLedger != 2000 {
		t.Errorf("advance 2 should be a no-op at 2000; got Advanced=%v To=%d", adv2.Advanced, adv2.ToLedger)
	}

	// ─── New settled data: 2500 now settles, 3000 becomes the tip ─
	insert(3000, timescale.SEP41EventMint, 500_000, t0.Add(3*time.Hour), 4)
	adv3, err := store.AdvanceSEP41SupplyRollup(ctx, contractID)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if adv3.ToLedger != 2500 {
		t.Errorf("advance 3 ToLedger = %d; want 2500 (tip 3000 deferred)", adv3.ToLedger)
	}
	assertTotals("rollup+delta-after-3", 4000, 1_500_000, 300_000, 100_000)

	// ─── Isolation: a different contract stays zero + advances clean
	oth, err := store.SEP41KindTotalsAtOrBefore(ctx, otherContract, 5000)
	if err != nil {
		t.Fatalf("other contract read: %v", err)
	}
	if oth.Mint.Sign() != 0 || oth.Burn.Sign() != 0 || oth.Clawback.Sign() != 0 {
		t.Errorf("other contract totals nonzero: mint=%s burn=%s clawback=%s", oth.Mint, oth.Burn, oth.Clawback)
	}
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, otherContract); err != nil {
		t.Fatalf("advance eventless contract: %v", err)
	}
}

// TestSEP41GenesisBaseline_LifetimeSupplyEndToEnd proves the migration-0088
// fix through the REAL store → reader → computer path against TimescaleDB
// (the 0088 incident):
//
//   - A SAC-wrapper with pre-Soroban issuance (seeded genesis mint) + a large
//     Soroban-era burn computes a POSITIVE LIFETIME total and does NOT trip the
//     negative-total guard — the failure mode for VELO/AQUA/yXLM/… .
//   - A Soroban-only token (no pre-genesis flows) is UNCHANGED whether or not a
//     baseline row exists, and seeding a ZERO baseline does not double-count.
//   - The baseline is gated on asOfLedger ≥ genesis_baseline_ledger, so a read
//     below the boundary omits it (aggregator always reads at tip).
//   - The genesis-seeded flag flips false → true across the seed.
func TestSEP41GenesisBaseline_LifetimeSupplyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	// Real production reader → computer over the store.
	reader := supply.NewStorageSEP41SupplyReader(sep41StoreAdapter{s: store}, nil)
	computer, err := supply.NewSEP41Computer(supply.Policy{}, reader)
	if err != nil {
		t.Fatalf("NewSEP41Computer: %v", err)
	}

	// Known-valid C-strkeys (canonical validates strkey CRC on construction).
	const (
		sacContract     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // pubnet native-XLM SAC
		sorobanContract = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	)
	const boundary = 50457424 // clickhouse.SorobanGenesisLedger
	const tip = 62000000
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	computeTotal := func(contractID string, asOf uint32) (*big.Int, error) {
		asset, aerr := canonical.NewSorobanAsset(contractID)
		if aerr != nil {
			t.Fatalf("NewSorobanAsset(%s): %v", contractID, aerr)
		}
		snap, cerr := computer.Compute(ctx, asset, asOf, t0)
		if cerr != nil {
			return nil, cerr
		}
		return snap.TotalSupply, nil
	}

	// ─── SAC-wrapper: Soroban-era burn dwarfs Soroban-era mint ───────────
	// Mimics VELO — nearly all issuance predates Soroban, so the Soroban-era
	// window alone reads Σburn ≫ Σmint.
	sorobanMint := big.NewInt(2) // negligible Soroban-era mint
	sorobanBurn, _ := new(big.Int).SetString("2180000000000", 10)
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sacContract, Ledger: 60000000, TxHash: txh(1), OpIndex: 0,
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: sorobanMint,
	}); err != nil {
		t.Fatalf("insert sac soroban mint: %v", err)
	}
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sacContract, Ledger: 60000001, TxHash: txh(2), OpIndex: 0,
		ObservedAt: t0.Add(time.Hour), Kind: timescale.SEP41EventBurn, Amount: sorobanBurn,
	}); err != nil {
		t.Fatalf("insert sac soroban burn: %v", err)
	}

	// No baseline seeded yet → negative total → benign
	// missing-baseline sentinel (NOT a paging compute_error).
	if seeded, err := store.SEP41GenesisBaselineSeeded(ctx, sacContract); err != nil || seeded {
		t.Fatalf("pre-seed SEP41GenesisBaselineSeeded = %v, %v; want false, nil", seeded, err)
	}
	if _, err := computeTotal(sacContract, tip); !errors.Is(err, supply.ErrNegativeTotalMissingBaseline) {
		t.Fatalf("pre-seed compute err = %v; want ErrNegativeTotalMissingBaseline", err)
	}

	// Seed the pre-Soroban opening balance (lifetime mint lived below the
	// boundary — synthesized from the CH lake in production).
	genesisMint, _ := new(big.Int).SetString("2400000000000", 10)
	if err := store.UpsertSEP41GenesisBaseline(ctx, sacContract,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("UpsertSEP41GenesisBaseline (sac): %v", err)
	}
	if seeded, err := store.SEP41GenesisBaselineSeeded(ctx, sacContract); err != nil || !seeded {
		t.Fatalf("post-seed SEP41GenesisBaselineSeeded = %v, %v; want true, nil", seeded, err)
	}

	// With the guard: lifetime total = (2 + 2.4e12) − 2.18e12 = 220000000002, positive,
	// guard not tripped.
	got, err := computeTotal(sacContract, tip)
	if err != nil {
		t.Fatalf("post-seed compute (sac): %v", err)
	}
	wantSac := new(big.Int).Sub(new(big.Int).Add(sorobanMint, genesisMint), sorobanBurn)
	if got.Cmp(wantSac) != 0 {
		t.Errorf("sac lifetime total = %s, want %s (positive, incl. pre-Soroban baseline)", got, wantSac)
	}
	if got.Sign() <= 0 {
		t.Errorf("sac lifetime total = %s, want > 0", got)
	}

	// Baseline gate: a read BELOW the boundary omits the genesis baseline (the
	// Soroban-era events are also above it, so the answer is 0 there).
	belowTotals, err := store.SEP41KindTotalsAtOrBefore(ctx, sacContract, boundary-1)
	if err != nil {
		t.Fatalf("kind totals below boundary: %v", err)
	}
	if belowTotals.Mint.Sign() != 0 || belowTotals.Burn.Sign() != 0 {
		t.Errorf("below-boundary totals = mint=%s burn=%s; want 0/0 (genesis not added below boundary)",
			belowTotals.Mint, belowTotals.Burn)
	}

	// Idempotent re-seed does not double-count.
	if err := store.UpsertSEP41GenesisBaseline(ctx, sacContract,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("re-seed (sac): %v", err)
	}
	if got2, err := computeTotal(sacContract, tip); err != nil || got2.Cmp(wantSac) != 0 {
		t.Errorf("re-seed changed total: got %s, %v; want %s (idempotent SET, no double-count)", got2, err, wantSac)
	}

	// The rollup worker + a seeded baseline coexist on the same row: advancing
	// the Soroban-era checkpoint must not disturb the genesis columns.
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, sacContract); err != nil {
		t.Fatalf("advance after seed: %v", err)
	}
	if got3, err := computeTotal(sacContract, tip); err != nil || got3.Cmp(wantSac) != 0 {
		t.Errorf("advance disturbed total: got %s, %v; want %s", got3, err, wantSac)
	}

	// ─── Soroban-only token: already correct, must stay UNCHANGED ────────
	sorOnlyMint := big.NewInt(1_000_000_000)
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: sorobanContract, Ledger: 60000000, TxHash: txh(10), OpIndex: 0,
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: sorOnlyMint,
	}); err != nil {
		t.Fatalf("insert soroban-only mint: %v", err)
	}
	// Unseeded: total == Soroban-era mint.
	if got, err := computeTotal(sorobanContract, tip); err != nil || got.Cmp(sorOnlyMint) != 0 {
		t.Errorf("soroban-only unseeded total = %s, %v; want %s (unchanged)", got, err, sorOnlyMint)
	}
	// Seeding a ZERO baseline (the production seed for a token with no
	// pre-genesis flows) leaves the served number identical — no double-count.
	if err := store.UpsertSEP41GenesisBaseline(ctx, sorobanContract,
		timescale.SEP41KindTotals{Mint: big.NewInt(0), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("zero-seed (soroban-only): %v", err)
	}
	if got, err := computeTotal(sorobanContract, tip); err != nil || got.Cmp(sorOnlyMint) != 0 {
		t.Errorf("soroban-only zero-seeded total = %s, %v; want %s (still unchanged — no double-count)", got, err, sorOnlyMint)
	}
}

// TestSEP41GenesisBaseline_BoundaryPartitionDisjoint pins the EXACT ledger seam
// between the two supply slices the migration-0088 reader stitches together: the
// seeded pre-Soroban genesis baseline (ledger < boundary, from the CH lake) and
// the Soroban-era totals (ledger >= boundary, from PG). The invariant is that
// they are a DISJOINT partition — every ledger belongs to exactly one — so
// folding them cannot double-count.
//
// It places a Soroban-era mint at EXACTLY the boundary ledger and asserts:
//
//   - a read one ledger BELOW the boundary sees NEITHER slice (genesis gated off
//     by asOf < genesis_baseline_ledger; the boundary event is above asOf) → 0;
//   - a read AT the boundary sees the genesis baseline PLUS the boundary event,
//     each counted exactly once (genesis + soroban), never twice;
//   - a read at the chain tip is identical.
//
// The `< boundary` (genesis) vs `>= boundary` (Soroban observer / the reader's
// `asOf >= genesis_baseline_ledger` gate) seam is what makes the fold sound; the
// operator-facing half of the same invariant — refusing a seed boundary above
// the true Soroban genesis — is enforced by validateGenesisLedgerBoundary in
// cmd/stellarindex-ops (its own unit test).
func TestSEP41GenesisBaseline_BoundaryPartitionDisjoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const boundary uint32 = 50457424 // clickhouse.SorobanGenesisLedger
	const tip uint32 = 62000000
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	genesisMint := big.NewInt(900)  // pre-Soroban opening balance (ledger < boundary)
	boundaryMint := big.NewInt(100) // a Soroban-era mint at EXACTLY the boundary ledger

	// Soroban-era event at ledger == boundary — the seam case: it must land in
	// the PG (Soroban) slice, never in the CH (genesis) slice.
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: contractID, Ledger: boundary, OpIndex: 0,
		TxHash:     fmt.Sprintf("%064x", 1),
		ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: boundaryMint,
	}); err != nil {
		t.Fatalf("insert boundary mint: %v", err)
	}

	// Seed the pre-Soroban genesis baseline with the boundary as its exclusive
	// upper bound (production passes clickhouse.SorobanGenesisLedger).
	if err := store.UpsertSEP41GenesisBaseline(ctx, contractID,
		timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("UpsertSEP41GenesisBaseline: %v", err)
	}

	mintAt := func(asOf uint32) *big.Int {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, asOf)
		if err != nil {
			t.Fatalf("SEP41KindTotalsAtOrBefore @%d: %v", asOf, err)
		}
		return got.Mint
	}

	// Below the boundary: genesis gated off, boundary event above asOf → 0.
	if got := mintAt(boundary - 1); got.Sign() != 0 {
		t.Errorf("mint @boundary-1 = %s, want 0 (neither slice contributes below the seam)", got)
	}
	// At the boundary: genesis (900) + the boundary event (100), each once.
	wantLifetime := new(big.Int).Add(genesisMint, boundaryMint) // 1000
	if got := mintAt(boundary); got.Cmp(wantLifetime) != 0 {
		t.Errorf("mint @boundary = %s, want %s (genesis + boundary event, each counted once)", got, wantLifetime)
	}
	// At tip: identical — no further events, and the fold does not re-add.
	if got := mintAt(tip); got.Cmp(wantLifetime) != 0 {
		t.Errorf("mint @tip = %s, want %s (disjoint fold, no double-count)", got, wantLifetime)
	}
}

// TestSEP41SupplyRollupFoldReset proves the re-derive
// footgun fix: ResetSEP41SupplyRollupFold (which `ch-rebuild -sep41 -write`
// calls automatically) rebuilds the worker-owned fold columns from zero over
// a re-derived history, in place, WITHOUT wiping the migration-0088 genesis
// baseline. It exercises both variants the ch-rebuild wiring drives:
//
//   - SCOPED reset (-contracts) re-folds ONLY the listed contract's fold row,
//     leaving other watched contracts' checkpoints intact;
//   - FULL reset (nil scope) re-folds EVERY contract's fold row (the
//     whole-table TRUNCATE-equivalent);
//   - neither leaves a row at last_ledger = 0 (the reader's unbounded
//     full-sum path) — the row goes straight from the old fold to the new;
//   - both PRESERVE the seeded genesis columns (genesis_mint_total +
//     genesis_baseline_ledger), asserted directly against the row;
//   - and the reset folds a below-checkpoint recovery the incremental worker
//     would otherwise never see (the served-undercount half of the bug that a
//     bare re-derive leaves behind), which a later advance keeps.
func TestSEP41SupplyRollupFoldReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	// Raw connection for DIRECT column assertions — the exported reader hides
	// last_ledger and the fold-vs-genesis column split, so this is the literal
	// proof that the reset zeroes the fold columns and spares the genesis ones.
	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contractA = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		contractB = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"

		boundary  uint32 = 50457424 // clickhouse.SorobanGenesisLedger
		lSettled1 uint32 = 50457500 // folded into the checkpoint
		lSettled2 uint32 = 50458000 // folded into the checkpoint (becomes last_ledger)
		lRecover  uint32 = 50457600 // a below-checkpoint recovery row (< last_ledger)
		lTip      uint32 = 50460000 // deferred by the < max(ledger) watermark guard
		readAt    uint32 = 50460000 // >= boundary, so the genesis baseline is added
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	insert := func(c string, ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: c, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: t0, Kind: kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s %s@%d: %v", c, kind, ledger, err)
		}
	}

	type rollupRow struct {
		mint, genesisMint string
		lastLedger        int64
		genesisSeeded     bool
	}
	readRow := func(c string) rollupRow {
		t.Helper()
		var r rollupRow
		var genesisLedger sql.NullInt64
		if err := rawdb.QueryRowContext(ctx, `
			SELECT mint_total::text, last_ledger, genesis_mint_total::text, genesis_baseline_ledger
			  FROM sep41_supply_rollup WHERE contract_id = $1`, c).
			Scan(&r.mint, &r.lastLedger, &r.genesisMint, &genesisLedger); err != nil {
			t.Fatalf("read rollup %s: %v", c, err)
		}
		r.genesisSeeded = genesisLedger.Valid
		return r
	}
	mintAt := func(c string, asOf uint32) int64 {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, c, asOf)
		if err != nil {
			t.Fatalf("SEP41KindTotalsAtOrBefore %s@%d: %v", c, asOf, err)
		}
		return got.Mint.Int64()
	}

	// ─── Seed both contracts: settled history + a seeded genesis baseline ─
	genesisMint := big.NewInt(4_000_000)
	for _, c := range []string{contractA, contractB} {
		insert(c, lSettled1, timescale.SEP41EventMint, 1_000_000, 1)
		insert(c, lSettled2, timescale.SEP41EventMint, 500_000, 2)
		insert(c, lTip, timescale.SEP41EventMint, 1, 3) // tip — deferred by the < max guard
		if err := store.UpsertSEP41GenesisBaseline(ctx, c,
			timescale.SEP41KindTotals{Mint: genesisMint, Burn: big.NewInt(0), Clawback: big.NewInt(0)},
			boundary); err != nil {
			t.Fatalf("seed genesis %s: %v", c, err)
		}
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance %s: %v", c, err)
		}
	}
	for _, c := range []string{contractA, contractB} {
		if r := readRow(c); r.mint != "1500000" || r.lastLedger != int64(lSettled2) {
			t.Fatalf("%s pre-reset fold = mint %s last_ledger %d; want 1500000 / %d", c, r.mint, r.lastLedger, lSettled2)
		}
	}

	// ─── Recover a below-checkpoint row on BOTH (the re-derive's effect) ──
	// A missing mint at ledger lRecover (< last_ledger). The worker's
	// `> last_ledger` fold can never see it, so a bare re-advance UNDERCOUNTS.
	for _, c := range []string{contractA, contractB} {
		insert(c, lRecover, timescale.SEP41EventMint, 700_000, 4)
	}
	// Lifetime mint = genesis(4M) + 1M + 0.5M + tip(1) [+ recovered 0.7M when folded].
	const wantUndercount = 5_500_001 // recovered row invisible to the incremental worker
	const wantFixed = 6_200_001      // recovered row folded after a reset
	const wantRefoldMint = "2200000" // settled 1M + 0.5M + recovered 0.7M; tip still deferred
	for _, c := range []string{contractA, contractB} {
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance-no-reset %s: %v", c, err)
		}
		if got := mintAt(c, readAt); got != wantUndercount {
			t.Fatalf("%s without reset = %d; want %d (below-checkpoint row invisible to the worker)", c, got, wantUndercount)
		}
	}

	// ─── SCOPED reset: only contractA ─────────────────────────────────────
	n, err := store.ResetSEP41SupplyRollupFold(ctx, []string{contractA})
	if err != nil {
		t.Fatalf("scoped reset: %v", err)
	}
	if n != 1 {
		t.Errorf("scoped reset touched %d rows; want 1 (only contractA)", n)
	}
	// The reset re-folds in place: the row is never left at last_ledger = 0,
	// and the recovered row is served before any aggregator pass.
	ra := readRow(contractA)
	if ra.mint != wantRefoldMint || ra.lastLedger != int64(lSettled2) {
		t.Errorf("A post-scoped-reset fold = mint %s last_ledger %d; want %s / %d (re-folded in place)", ra.mint, ra.lastLedger, wantRefoldMint, lSettled2)
	}
	if got := mintAt(contractA, readAt); got != wantFixed {
		t.Errorf("A right after scoped reset = %d; want %d (recovered row folded by the reset itself)", got, wantFixed)
	}
	if ra.genesisMint != "4000000" || !ra.genesisSeeded {
		t.Errorf("A post-scoped-reset genesis = %s seeded %v; want 4000000 / true (baseline must survive the reset)", ra.genesisMint, ra.genesisSeeded)
	}
	if rb := readRow(contractB); rb.lastLedger != int64(lSettled2) {
		t.Errorf("B fold last_ledger = %d; want %d (scoped reset must not touch B)", rb.lastLedger, lSettled2)
	}

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractA); err != nil {
		t.Fatalf("re-advance A: %v", err)
	}
	if got := mintAt(contractA, readAt); got != wantFixed {
		t.Errorf("A after scoped reset+advance = %d; want %d (recovered row now folded)", got, wantFixed)
	}
	if got := mintAt(contractB, readAt); got != wantUndercount {
		t.Errorf("B still = %d; want %d (unaffected by the A-scoped reset)", got, wantUndercount)
	}

	// ─── FULL reset: nil scope zeroes EVERY remaining folded row ──────────
	n, err = store.ResetSEP41SupplyRollupFold(ctx, nil)
	if err != nil {
		t.Fatalf("full reset: %v", err)
	}
	if n != 2 {
		t.Errorf("full reset touched %d rows; want 2 (all watched contracts)", n)
	}
	for _, c := range []string{contractA, contractB} {
		r := readRow(c)
		if r.mint != wantRefoldMint || r.lastLedger != int64(lSettled2) {
			t.Errorf("%s post-full-reset fold = mint %s last_ledger %d; want %s / %d (re-folded in place)", c, r.mint, r.lastLedger, wantRefoldMint, lSettled2)
		}
		if got := mintAt(c, readAt); got != wantFixed {
			t.Errorf("%s right after full reset = %d; want %d", c, got, wantFixed)
		}
		if r.genesisMint != "4000000" || !r.genesisSeeded {
			t.Errorf("%s post-full-reset genesis = %s seeded %v; want 4000000 / true (baseline preserved)", c, r.genesisMint, r.genesisSeeded)
		}
	}
	for _, c := range []string{contractA, contractB} {
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("final advance %s: %v", c, err)
		}
		if got := mintAt(c, readAt); got != wantFixed {
			t.Errorf("%s after full reset+advance = %d; want %d", c, got, wantFixed)
		}
	}
}

// TestSEP41GenesisBaseline_SeedAfterUnflooredFold proves the
// double-count defect is gone: seeding the pre-Soroban baseline AFTER the rollup
// worker has already folded pre-boundary rows must not double-count that band
// into served lifetime supply.
//
// The production ordering that arms it: an operator adds a SAC wrapper to
// `[supply] watched_sep41_contracts`; the aggregator's rollup worker folds it
// on the next pass (and immediately on start) with NO baseline seeded, so
// sep41SorobanFloor is 0 and the fold sweeps the CAP-67-replayed pre-Soroban
// rows into mint_total and moves last_ledger past them. `stellarindex-ops
// supply seed-sep41-genesis -write` — the runbook's documented remedy for the
// `missing_baseline` outcome — then adds the SAME band a second time as the
// genesis baseline.
//
// Three legs, each asserting the exact corrected lifetime total (not merely
// "non-nil"):
//
//   - first seed onto an unfloored fold rebuilds the fold under the new floor
//     in the same transaction, so the served read stays on the checkpoint fast
//     path (last_ledger at the settled tip, never 0) and counts each row ONCE;
//   - a REPEAT seed of the SAME boundary converges on the same fold (the
//     "idempotent" the runbook promises is preserved, not traded away);
//   - a CORRECTING seed that MOVES the boundary re-zeroes the fold, so the
//     re-partitioned genesis/Soroban slices still conserve the same total.
func TestSEP41GenesisBaseline_SeedAfterUnflooredFold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	// Raw connection for DIRECT fold-column assertions — the exported reader
	// hides last_ledger and the fold-vs-genesis column split.
	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

		boundary uint32 = 50457424 // clickhouse.SorobanGenesisLedger
		lPre     uint32 = 50456424 // CAP-67-replayed classic mint, BELOW the boundary
		lSeam    uint32 = 50457424 // Soroban-era mint exactly AT the boundary
		lTip     uint32 = 50557424 // deferred by the < max(ledger) settled guard
		readAt   uint32 = 60000000

		preMint  int64 = 900 // the pre-Soroban band — present in PG *and* in the CH lake
		seamMint int64 = 100
		tipMint  int64 = 1

		// Lifetime = pre(900) + seam(100) + tip(1), each counted exactly once.
		wantLifetime int64 = 1001
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	insert := func(ledger uint32, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ledger, TxHash: fmt.Sprintf("%064x", tx), OpIndex: 0,
			ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert mint@%d: %v", ledger, err)
		}
	}
	seed := func(genesisMint int64, baseline uint32) {
		t.Helper()
		if err := store.UpsertSEP41GenesisBaseline(ctx, contractID,
			timescale.SEP41KindTotals{Mint: big.NewInt(genesisMint), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
			baseline); err != nil {
			t.Fatalf("UpsertSEP41GenesisBaseline(%d, %d): %v", genesisMint, baseline, err)
		}
	}
	type foldRow struct {
		mint, genesisMint string
		lastLedger        int64
		genesisLedger     sql.NullInt64
	}
	readFold := func() foldRow {
		t.Helper()
		var r foldRow
		if err := rawdb.QueryRowContext(ctx, `
			SELECT mint_total::text, last_ledger, genesis_mint_total::text, genesis_baseline_ledger
			  FROM sep41_supply_rollup WHERE contract_id = $1`, contractID).
			Scan(&r.mint, &r.lastLedger, &r.genesisMint, &r.genesisLedger); err != nil {
			t.Fatalf("read rollup row: %v", err)
		}
		return r
	}
	lifetimeMint := func() int64 {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, readAt)
		if err != nil {
			t.Fatalf("SEP41KindTotalsAtOrBefore @%d: %v", readAt, err)
		}
		return got.Mint.Int64()
	}

	// ─── The arming order: events, then a fold with NO baseline (floor 0) ──
	insert(lPre, preMint, 1)
	insert(lSeam, seamMint, 2)
	insert(lTip, tipMint, 3)
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance (unseeded): %v", err)
	}
	// Precondition: the fold really did sweep the pre-boundary row in. If this
	// ever stops holding the rest of the test proves nothing, so it is fatal.
	if r := readFold(); r.mint != "1000" || r.lastLedger != int64(lSeam) {
		t.Fatalf("unfloored fold = mint %s last_ledger %d; want 1000 / %d (pre-boundary row swept in at floor 0)",
			r.mint, r.lastLedger, lSeam)
	}

	// ─── Leg 1: the remedy seed must not re-add the band it already holds ──
	seed(preMint, boundary)
	r := readFold()
	if r.mint != "100" || r.lastLedger != int64(lSeam) {
		t.Errorf("post-seed fold = mint %s last_ledger %d; want 100 / %d (the seed re-folds under the new floor, excluding the pre-boundary row, and leaves the checkpoint at the settled tip rather than 0)",
			r.mint, r.lastLedger, lSeam)
	}
	if r.genesisMint != "900" || !r.genesisLedger.Valid || r.genesisLedger.Int64 != int64(boundary) {
		t.Errorf("post-seed genesis = mint %s ledger %v; want 900 / %d", r.genesisMint, r.genesisLedger, boundary)
	}
	if got := lifetimeMint(); got != wantLifetime {
		t.Errorf("lifetime mint after seed-onto-unfloored-fold = %d; want %d (pre-Soroban band counted ONCE)", got, wantLifetime)
	}

	// ─── Leg 2: a repeat seed of the SAME boundary converges on the same fold
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance (post-seed re-fold): %v", err)
	}
	if r := readFold(); r.mint != "100" || r.lastLedger != int64(lSeam) {
		t.Fatalf("re-fold under the seeded floor = mint %s last_ledger %d; want 100 / %d (pre-boundary row now excluded)",
			r.mint, r.lastLedger, lSeam)
	}
	seed(preMint, boundary)
	if r := readFold(); r.mint != "100" || r.lastLedger != int64(lSeam) {
		t.Errorf("fold after a repeat seed of the same boundary = mint %s last_ledger %d; want 100 / %d (idempotent: the rebuilt fold is the same fold)",
			r.mint, r.lastLedger, lSeam)
	}
	if got := lifetimeMint(); got != wantLifetime {
		t.Errorf("lifetime mint after a repeat seed = %d; want %d", got, wantLifetime)
	}

	// ─── Leg 3: a CORRECTING seed that moves the boundary re-folds ─────────
	// The operator re-runs with a boundary above the seam, so the CH-side
	// genesis legitimately grows to pre+seam and the Soroban slice shrinks to
	// the tip. Conservation must hold: same lifetime total, different split.
	const boundary2 = lSeam + 1
	seed(preMint+seamMint, boundary2)
	if r := readFold(); r.mint != "0" || r.lastLedger != 0 {
		t.Errorf("fold after a boundary-moving re-seed = mint %s last_ledger %d; want 0 / 0", r.mint, r.lastLedger)
	}
	if got := lifetimeMint(); got != wantLifetime {
		t.Errorf("lifetime mint after a boundary-moving re-seed = %d; want %d (conserved across the re-partition)", got, wantLifetime)
	}
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance (post-correction re-fold): %v", err)
	}
	if got := lifetimeMint(); got != wantLifetime {
		t.Errorf("lifetime mint after the corrected re-fold = %d; want %d", got, wantLifetime)
	}
}

// TestSEP41GenesisBaseline_ReseedRepairsFoldPoisonedUnderSameFloor pins the
// half a floor-move trigger cannot reach: a row that was ALREADY
// seeded while its fold still held the pre-boundary band (written before the
// seed touched the fold at all).
// Re-running the seed with the same boundary is the documented remedy, and it
// must repair the row even though the floor does not move.
func TestSEP41GenesisBaseline_ReseedRepairsFoldPoisonedUnderSameFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)
	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contractID = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

		boundary uint32 = 50457424
		lPre     uint32 = 50456424 // CAP-67-replayed mint, BELOW the boundary
		lSeam    uint32 = 50457424
		lTip     uint32 = 50557424 // deferred by the < max(ledger) guard
		readAt   uint32 = 60000000

		preMint  int64 = 10_000_000_000
		seamMint int64 = 8_762_638_133
		tipMint  int64 = 1

		wantLifetime int64 = preMint + seamMint + tipMint
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	for i, ev := range []struct {
		ledger uint32
		amount int64
	}{{lPre, preMint}, {lSeam, seamMint}, {lTip, tipMint}} {
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ev.ledger, TxHash: fmt.Sprintf("%064x", i+1), OpIndex: 0,
			ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: big.NewInt(ev.amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert mint@%d: %v", ev.ledger, err)
		}
	}
	seed := func() {
		t.Helper()
		if err := store.UpsertSEP41GenesisBaseline(ctx, contractID,
			timescale.SEP41KindTotals{Mint: big.NewInt(preMint), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
			boundary); err != nil {
			t.Fatalf("UpsertSEP41GenesisBaseline: %v", err)
		}
	}

	// The already-poisoned production state: seeded at `boundary`, and a fold
	// that swept the pre-boundary row in at floor 0 before the seed existed.
	seed()
	if _, err := rawdb.ExecContext(ctx, `
		UPDATE sep41_supply_rollup
		   SET mint_total = $2, burn_total = 0, clawback_total = 0, last_ledger = $3
		 WHERE contract_id = $1`, contractID, fmt.Sprint(preMint+seamMint), int64(lSeam)); err != nil {
		t.Fatalf("arm poisoned fold: %v", err)
	}
	if got := lifetimeSEP41Mint(t, ctx, store, contractID, readAt); got != wantLifetime+preMint {
		t.Fatalf("precondition: poisoned lifetime mint = %d; want %d (pre-boundary band counted twice)", got, wantLifetime+preMint)
	}

	// The remedy: the same seed, same boundary.
	seed()
	var mint string
	var lastLedger int64
	if err := rawdb.QueryRowContext(ctx, `
		SELECT mint_total::text, last_ledger FROM sep41_supply_rollup WHERE contract_id = $1`, contractID).
		Scan(&mint, &lastLedger); err != nil {
		t.Fatalf("read rollup row: %v", err)
	}
	if mint != fmt.Sprint(seamMint) || lastLedger != int64(lSeam) {
		t.Errorf("fold after a same-boundary re-seed = mint %s last_ledger %d; want %d / %d (rebuilt under the seeded floor)",
			mint, lastLedger, seamMint, lSeam)
	}
	if got := lifetimeSEP41Mint(t, ctx, store, contractID, readAt); got != wantLifetime {
		t.Errorf("lifetime mint after a same-boundary re-seed = %d; want %d (pre-boundary band counted ONCE)", got, wantLifetime)
	}
}

// TestSEP41SupplyRollup_ContendedWritersYield pins the convoy: a fold
// writer blocked on another writer's row lock must give up after the bounded
// lock_timeout (SQLSTATE 55P03) rather than wait out a cold full-history fold,
// because the aggregator advances contracts sequentially and every later
// contract in the pass would wait behind it.
func TestSEP41SupplyRollup_ContendedWritersYield(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)
	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("materialise rollup row: %v", err)
	}

	// The holder: a writer mid-way through a long fold, holding the row.
	holder, err := rawdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx,
		`SELECT 1 FROM sep41_supply_rollup WHERE contract_id = $1 FOR UPDATE`, contractID); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	// Well past the 10s bound, well short of forever.
	const deadline = 45 * time.Second
	writers := map[string]func(context.Context) error{
		"advance": func(c context.Context) error {
			_, err := store.AdvanceSEP41SupplyRollup(c, contractID)
			return err
		},
		"seed": func(c context.Context) error {
			return store.UpsertSEP41GenesisBaseline(c, contractID,
				timescale.SEP41KindTotals{Mint: big.NewInt(1), Burn: big.NewInt(0), Clawback: big.NewInt(0)}, 50457424)
		},
	}
	for name, write := range writers {
		wctx, wcancel := context.WithTimeout(ctx, deadline)
		start := time.Now()
		err := write(wctx)
		wcancel()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("%s under a held row lock returned after %s with err=%v; want SQLSTATE 55P03 (lock_timeout), not a wait bounded only by the caller's context",
				name, time.Since(start).Round(time.Millisecond), err)
		}
	}
}

// TestSEP41SupplyRollup_ResetDuringAdvanceIsNotStranded proves the
// fold-reset race is gone: a fold reset that commits while
// the aggregator's rollup worker is mid-pass must not be stranded.
//
// Both writers of the fold's input boundary run against a LIVE aggregator:
// `ch-rebuild -sep41 -write` resets the fold after a re-derive, and
// `supply seed-sep41-genesis -write` resets it whenever the
// genesis floor moves. When the pass decided its own boundary in a round trip
// BEFORE the write, a reset landing in that gap was silently undone: the pass
// added its delta over (stale last_ledger, mx) on top of the freshly-zeroed
// totals and then pushed last_ledger back up, so every row at-or-below the
// stale checkpoint was excluded from the fold forever — a served UNDERCOUNT
// the next pass can never repair, and the exact failure the reset exists to
// prevent.
//
// The interleave is pinned, not raced: a blocker transaction holds the rollup
// row's write lock with the literal statement ResetSEP41SupplyRollupFold
// issues, the worker pass is started against it, the test WAITS (via
// pg_stat_activity) until that pass is genuinely blocked on the lock, and only
// then does the reset commit. If the pass never blocks the test fails rather
// than passing vacuously.
func TestSEP41SupplyRollup_ResetDuringAdvanceIsNotStranded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	rawdb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = rawdb.Close() })

	const (
		contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"

		boundary  uint32 = 50457424 // clickhouse.SorobanGenesisLedger
		lSettled1 uint32 = 50457500
		lSettled2 uint32 = 50458000 // becomes last_ledger
		lTip      uint32 = 50460000 // deferred by the < max(ledger) settled guard
		readAt    uint32 = 50460000

		genesisMint int64 = 4_000_000
		settled1    int64 = 1_000_000
		settled2    int64 = 500_000
		tipMint     int64 = 1

		// genesis(4M) + settled(1M + 0.5M) + tip(1), each counted once.
		wantLifetime int64 = 5_500_001
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	insert := func(ledger uint32, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ledger, TxHash: fmt.Sprintf("%064x", tx), OpIndex: 0,
			ObservedAt: t0, Kind: timescale.SEP41EventMint, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert mint@%d: %v", ledger, err)
		}
	}

	insert(lSettled1, settled1, 1)
	insert(lSettled2, settled2, 2)
	insert(lTip, tipMint, 3)
	if err := store.UpsertSEP41GenesisBaseline(ctx, contractID,
		timescale.SEP41KindTotals{Mint: big.NewInt(genesisMint), Burn: big.NewInt(0), Clawback: big.NewInt(0)},
		boundary); err != nil {
		t.Fatalf("seed genesis: %v", err)
	}
	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("first advance: %v", err)
	}
	if got := lifetimeSEP41Mint(t, ctx, store, contractID, readAt); got != wantLifetime {
		t.Fatalf("baseline lifetime mint = %d; want %d", got, wantLifetime)
	}

	// ─── Blocker: the scoped reset, held open on its own connection ───────
	blockerConn, err := rawdb.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker conn: %v", err)
	}
	defer func() { _ = blockerConn.Close() }()
	blockerTx, err := blockerConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = blockerTx.Rollback()
		}
	}()
	// Byte-for-byte the scoped statement ResetSEP41SupplyRollupFold issues.
	if _, err := blockerTx.ExecContext(ctx, `
            UPDATE sep41_supply_rollup
               SET mint_total = 0, burn_total = 0, clawback_total = 0,
                   last_ledger = 0, updated_at = now()
             WHERE contract_id = ANY($1)
    `, []string{contractID}); err != nil {
		t.Fatalf("blocker reset: %v", err)
	}

	// ─── The worker pass, which must end up waiting on that row lock ──────
	type advResult struct {
		res timescale.SEP41RollupAdvance
		err error
	}
	done := make(chan advResult, 1)
	go func() {
		res, aerr := store.AdvanceSEP41SupplyRollup(ctx, contractID)
		done <- advResult{res, aerr}
	}()

	waitForLockWait := func() {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := rawdb.QueryRowContext(ctx, `
				SELECT count(*) FROM pg_stat_activity
				 WHERE datname = current_database()
				   AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
				t.Fatalf("pg_stat_activity: %v", err)
			}
			if n > 0 {
				return
			}
			select {
			case r := <-done:
				t.Fatalf("the rollup pass finished without ever waiting on the row lock (res=%+v err=%v) — the interleave never armed, so this test would prove nothing", r.res, r.err)
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatalf("rollup pass never blocked on the rollup row lock within 30s")
	}
	waitForLockWait()

	// The reset commits INSIDE the worker's pass.
	if err := blockerTx.Commit(); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}
	committed = true

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("advance across the reset: %v", r.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("rollup pass did not finish after the reset committed")
	}

	// ─── The reset must have been honoured, not stranded ──────────────────
	var mint string
	var lastLedger int64
	if err := rawdb.QueryRowContext(ctx, `
		SELECT mint_total::text, last_ledger
		  FROM sep41_supply_rollup WHERE contract_id = $1`, contractID).
		Scan(&mint, &lastLedger); err != nil {
		t.Fatalf("read rollup row: %v", err)
	}
	if mint != "1500000" || lastLedger != int64(lSettled2) {
		t.Errorf("fold after reset-during-advance = mint %s last_ledger %d; want 1500000 / %d (the pass must re-fold from the reset boundary, not resume from the stale one)",
			mint, lastLedger, lSettled2)
	}
	if got := lifetimeSEP41Mint(t, ctx, store, contractID, readAt); got != wantLifetime {
		t.Errorf("lifetime mint after a reset during an advance = %d; want %d (rows at-or-below the stale checkpoint must not be stranded)", got, wantLifetime)
	}
}

// lifetimeSEP41Mint reads the served Algorithm-3 mint component.
func lifetimeSEP41Mint(t *testing.T, ctx context.Context, store *timescale.Store, contractID string, asOf uint32) int64 {
	t.Helper()
	got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, asOf)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore %s@%d: %v", contractID, asOf, err)
	}
	return got.Mint.Int64()
}

// TestSEP41RollupCheckpoints_DerivedReconcile exercises the two storage
// seams the derived-checkpoint reconcile (`stellarindex-ops supply
// verify-rollup`) is built on — ListSEP41RollupCheckpoints (the
// checkpoint side) and SEP41SupplyEventKindResum (the same-source truth
// side) — against real TimescaleDB, then feeds them through
// completeness.ReconcileRunningTotals. It pins that:
//
//   - a healthy checkpoint (fold == Σ rows it folds, bounded at
//     last_ledger) reconciles CLEAN, and the re-sum is correctly bounded
//     at the checkpoint's own last_ledger (NOT the tip — the deferred
//     tip must not count as drift);
//   - the KALE 2× double-fold — a fold column
//     wrongly doubled below the checkpoint — is FLAGGED with Delta =
//     +truth, the exact signature the row-count reconciles miss;
//   - the -contracts scope filters ListSEP41RollupCheckpoints.
func TestSEP41RollupCheckpoints_DerivedReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	const (
		contractA = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		contractB = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
	)
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }
	insert := func(c string, ledger uint32, kind timescale.SEP41EventKind, amount int64, tx int) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: c, Ledger: ledger, TxHash: txh(tx), OpIndex: 0,
			ObservedAt: t0, Kind: kind, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert %s %s@%d: %v", c, kind, ledger, err)
		}
	}

	// Settled history + a deferred tip on each contract, then fold.
	insert(contractA, 1000, timescale.SEP41EventMint, 1_000_000, 1)
	insert(contractA, 2000, timescale.SEP41EventBurn, 200_000, 2)
	insert(contractA, 5000, timescale.SEP41EventMint, 9, 3) // deferred tip (< max guard)
	insert(contractB, 1000, timescale.SEP41EventMint, 42, 4)
	insert(contractB, 5000, timescale.SEP41EventMint, 1, 5) // deferred tip
	for _, c := range []string{contractA, contractB} {
		if _, err := store.AdvanceSEP41SupplyRollup(ctx, c); err != nil {
			t.Fatalf("advance %s: %v", c, err)
		}
	}

	// reconcile lists checkpoints, computes the same-source re-sum bounded
	// at each checkpoint's own last_ledger, and diffs them.
	reconcile := func(contractIDs []string) ([]completeness.TotalsDrift, int) {
		t.Helper()
		cps, err := store.ListSEP41RollupCheckpoints(ctx, contractIDs)
		if err != nil {
			t.Fatalf("ListSEP41RollupCheckpoints: %v", err)
		}
		cpMap := map[string]completeness.RunningTotals{}
		truthMap := map[string]completeness.RunningTotals{}
		for _, cp := range cps {
			cpMap[cp.ContractID] = completeness.RunningTotals{Mint: cp.Fold.Mint, Burn: cp.Fold.Burn, Clawback: cp.Fold.Clawback}
			resum, err := store.SEP41SupplyEventKindResum(ctx, cp.ContractID, cp.LastLedger, 2*time.Minute)
			if err != nil {
				t.Fatalf("SEP41SupplyEventKindResum %s@%d: %v", cp.ContractID, cp.LastLedger, err)
			}
			truthMap[cp.ContractID] = completeness.RunningTotals{Mint: resum.Mint, Burn: resum.Burn, Clawback: resum.Clawback}
		}
		return completeness.ReconcileRunningTotals(cpMap, truthMap, nil), len(cps)
	}

	// ─── Healthy: fold == bounded re-sum → clean ─────────────────────────
	if drifts, checked := reconcile(nil); len(drifts) != 0 || checked != 2 {
		t.Fatalf("healthy reconcile = %d drift(s) over %d checkpoint(s); want 0 over 2: %+v", len(drifts), checked, drifts)
	}

	// ─── Scope filter: -contracts restricts the checkpoint set ───────────
	if _, checked := reconcile([]string{contractB}); checked != 1 {
		t.Fatalf("scoped reconcile checked %d checkpoints; want 1 (contractB only)", checked)
	}

	// ─── KALE double-fold: wrongly double contractA's folded mint ────────
	// The raw rows stay correct (row-count reconciles pass); only the
	// derived checkpoint is doubled. Delta must equal +truth.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE sep41_supply_rollup SET mint_total = mint_total * 2 WHERE contract_id = $1`, contractA); err != nil {
		t.Fatalf("poison double-fold: %v", err)
	}
	drifts, _ := reconcile(nil)
	if len(drifts) != 1 {
		t.Fatalf("post-poison reconcile = %d drift(s); want exactly 1 (contractA mint): %+v", len(drifts), drifts)
	}
	d := drifts[0]
	if d.ContractID != contractA || d.Kind != "mint" {
		t.Fatalf("drift = %s/%s; want %s/mint", d.ContractID, d.Kind, contractA)
	}
	// contractA folded mint = 1_000_000 (the 5000 tip was deferred), doubled
	// to 2_000_000 ⇒ Delta = checkpoint − truth = +1_000_000.
	if d.Delta.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("Delta = %s; want +1000000 (the 2× over-count signature)", d.Delta)
	}
}

// TestSEP41SupplyRollup_LargeI128 verifies the rollup checkpoint + delta
// preserve values exceeding int64 — Σmint alone can exceed i128, so the
// running NUMERIC totals must never truncate (ADR-0003).
func TestSEP41SupplyRollup_LargeI128(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This test pins the tip-deferral guard, so declare the domain durably
	// ingested past every ledger it uses: AdvanceSEP41SupplyRollup folds no
	// further than the projector's cursor (see settleSEP41Cursor).
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	tail := big.NewInt(7)
	want := new(big.Int).Add(huge, tail)

	// huge at ledger 1 (folded into the checkpoint), tail at ledger 2
	// (the deferred tip, served from the live delta) — so the read sum
	// spans BOTH the rollup and the delta.
	for i, ev := range []struct {
		ledger uint32
		amt    *big.Int
	}{{1, huge}, {2, tail}} {
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ev.ledger,
			TxHash:  fmt.Sprintf("%064x", i+1),
			OpIndex: 0, ObservedAt: time.Now().UTC(),
			Kind: timescale.SEP41EventMint, Amount: ev.amt,
		}); err != nil {
			t.Fatalf("insert huge[%d]: %v", i, err)
		}
	}

	if _, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, 100)
	if err != nil {
		t.Fatalf("SEP41KindTotalsAtOrBefore: %v", err)
	}
	if got.Mint.Cmp(want) != 0 {
		t.Errorf("rollup+delta mint = %s, want %s — i128 truncated across the checkpoint boundary", got.Mint, want)
	}
}

// TestMinSEP41ComponentLedgerUsesObserverWatermark is the SEP-41 half of
// the freshness rule: the anchor must track the supply-event PRODUCER's
// progress, not one contract's last mint/burn.
//
// This bit harder than the classic case. 40 of 48 watched assets are
// C-address SEP-41 tokens that never touch the classic component tables, so
// this path froze the MAJORITY of served supply: with the producer watermark
// at 63,671,020, frozen contracts sat 46k-169k ledgers behind it, and the one
// asset still publishing had a last event landing exactly ON the
// watermark.
//
// Anchoring on event ledgers would be perverse in a way worth pinning: a
// contract with NO events would return 0 and skip the gate entirely, so it
// kept publishing — while a contract that merely went quiet froze. Both cases
// are asserted below.
func TestMinSEP41ComponentLedgerUsesObserverWatermark(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		quiet  = "CBEM2CAIYLM3HBOPU5HLQL7V5BUAKM3N77DYQKX4FNHTQLQUUD2ZFBOX"
		lively = "CB23WRDQWGSP6YPMY4UV5C4OW5CBTXKYN3XEATG7KJEZCXMJBYEHOUOV"
		silent = "CAFD2IS6FEBUXWHAOH3G5LM4LMXIHVH6LAYRHUPYUU62NXH3I4TUCI2C"
	)
	t0 := time.Date(2026, 7, 28, 8, 0, 0, 0, time.UTC)

	mint := func(contract string, ledger uint32, tx string) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID:   contract,
			Ledger:       ledger,
			TxHash:       tx,
			OpIndex:      0,
			ObservedAt:   t0,
			Kind:         timescale.SEP41EventMint,
			Amount:       big.NewInt(1_000_000),
			Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("InsertSEP41SupplyEvent %s@%d: %v", contract, ledger, err)
		}
	}

	// The producer has reached ledger 9000 (via the lively contract). The
	// quiet contract's own last event is far behind — normal for a token
	// whose supply simply hasn't moved.
	mint(quiet, 1000, "2200000000000000000000000000000000000000000000000000000000000001")
	mint(lively, 9000, "2200000000000000000000000000000000000000000000000000000000000002")

	got, err := store.MinSEP41ComponentLedger(ctx, quiet, 10_000)
	if err != nil {
		t.Fatalf("MinSEP41ComponentLedger(quiet): %v", err)
	}
	if got != 9000 {
		t.Errorf("quiet contract anchor = %d, want 9000 (producer watermark). "+
			"Got 1000 => regressed to per-contract last activity, which freezes "+
			"every SEP-41 token whose supply has not recently changed.", got)
	}

	// A contract with no events at all stays uninstrumented (0) so the caller
	// skips the gate — unchanged from before, and asserted so the fix cannot
	// silently start publishing a zero-valued supply behind a live anchor.
	got, err = store.MinSEP41ComponentLedger(ctx, silent, 10_000)
	if err != nil {
		t.Fatalf("MinSEP41ComponentLedger(silent): %v", err)
	}
	if got != 0 {
		t.Errorf("contract with no events anchor = %d, want 0 (gate skipped)", got)
	}

	// asOfLedger must still bound the watermark: replaying history must not
	// be handed a future producer position.
	got, err = store.MinSEP41ComponentLedger(ctx, quiet, 5_000)
	if err != nil {
		t.Fatalf("MinSEP41ComponentLedger(historical): %v", err)
	}
	if got != 1000 {
		t.Errorf("anchor at asOf=5000 = %d, want 1000 — the watermark must be "+
			"clamped to asOfLedger, not read at the live tip", got)
	}
}
