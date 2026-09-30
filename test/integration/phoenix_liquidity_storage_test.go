//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPhoenixLiquidityRoundTrip exercises InsertPhoenixLiquidityChange
// across both provide_liquidity (token addresses populated, shares
// NULL) and withdraw_liquidity (token addresses NULL, shares
// populated) shapes through real TimescaleDB. Validates the
// migration 0044 PK, the per-column NULL-on-mismatch behaviour, the
// idempotent ON CONFLICT DO NOTHING semantics, and that a large
// i128 amount round-trips through NUMERIC without precision loss
// (ADR-0003).
func TestPhoenixLiquidityRoundTrip(t *testing.T) {
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
		pool   = "CDPHXPOOL00000000000000000000000000000000000000000000A"
		sender = "GPHXSENDER0000000000000000000000000000000000000000000B"
		tokenA = "CDTKNA000000000000000000000000000000000000000000000C"
		tokenB = "CDTKNB000000000000000000000000000000000000000000000D"
		largeI = "123456789012345678901234567890" // > 2^63
	)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	// ─── Provide_liquidity row ──────────────────────────────────
	provide := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_000,
		ObservedAt: t0,
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000001",
		OpIndex:    0,
		Action:     timescale.PhoenixProvideLiquidity,
		Sender:     sender,
		TokenA:     tokenA,
		TokenB:     tokenB,
		AmountA:    "1000000000",
		AmountB:    "50000000",
		// SharesAmount intentionally empty — provide rows don't carry it.
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, provide); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (provide): %v", err)
	}

	// Idempotent re-insert — same PK is a no-op.
	if err := store.InsertPhoenixLiquidityChange(ctx, provide); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (provide dup): %v", err)
	}

	// ─── Withdraw_liquidity row at the same pool / later ledger ─
	withdraw := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_100,
		ObservedAt: t0.Add(time.Hour),
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000002",
		OpIndex:    0,
		Action:     timescale.PhoenixWithdrawLiquidity,
		Sender:     sender,
		// TokenA / TokenB intentionally empty — withdraw doesn't carry them.
		AmountA:      "990000000",
		AmountB:      "49000000",
		SharesAmount: "7000000",
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, withdraw); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (withdraw): %v", err)
	}

	// ─── Large-i128 row to prove NUMERIC round-trips ────────────
	huge := timescale.PhoenixLiquidityChange{
		Pool:       pool,
		Ledger:     62_500_200,
		ObservedAt: t0.Add(2 * time.Hour),
		TxHash:     "3300000000000000000000000000000000000000000000000000000000000003",
		OpIndex:    0,
		Action:     timescale.PhoenixProvideLiquidity,
		Sender:     sender,
		TokenA:     tokenA,
		TokenB:     tokenB,
		AmountA:    largeI,
		AmountB:    largeI,
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, huge); err != nil {
		t.Fatalf("InsertPhoenixLiquidityChange (huge): %v", err)
	}

	// ─── Verify shape — query directly via the store's DB handle ─
	type row struct {
		action       string
		tokenA       *string
		tokenB       *string
		amountA      string
		amountB      string
		sharesAmount *string
	}
	var rows []row
	q := `
        SELECT action, token_a, token_b, amount_a::text, amount_b::text, shares_amount::text
          FROM phoenix_liquidity
         WHERE pool = $1
         ORDER BY ledger
    `
	dbh := store.DB()
	rs, err := dbh.QueryContext(ctx, q, pool)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.action, &r.tokenA, &r.tokenB, &r.amountA, &r.amountB, &r.sharesAmount); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows (after dup-insert), want 3", len(rows))
	}

	// Provide row: tokens populated, shares NULL.
	if rows[0].action != "provide_liquidity" {
		t.Errorf("rows[0].action = %q", rows[0].action)
	}
	if rows[0].tokenA == nil || *rows[0].tokenA != tokenA {
		t.Errorf("rows[0].tokenA = %v, want %q", rows[0].tokenA, tokenA)
	}
	if rows[0].sharesAmount != nil {
		t.Errorf("rows[0].sharesAmount = %v, want NULL", *rows[0].sharesAmount)
	}

	// Withdraw row: tokens NULL, shares populated.
	if rows[1].action != "withdraw_liquidity" {
		t.Errorf("rows[1].action = %q", rows[1].action)
	}
	if rows[1].tokenA != nil {
		t.Errorf("rows[1].tokenA = %v, want NULL (withdraw doesn't emit token addresses)", *rows[1].tokenA)
	}
	if rows[1].sharesAmount == nil || *rows[1].sharesAmount != "7000000" {
		t.Errorf("rows[1].sharesAmount = %v, want 7000000", rows[1].sharesAmount)
	}

	// Large-i128 row: amount round-trips exactly.
	if rows[2].amountA != largeI {
		t.Errorf("rows[2].amountA = %q, want %q (NUMERIC precision lost?)", rows[2].amountA, largeI)
	}
}

// TestPhoenixStakeEventsRoundTrip exercises InsertPhoenixStakeEvent
// across bond + unbond with a shared user/contract/lp_token. The
// action column discriminator distinguishes the two directions; the
// PK keeps a same-(ledger, tx, op) bond+unbond pair from colliding.
func TestPhoenixStakeEventsRoundTrip(t *testing.T) {
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
		stakeContract = "CDSTAKE0000000000000000000000000000000000000000000000A"
		user          = "GUSER000000000000000000000000000000000000000000000000B"
		lpToken       = "CDLPTOK0000000000000000000000000000000000000000000000C"
	)
	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract,
		Ledger:        62_500_300,
		ObservedAt:    t0,
		TxHash:        "4400000000000000000000000000000000000000000000000000000000000001",
		OpIndex:       0,
		Action:        timescale.PhoenixBond,
		User:          user,
		LPToken:       lpToken,
		Amount:        "1000000",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (bond): %v", err)
	}

	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract,
		Ledger:        62_500_400,
		ObservedAt:    t0.Add(time.Hour),
		TxHash:        "4400000000000000000000000000000000000000000000000000000000000002",
		OpIndex:       0,
		Action:        timescale.PhoenixUnbond,
		User:          user,
		LPToken:       lpToken,
		Amount:        "400000",
	}); err != nil {
		t.Fatalf("InsertPhoenixStakeEvent (unbond): %v", err)
	}

	// Same (ledger, tx, op) for a bond+unbond pair: distinct rows.
	const sharedTx = "4400000000000000000000000000000000000000000000000000000000000003"
	for _, action := range []timescale.PhoenixStakeAction{timescale.PhoenixBond, timescale.PhoenixUnbond} {
		if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
			StakeContract: stakeContract,
			Ledger:        62_500_500,
			ObservedAt:    t0.Add(2 * time.Hour),
			TxHash:        sharedTx,
			OpIndex:       0,
			Action:        action,
			User:          user,
			LPToken:       lpToken,
			Amount:        "1",
		}); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent (%s @ shared tx): %v", action, err)
		}
	}

	// ─── Verify: 4 rows total ────────────────────────────────────
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM phoenix_stake_events WHERE stake_contract = $1`,
		stakeContract).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 4 {
		t.Errorf("count = %d, want 4 (bond/unbond pair at shared tx not colliding)", count)
	}

	// ─── Per-action breakdown ────────────────────────────────────
	type tally struct {
		action string
		n      int
		sum    string
	}
	var ts []tally
	rs, err := store.DB().QueryContext(ctx, `
        SELECT action, count(*), sum(amount)::text
          FROM phoenix_stake_events
         WHERE stake_contract = $1
         GROUP BY action
         ORDER BY action
    `, stakeContract)
	if err != nil {
		t.Fatalf("group query: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var ti tally
		if err := rs.Scan(&ti.action, &ti.n, &ti.sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ts = append(ts, ti)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(ts))
	}
	// bond: 1000000 + 1 = 1000001
	// unbond: 400000 + 1 = 400001
	if ts[0].action != "bond" || ts[0].n != 2 || ts[0].sum != "1000001" {
		t.Errorf("bond tally = %+v, want {bond 2 1000001}", ts[0])
	}
	if ts[1].action != "unbond" || ts[1].n != 2 || ts[1].sum != "400001" {
		t.Errorf("unbond tally = %+v, want {unbond 2 400001}", ts[1])
	}
}

// TestPhoenixLifecycleAndConfigEventsRoundTrip lands every action
// migration 0192 admits: a create_distribution_flow row with no user, the
// migration_* rows with no token, and the factory / blend-pool admin rows,
// with the i128 minimum-trading value round-tripping through NUMERIC.
func TestPhoenixLifecycleAndConfigEventsRoundTrip(t *testing.T) {
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
		stakeContract = "CDSTAKE0000000000000000000000000000000000000000000000A"
		user          = "GUSER000000000000000000000000000000000000000000000000B"
		asset         = "CDASSET0000000000000000000000000000000000000000000000C"
		pool          = "CDPHXPOOL00000000000000000000000000000000000000000000A"
		maxI128       = "170141183460469231731687303715884105727"
		tx            = "5500000000000000000000000000000000000000000000000000000000000001"
	)
	t0 := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	stake := []timescale.PhoenixStakeEvent{
		{Action: timescale.PhoenixCreateDistributionFlow, LPToken: asset},
		{Action: timescale.PhoenixMigrationStarted, User: user},
		{Action: timescale.PhoenixMigrationQueried, User: user},
		{Action: timescale.PhoenixMigrationCompleted, User: user},
	}
	for i, e := range stake {
		e.StakeContract, e.Ledger, e.ObservedAt, e.TxHash, e.EventIndex = stakeContract, 63_000_000, t0, tx, uint32(i) //nolint:gosec // small test index
		if err := store.InsertPhoenixStakeEvent(ctx, e); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent (%s): %v", e.Action, err)
		}
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: stakeContract, Ledger: 63_000_000, ObservedAt: t0, TxHash: tx, EventIndex: 9,
		Action: timescale.PhoenixMigrationStarted,
	}); err == nil {
		t.Error("a migration_started row with no user was accepted")
	}

	rs, err := store.DB().QueryContext(ctx, `
        SELECT action, user_addr, lp_token, amount::text
          FROM phoenix_stake_events
         WHERE stake_contract = $1
         ORDER BY event_index`, stakeContract)
	if err != nil {
		t.Fatalf("stake query: %v", err)
	}
	defer rs.Close()
	n := 0
	for rs.Next() {
		var action string
		var userAddr, lpToken, amount *string
		if err := rs.Scan(&action, &userAddr, &lpToken, &amount); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want := stake[n]
		if action != string(want.Action) || (userAddr != nil) != (want.User != "") ||
			(lpToken != nil) != (want.LPToken != "") || amount != nil {
			t.Errorf("row %d = %s user=%v lp_token=%v amount=%v, want %+v", n, action, userAddr, lpToken, amount, want)
		}
		n++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != len(stake) {
		t.Fatalf("got %d stake rows, want %d", n, len(stake))
	}

	admin := []timescale.PhoenixAdminEvent{
		{Pool: pool, AdminAction: "factory_config_updated"},
		{Pool: pool, AdminAction: "blend_set_delegate", Admin: asset},
		{Pool: pool, AdminAction: "blend_set_min_trading_a", Value: "0"},
		{Pool: pool, AdminAction: "blend_set_min_trading_b", Value: maxI128},
	}
	for i, e := range admin {
		e.Ledger, e.LedgerCloseTime, e.TxHash, e.EventIndex = 63_000_001, t0, tx, uint32(i) //nolint:gosec // small test index
		if err := store.InsertPhoenixAdmin(ctx, e); err != nil {
			t.Fatalf("InsertPhoenixAdmin (%s): %v", e.AdminAction, err)
		}
	}
	ar, err := store.DB().QueryContext(ctx, `
        SELECT admin_action, admin, value::text
          FROM phoenix_admin_events
         WHERE pool = $1
         ORDER BY event_index`, pool)
	if err != nil {
		t.Fatalf("admin query: %v", err)
	}
	defer ar.Close()
	n = 0
	for ar.Next() {
		var action string
		var adminAddr, value *string
		if err := ar.Scan(&action, &adminAddr, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want := admin[n]
		gotValue := ""
		if value != nil {
			gotValue = *value
		}
		if action != want.AdminAction || (adminAddr != nil) != (want.Admin != "") || gotValue != want.Value {
			t.Errorf("row %d = %s admin=%v value=%q, want %+v", n, action, adminAddr, gotValue, want)
		}
		n++
	}
	if err := ar.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n != len(admin) {
		t.Fatalf("got %d admin rows, want %d", n, len(admin))
	}
}
