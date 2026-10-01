//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
