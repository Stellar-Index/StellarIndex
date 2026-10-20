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

// Commits the sep41_supply cursor through the projector's own pair of calls (GetCursor, CursorRead, AdvanceCursorFrom), a
// CAS so a replay rewind isn't reverted. Restating the ("projector","sep41_supply") pair pins nothing.
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

// Pins that the fold's `< max(ledger)` guard alone does not mean settled: on a transient sink fault the projector holds
// the row, writes the rest of the window, and caps only the cursor, so a fold bounded by max(ledger) skips the held ledger
// and its amount is permanently absent from served supply. Two connections, no race.
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
	// Non-vacuity: the pass must be parked taking the rollup row's lock; otherwise its snapshot predates the writes and the
	// interleave never armed.
	if !strings.Contains(parked, "FOR UPDATE") {
		t.Fatalf("the rollup pass is parked in %q, not in its row-locking statement — the interleave never armed", parked)
	}

	// Hold-and-advance while parked: the sink write for lHeld faults and is not written, later ledgers are written and
	// committed, and the cursor stays at lHeld-1.
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
	// max(ledger) says lAfter but the cursor says only ledgers <= cursorWhileHolding committed; the fold must believe the
	// cursor. Errorf, not Fatalf: report the over-folded checkpoint and the served undercount from one run.
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

// Failure path of the in-place reset: a contract whose re-fold can't take its row lock in time is zeroed (exact full-sum
// path) and reported as an error, while an uncontended contract in the same full reset is still re-folded.
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

// Pins the residual reset race the row lock alone doesn't close: the fold must not pair a post-reset boundary with a pre-
// rewrite view of events. Under READ COMMITTED a SELECT ... FOR UPDATE inside the folding statement sees the latest
// committed row (last_ledger = 0) but old events, producing an unrepairable undercount.
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
	// A locker-only hold is the pause instrument: the pass's INSERT ... ON CONFLICT DO NOTHING doesn't wait on it,
	// so the pass parks where it takes the row lock, the statement whose snapshot is under test.
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

// Blocks until a backend waits on a heavyweight lock (the pass parked behind the blocker) and returns the statement text.
// finished means the pass already returned: the interleave never armed and the test must fail, not pass vacuously.
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

// Marks the projection durable through `ledger` by writing the projector's cursor on the real ("projector", src.Name)
// pair. Uses UpsertCursor, not the projector's CAS AdvanceCursorFrom, because seeding a start has nothing to compare
// against.
func settleSEP41Cursor(t *testing.T, ctx context.Context, store *timescale.Store, ledger uint32) { //nolint:revive // t-first matches the file's other helpers (startTimescale).
	t.Helper()
	if err := store.UpsertCursor(ctx, "projector", sep41supply.SourceName, ledger); err != nil {
		t.Fatalf("UpsertCursor(projector, %s, %d): %v", sep41supply.SourceName, ledger, err)
	}
}

// Far above every ledger the rollup tests use, so `< max(ledger)` stays the binding bound and those tests keep pinning the
// tip-deferral guard.
const sep41SettledTestCursorLedger = 200_000_000

// AdvanceSEP41SupplyRollup must not fold past a ledger the projector holds for retry, or the retried row lands between the
// folded checkpoint and the reader's live delta and supply is silently under-counted. Also pins fail-closed: no cursor
// folds nothing and the reader stays exact.
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

// Projects *timescale.Store onto supply.SEP41SupplyStore (supply defines its own SEP41KindTotals to avoid an import
// cycle), mirroring supplyAggregatorSEP41Store so the test exercises the production reader-to-computer path.
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

// Round-trips the SEP-41 event insert and net-mint/kind-totals reads through real TimescaleDB: net mint (mint - burn -
// clawback) IS total supply (ADR-0023/0011), and decoder unit tests can't validate the CASE-WHEN sign flip or DISTINCT ON
// / FILTER aggregations.
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

// Rollup path end-to-end: full-sum fallback with no checkpoint; advance folds only settled ledgers (tip deferred); after
// advance rollup + live delta equals the full sum; a read below the checkpoint falls back to the full sum; re-advancing
// with nothing settled is a no-op.
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

// The delta read's observed_at bound applies only while bound_ledger = last_ledger: a binary that predates the bound
// columns can move last_ledger and leave them stale, and a stale bound must not hide rows above the checkpoint.
func TestSEP41SupplyRollup_DeltaTimeBoundGuardedByLedger(t *testing.T) {
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
	settleSEP41Cursor(t, ctx, store, sep41SettledTestCursorLedger)

	const contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	t0 := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	insert := func(ledger uint32, amount int64, at time.Time) {
		t.Helper()
		if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
			ContractID: contractID, Ledger: ledger, TxHash: fmt.Sprintf("%064x", ledger), OpIndex: 0,
			ObservedAt: at, Kind: timescale.SEP41EventMint, Amount: big.NewInt(amount), Counterparty: "GA1",
		}); err != nil {
			t.Fatalf("insert @%d: %v", ledger, err)
		}
	}
	bound := func() (sql.NullInt64, sql.NullTime) {
		t.Helper()
		var l sql.NullInt64
		var at sql.NullTime
		if err := rawdb.QueryRowContext(ctx,
			`SELECT bound_ledger, bound_observed_at FROM sep41_supply_rollup WHERE contract_id = $1`, contractID,
		).Scan(&l, &at); err != nil {
			t.Fatalf("read bound: %v", err)
		}
		return l, at
	}
	assertMint := func(label string, want int64) {
		t.Helper()
		got, err := store.SEP41KindTotalsAtOrBefore(ctx, contractID, 10_000)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got.Mint.Cmp(big.NewInt(want)) != 0 {
			t.Errorf("%s: mint = %s; want %d", label, got.Mint, want)
		}
	}

	insert(1000, 1, t0)
	insert(2000, 10, t0.Add(time.Hour))
	insert(3000, 100, t0.Add(2*time.Hour)) // tip, deferred
	if adv, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil || adv.ToLedger != 2000 {
		t.Fatalf("advance = %+v, %v; want ToLedger 2000", adv, err)
	}
	if l, at := bound(); l.Int64 != 2000 || !at.Time.Equal(t0.Add(time.Hour)) {
		t.Fatalf("bound after fold = (%v, %v); want (2000, %v)", l, at, t0.Add(time.Hour))
	}
	insert(2500, 1000, t0.Add(90*time.Minute))
	assertMint("bounded delta", 1111)

	// A bound past every row proves the bound is applied while bound_ledger matches...
	if _, err := rawdb.ExecContext(ctx,
		`UPDATE sep41_supply_rollup SET bound_observed_at = $2 WHERE contract_id = $1`, contractID, t0.Add(100*time.Hour),
	); err != nil {
		t.Fatalf("set future bound: %v", err)
	}
	assertMint("matching bound applied", 11)
	// ...and ignored once last_ledger moves without it.
	if _, err := rawdb.ExecContext(ctx,
		`UPDATE sep41_supply_rollup SET bound_ledger = 1500 WHERE contract_id = $1`, contractID,
	); err != nil {
		t.Fatalf("stale bound_ledger: %v", err)
	}
	assertMint("stale bound ignored", 1111)

	if _, err := store.ResetSEP41SupplyRollupFold(ctx, []string{contractID}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// The reset refolds, so the stale bound must be replaced by one matching the new last_ledger.
	var last int64
	if err := rawdb.QueryRowContext(ctx,
		`SELECT last_ledger FROM sep41_supply_rollup WHERE contract_id = $1`, contractID,
	).Scan(&last); err != nil {
		t.Fatalf("read last_ledger: %v", err)
	}
	if l, at := bound(); l.Valid && (l.Int64 != last || !at.Time.Equal(t0.Add(90*time.Minute))) {
		t.Errorf("bound after reset = (%v, %v); want (%d, %v) or NULL", l, at, last, t0.Add(90*time.Minute))
	}
	assertMint("after reset", 1111)

	// A row folded before the bound columns existed: a fold with nothing new to add still fills the bound in.
	if _, err := rawdb.ExecContext(ctx,
		`UPDATE sep41_supply_rollup SET bound_ledger = NULL, bound_observed_at = NULL WHERE contract_id = $1`, contractID,
	); err != nil {
		t.Fatalf("clear bound: %v", err)
	}
	if adv, err := store.AdvanceSEP41SupplyRollup(ctx, contractID); err != nil || adv.ToLedger != uint32(last) {
		t.Fatalf("eventless advance = %+v, %v; want ToLedger %d", adv, err, last)
	}
	if l, at := bound(); l.Int64 != last || !at.Time.Equal(t0.Add(90*time.Minute)) {
		t.Errorf("bound after eventless fold = (%v, %v); want (%d, %v)", l, at, last, t0.Add(90*time.Minute))
	}
	assertMint("after bound fill", 1111)
}

// Genesis baseline through the real store-reader-computer path: a SAC wrapper with pre-Soroban issuance plus a large burn
// computes a positive lifetime total; a Soroban-only token is unchanged (a zero baseline doesn't double-count); the
// baseline applies only at asOfLedger >= genesis_baseline_ledger.
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

// Pins the exact ledger seam between the seeded genesis baseline (ledger < boundary) and the Soroban-era totals (>=
// boundary): a disjoint partition. With a mint at the boundary, a read below sees neither slice, a read at the boundary
// sees genesis plus the mint once each, and the tip read is identical.
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

// ResetSEP41SupplyRollupFold rebuilds the fold columns from zero in place without wiping the genesis baseline. Scoped
// reset re-folds only the listed contract, full reset re-folds every contract; neither leaves last_ledger = 0; both
// preserve genesis_mint_total and genesis_baseline_ledger.
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

// Seeding the pre-Soroban baseline after the worker already folded pre-boundary rows must not double-count that band.
// Legs: first seed rebuilds the fold under the new floor in one transaction (checkpoint fast path, each row once); a
// repeat seed of the same boundary converges (idempotent).
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

	// Leg 3: a correcting seed moves the boundary above the seam, so CH-side genesis grows and the Soroban slice shrinks;
	// the lifetime total is conserved, only the split changes.
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

// Pins what a floor-move trigger can't reach: a row already seeded while its fold still held the pre-boundary band.
// Re-running the seed with the same boundary is the remedy and must repair the row even though the floor doesn't move.
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

// A fold writer blocked on another writer's row lock must give up after the bounded lock_timeout (55P03) instead of
// waiting out a cold full-history fold, since the aggregator advances contracts sequentially and later contracts would
// queue behind it.
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

// A fold reset committing while the rollup worker is mid-pass must not be stranded: a pass that decided its boundary
// before the reset would add its delta over stale (last_ledger, mx) on freshly zeroed totals, a served undercount no later
// pass repairs. Pinned, not raced.
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

// Exercises ListSEP41RollupCheckpoints and SEP41SupplyEventKindResum through ReconcileRunningTotals: a healthy checkpoint
// reconciles clean, bounded at its own last_ledger (a deferred tip is not drift); a doubled fold column (KALE 2x) is
// flagged with Delta = +truth; the -contracts scope filters the checkpoint list.
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

// The freshness anchor must track the supply-event producer's watermark, not one contract's last mint/burn: most watched
// assets are C-address SEP-41 tokens, so event-ledger anchoring froze most served supply, and a contract with no events
// returned 0 and skipped the gate. Both the quiet-contract and no-events cases are asserted.
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

	// A row inside the recent window is found by the bounded read; an asOf below every recent row falls back to the full
	// read rather than returning 0, which would skip the gate.
	if err := store.InsertSEP41SupplyEvent(ctx, timescale.SEP41SupplyEvent{
		ContractID: lively, Ledger: 9500, TxHash: "2200000000000000000000000000000000000000000000000000000000000003",
		ObservedAt: time.Now().Add(-time.Hour), Kind: timescale.SEP41EventMint, Amount: big.NewInt(1), Counterparty: "GA1",
	}); err != nil {
		t.Fatalf("InsertSEP41SupplyEvent recent: %v", err)
	}
	if got, err = store.MinSEP41ComponentLedger(ctx, quiet, 10_001); err != nil || got != 9500 {
		t.Errorf("anchor at asOf=10001 = %d (err %v), want 9500 from the recent window", got, err)
	}
	if got, err = store.MinSEP41ComponentLedger(ctx, quiet, 9_200); err != nil || got != 9000 {
		t.Errorf("anchor at asOf=9200 = %d (err %v), want 9000 from the full-read fallback", got, err)
	}
}
