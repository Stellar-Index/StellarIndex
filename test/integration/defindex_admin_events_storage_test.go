//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	dxAdminVault    = "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ"
	dxAdminStrategy = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	dxAdminCaller   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	dxAdminNew      = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
)

// TestDefindexAdminEventsInsert executes InsertDefindexAdminEvent against
// migration 0192: every kind's shape lands, a replay is idempotent, the
// i128 amount survives above 2^63, and the per-kind CHECKs reject a row
// carrying the wrong columns.
func TestDefindexAdminEventsInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const bigAmount = "170141183460469231731687303715884105727" // i128 max
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	row := func(n int, kind string) timescale.DefindexAdminEvent {
		return timescale.DefindexAdminEvent{
			Ledger: uint32(62_000_000 + n), LedgerCloseTime: t0.Add(time.Duration(n) * time.Minute),
			TxHash: pad64("d", n), ContractID: dxAdminVault, EventKind: kind,
		}
	}
	rescue := row(0, "rescue")
	rescue.Caller, rescue.Strategy, rescue.Amount = dxAdminCaller, dxAdminStrategy, bigAmount
	paused := row(1, "paused")
	paused.Caller, paused.Strategy = dxAdminCaller, dxAdminStrategy
	unpaused := row(2, "unpaused")
	unpaused.Caller, unpaused.Strategy = dxAdminCaller, dxAdminStrategy
	nreceiver := row(3, "nreceiver")
	nreceiver.Caller, nreceiver.NewAddress = dxAdminCaller, dxAdminNew
	valid := []timescale.DefindexAdminEvent{rescue, paused, unpaused, nreceiver}
	for i, kind := range []string{"nmanager", "nemanager", "rbmanager"} {
		e := row(4+i, kind)
		e.NewAddress = dxAdminNew
		valid = append(valid, e)
	}

	for pass := 0; pass < 2; pass++ {
		for _, e := range valid {
			if err := store.InsertDefindexAdminEvent(ctx, e); err != nil {
				t.Fatalf("pass %d InsertDefindexAdminEvent %s: %v", pass, e.EventKind, err)
			}
		}
	}
	var n int
	mustScan(t, ctx, db, &n, `SELECT count(*) FROM defindex_admin_events`)
	if n != len(valid) {
		t.Fatalf("rows after two passes = %d, want %d (upsert must be idempotent)", n, len(valid))
	}
	var amount string
	mustScan(t, ctx, db, &amount, `SELECT amount::text FROM defindex_admin_events WHERE event_kind = 'rescue'`)
	if amount != bigAmount {
		t.Fatalf("rescue amount = %s, want %s", amount, bigAmount)
	}

	rescueNoAmount := row(10, "rescue")
	rescueNoAmount.Caller, rescueNoAmount.Strategy = dxAdminCaller, dxAdminStrategy
	managerWithCaller := row(11, "nmanager")
	managerWithCaller.Caller, managerWithCaller.NewAddress = dxAdminCaller, dxAdminNew
	pausedNoStrategy := row(12, "paused")
	pausedNoStrategy.Caller = dxAdminCaller
	for _, e := range []timescale.DefindexAdminEvent{rescueNoAmount, managerWithCaller, pausedNoStrategy} {
		if err := store.InsertDefindexAdminEvent(ctx, e); err == nil {
			t.Errorf("InsertDefindexAdminEvent %s with the wrong columns succeeded, want a CHECK violation", e.EventKind)
		}
	}
}
