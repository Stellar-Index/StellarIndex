//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Q142 (audit-2026-09-18) — the reaper sweeps over `magic_link_tokens`,
// `login_code_lockouts` and `accounts` used to issue a single unbounded
// DELETE with no LIMIT. Every one of those tables is grown by an
// unauthenticated or attacker-influenced write path, so a sweep landing
// on a large backlog held row locks and WAL for as long as the DELETE
// ran, on the shared connection pool the request path uses.
//
// This proves the bound is real: with the batch size overridden small,
// a single Sweep call over a backlog LARGER than one call's cap
// (batchSize * sweepMaxBatchesPerCall) deletes only up to the cap and
// leaves the remainder for the next call — the unbounded pre-fix
// DELETE would have removed everything in the first call.
func TestSweepExpiredMagicLinkTokens_BoundedPerCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Small batch size + the package's fixed sweepMaxBatchesPerCall (25)
	// gives a per-call cap of batchSize*25 that a modest seed count can
	// exceed without seeding tens of thousands of rows.
	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10  // exceeds one call's cap

	tokens := postgresstore.NewTokenStore(postgresstore.New(db)).WithSweepBatchSize(batchSize)

	for i := 0; i < seedRows; i++ {
		hash := make([]byte, 32)
		hash[0] = byte(i)
		hash[1] = byte(i >> 8)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO magic_link_tokens
			     (token_hash, email, purpose, expires_at, requested_ip)
			 VALUES ($1, 'bulk-expired@example.com', 'login',
			         now() - interval '1 hour', '203.0.113.9')`,
			hash); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	deletedFirstCall, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deletedFirstCall != perCallCap {
		t.Errorf("first call deleted = %d, want exactly the per-call cap %d "+
			"(an unbounded DELETE would have removed all %d seeded rows in one call)",
			deletedFirstCall, perCallCap, seedRows)
	}

	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM magic_link_tokens WHERE email = 'bulk-expired@example.com'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	wantRemaining := seedRows - perCallCap
	if remaining != wantRemaining {
		t.Errorf("rows remaining after first call = %d, want %d", remaining, wantRemaining)
	}

	// The next call (the reaper's next tick) finishes the backlog.
	deletedSecondCall, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if deletedSecondCall != int64(wantRemaining) {
		t.Errorf("second call deleted = %d, want %d (the remainder)", deletedSecondCall, wantRemaining)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM magic_link_tokens WHERE email = 'bulk-expired@example.com'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count final: %v", err)
	}
	if remaining != 0 {
		t.Errorf("rows remaining after second call = %d, want 0", remaining)
	}
}

// T371/T372/T374 — SweepEndedSessions shares deleteInBatches with the
// sweeps above, but nothing proved its own call site actually threads
// the per-call cap: a bug pinning it to defaultSweepBatchRows instead of
// r.sweepBatchRows would still pass the table-driven correctness test in
// platform_retention_reaper_test.go, since that test never seeds past
// one batch.
func TestSweepEndedSessions_BoundedPerCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	pg := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(pg)
	acct, err := accounts.Create(ctx, platform.Account{
		Name: "Bounded Sweep Co", Slug: "bounded-sweep-co",
		BillingEmail: "billing@bounded-sweep.example",
		Tier:         platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(pg)
	user, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "owner@bounded-sweep.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10  // exceeds one call's cap

	for i := 0; i < seedRows; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, expires_at, ip_first_seen, ip_last_seen, user_agent)
			 VALUES ($1, $2, now() - interval '1 hour', '203.0.113.9', '203.0.113.9', 'bulk')`,
			[]byte(fmt.Sprintf("bounded-sweep-session-%04d", i)), user.ID); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	sessions := users.WithSweepBatchSize(batchSize)

	deletedFirstCall, err := sessions.SweepEndedSessions(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deletedFirstCall != perCallCap {
		t.Errorf("first call deleted = %d, want exactly the per-call cap %d", deletedFirstCall, perCallCap)
	}

	deletedSecondCall, err := sessions.SweepEndedSessions(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if wantRemaining := int64(seedRows - perCallCap); deletedSecondCall != wantRemaining {
		t.Errorf("second call deleted = %d, want %d (the remainder)", deletedSecondCall, wantRemaining)
	}

	var remaining int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1`, user.ID,
	).Scan(&remaining); err != nil {
		t.Fatalf("count final: %v", err)
	}
	if remaining != 0 {
		t.Errorf("rows remaining after second call = %d, want 0", remaining)
	}
}

// CA2-A33-harden-1 — SweepLoginCodeLockouts reports whether it stopped
// at the per-call cap. The reaper re-sweeps promptly on that signal
// instead of waiting a full Interval, so the signal must be true exactly
// when every batch came back full, and false once the backlog is gone.
func TestSweepLoginCodeLockouts_ReportsCapHit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const batchSize = 4
	const perCallCap = batchSize * 25 // matches postgresstore.sweepMaxBatchesPerCall
	const seedRows = perCallCap + 10

	tokens := postgresstore.NewTokenStore(postgresstore.New(db)).WithSweepBatchSize(batchSize)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO login_code_lockouts (email, failed_count, updated_at)
		 SELECT 'bulk-' || g || '@example.com', 1, now() - interval '72 hours'
		   FROM generate_series(1, $1::int) AS g`, seedRows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cutoff := time.Now().UTC().Add(-48 * time.Hour)
	deleted, more, err := tokens.SweepLoginCodeLockouts(ctx, cutoff)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if deleted != perCallCap || !more {
		t.Errorf("first call = (%d, more=%v), want (%d, more=true)", deleted, more, perCallCap)
	}

	deleted, more, err = tokens.SweepLoginCodeLockouts(ctx, cutoff)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if want := int64(seedRows - perCallCap); deleted != want || more {
		t.Errorf("second call = (%d, more=%v), want (%d, more=false)", deleted, more, want)
	}
}
