//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// C3-032 (audit-2026-07-23) — the store half of the durable per-email
// code-guess lockout (migration 0122).
//
// The handler half lives in
// internal/api/v1/dashboardauth/login_lockout_test.go and proves the policy:
// a token re-mint no longer hands out a fresh guess budget. This proves the
// three things only a real database can:
//
//  1. the counter is DURABLE — it is a row, not a Redis key with a TTL, so a
//     cache flush cannot clear it (which is the entire finding);
//  2. the UPSERT is atomic under concurrency — parallel wrong guesses for one
//     address cannot both read n and both write n+1, which would let a
//     parallel grinder run past the cap;
//  3. the window/lock arithmetic is Postgres', not Go's.
func TestLoginCodeLockout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		maxFailures = 10
		window      = 24 * time.Hour
		lockFor     = 24 * time.Hour
	)
	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	t.Run("CountsUpToTheCapThenLocks", func(t *testing.T) {
		const email = "grind@example.com"
		for i := 1; i < maxFailures; i++ {
			state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
			if err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
			if state.FailedCount != i {
				t.Fatalf("failure %d: FailedCount = %d, want %d", i, state.FailedCount, i)
			}
			if state.Locked(time.Now().UTC()) {
				t.Fatalf("locked early at failure %d of %d", i, maxFailures)
			}
		}
		state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
		if err != nil {
			t.Fatalf("capping failure: %v", err)
		}
		if !state.Locked(time.Now().UTC()) {
			t.Fatalf("not locked after %d failures (FailedCount=%d, LockedUntil=%v)",
				maxFailures, state.FailedCount, state.LockedUntil)
		}
		// And a fresh read agrees — the lock is in the row, not in the
		// return value of the call that set it.
		read, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !read.Locked(time.Now().UTC()) {
			t.Error("LoginCodeLockoutStatus does not see the lock the register call reported")
		}
	})

	t.Run("UnknownEmailIsNotLocked", func(t *testing.T) {
		state, err := tokens.LoginCodeLockoutStatus(ctx, "never-seen@example.com")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state != (platform.LoginCodeLockout{}) {
			t.Errorf("state = %+v, want the zero value for an address with no failures", state)
		}
	})

	t.Run("ClearResetsTheCounter", func(t *testing.T) {
		const email = "cleared@example.com"
		for i := 0; i < maxFailures; i++ {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		if err := tokens.ClearLoginCodeLockout(ctx, email); err != nil {
			t.Fatalf("clear: %v", err)
		}
		state, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state.FailedCount != 0 || state.Locked(time.Now().UTC()) {
			t.Errorf("state = %+v after clear, want the zero value", state)
		}
		// Idempotent — a second clear (or one for an unknown address) is
		// not an error; the handler calls it on every successful login.
		if err := tokens.ClearLoginCodeLockout(ctx, email); err != nil {
			t.Errorf("second clear: %v", err)
		}
	})

	// The parallel grinder. Without an atomic UPSERT, N concurrent
	// failures collapse into far fewer counted ones and the cap is
	// effectively raised by whatever concurrency the attacker can muster.
	t.Run("ConcurrentFailuresAllCount", func(t *testing.T) {
		const (
			email   = "parallel@example.com"
			racers  = 12
			bigCap  = 1000 // high enough that no lock arms mid-race
			bigLock = time.Hour
		)
		var wg sync.WaitGroup
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := tokens.RegisterFailedLoginCode(ctx, email, bigCap, window, bigLock); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent register: %v", err)
		}

		state, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if state.FailedCount != racers {
			t.Errorf("FailedCount = %d after %d concurrent failures, want %d — "+
				"lost increments let a parallel grinder run past the cap", state.FailedCount, racers, racers)
		}
	})

	// A window that fully elapses without reaching the cap starts fresh:
	// a user who mistypes twice in March must not carry that into June.
	t.Run("ElapsedWindowRestartsTheCount", func(t *testing.T) {
		const email = "stale-window@example.com"
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("first failure: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE login_code_lockouts
			    SET window_started_at = now() - interval '48 hours',
			        failed_count      = 9
			  WHERE email = $1`, email); err != nil {
			t.Fatalf("age the window: %v", err)
		}
		state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor)
		if err != nil {
			t.Fatalf("post-window failure: %v", err)
		}
		if state.FailedCount != 1 {
			t.Errorf("FailedCount = %d after an elapsed window, want 1 (the window restarts)", state.FailedCount)
		}
		if state.Locked(time.Now().UTC()) {
			t.Error("locked on the first failure of a fresh window")
		}
	})

	// An in-force lock is NOT slid forward by further guessing: otherwise
	// an attacker could hold a victim's code path shut indefinitely by
	// guessing once every 23 hours.
	t.Run("FurtherFailuresDoNotExtendAnActiveLock", func(t *testing.T) {
		const email = "no-extend@example.com"
		for i := 0; i < maxFailures; i++ {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		first, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("extra failure: %v", err)
		}
		after, err := tokens.LoginCodeLockoutStatus(ctx, email)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !after.LockedUntil.Equal(first.LockedUntil) {
			t.Errorf("LockedUntil moved from %v to %v — a grinder can hold the lock open forever",
				first.LockedUntil, after.LockedUntil)
		}
	})

	// ── Retention sweep ──
	//
	// `login_code_lockouts.email` is ATTACKER-CHOSEN: POST
	// /v1/auth/verify-code is unauthenticated and accepts any well-formed
	// address, so one wrong guess against a synthetic address inserts a
	// row that ClearLoginCodeLockout can never remove (nobody can sign in
	// as an address that does not exist). Bounded only by the anonymous
	// rate limit, that is a slow remote table-fill on a disk-fixed host.
	//
	// The sweep is what bounds it — and the predicate has to be exactly
	// right in both directions: reap settled rows, never touch a live
	// lock.
	t.Run("SweepReapsSettledRowsOnly", func(t *testing.T) {
		const retention = 48 * time.Hour
		type fixture struct {
			email       string
			ageHours    int
			lockedHours int // >0 = locked into the future, <0 = lock expired that long ago
			wantReaped  bool
		}
		fixtures := []fixture{
			// The attack residue: old, never locked, nobody owns it.
			{email: "sweep-old-unlocked@example.com", ageHours: 72, wantReaped: true},
			// Old and its lock has since expired — settled, reapable.
			{email: "sweep-old-expired-lock@example.com", ageHours: 72, lockedHours: -1, wantReaped: true},
			// Old but STILL LOCKED. Must survive at any age: reaping it
			// would hand a grinder an early release.
			{email: "sweep-old-live-lock@example.com", ageHours: 72, lockedHours: 6, wantReaped: false},
			// Recent, inside retention — a real user's in-progress
			// counting window.
			{email: "sweep-fresh@example.com", ageHours: 1, wantReaped: false},
			// Exactly at the boundary, on the keep side.
			{email: "sweep-boundary@example.com", ageHours: 47, wantReaped: false},
		}
		for _, f := range fixtures {
			if _, err := tokens.RegisterFailedLoginCode(ctx, f.email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("seed %s: %v", f.email, err)
			}
			if _, err := db.ExecContext(ctx,
				`UPDATE login_code_lockouts
				    SET updated_at   = now() - make_interval(hours => $2),
				        locked_until = CASE WHEN $3 = 0 THEN NULL
				                            ELSE now() + make_interval(hours => $3) END
				  WHERE email = $1`,
				f.email, f.ageHours, f.lockedHours); err != nil {
				t.Fatalf("age %s: %v", f.email, err)
			}
		}

		// A row in a NEIGHBOURING table with the same shape of age — the
		// sweep must not widen into it.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO magic_link_tokens (token_hash, email, purpose, expires_at, requested_ip, created_at)
			 VALUES ($1, $2, 'login', now() + interval '15 minutes', '203.0.113.7', now() - interval '72 hours')`,
			[]byte("sweep-bystander-token-hash-32byt"), "bystander@example.com"); err != nil {
			t.Fatalf("seed bystander token: %v", err)
		}

		deleted, err := tokens.SweepLoginCodeLockouts(ctx, time.Now().UTC().Add(-retention))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if deleted != 2 {
			t.Errorf("deleted = %d, want 2 (the two settled rows only)", deleted)
		}

		for _, f := range fixtures {
			state, err := tokens.LoginCodeLockoutStatus(ctx, f.email)
			if err != nil {
				t.Fatalf("status %s: %v", f.email, err)
			}
			gone := state == (platform.LoginCodeLockout{})
			if gone != f.wantReaped {
				verb := map[bool]string{true: "reaped", false: "kept"}
				t.Errorf("%s was %s, want %s (age=%dh locked=%dh)",
					f.email, verb[gone], verb[f.wantReaped], f.ageHours, f.lockedHours)
			}
		}

		var bystanders int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM magic_link_tokens WHERE email = $1`,
			"bystander@example.com").Scan(&bystanders); err != nil {
			t.Fatalf("count bystander: %v", err)
		}
		if bystanders != 1 {
			t.Errorf("magic_link_tokens rows for the bystander = %d, want 1 — the sweep reached into another table",
				bystanders)
		}
	})

	// The gauge's source. A count that does not see the rows makes the
	// growth signal useless.
	t.Run("CountSeesTheRows", func(t *testing.T) {
		before, err := tokens.CountLoginCodeLockouts(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		for _, email := range []string{"count-a@example.com", "count-b@example.com"} {
			if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
				t.Fatalf("seed %s: %v", email, err)
			}
		}
		after, err := tokens.CountLoginCodeLockouts(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if after != before+2 {
			t.Errorf("count = %d, want %d", after, before+2)
		}
	})

	// The sweep's driving column must be indexed: the table's size is
	// attacker-influenced, so a seq scan here is a second-order DoS.
	t.Run("SweepPredicateIsIndexed", func(t *testing.T) {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM pg_indexes
			     WHERE tablename = 'login_code_lockouts'
			       AND indexdef ILIKE '%(updated_at)%'
			)`).Scan(&exists); err != nil {
			t.Fatalf("check index: %v", err)
		}
		if !exists {
			t.Error("no index leading with updated_at — the retention sweep seq-scans a table an unauthenticated caller can grow")
		}
	})

	// Durability, stated plainly: the state is a row. This is the whole
	// point of the finding — the pre-fix bound lived in Redis and a flush
	// cleared it.
	t.Run("StateIsARowNotACacheEntry", func(t *testing.T) {
		const email = "durable@example.com"
		if _, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, window, lockFor); err != nil {
			t.Fatalf("failure: %v", err)
		}
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT failed_count FROM login_code_lockouts WHERE email = $1`, email).Scan(&count); err != nil {
			t.Fatalf("read the row directly: %v", err)
		}
		if count != 1 {
			t.Errorf("login_code_lockouts.failed_count = %d, want 1", count)
		}
	})
}
