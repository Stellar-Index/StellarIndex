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

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSEP41SupplyRollup_ResetSeenButRewriteUnseenIsNotStranded pins the
// residual half of audit-2026-09-02 F108 that the row lock alone does not
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
