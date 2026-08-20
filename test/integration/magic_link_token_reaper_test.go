//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// PRV-2 (audit-2026-08-14) — the store half of the `magic_link_tokens`
// retention sweep.
//
// `magic_link_tokens` is durable plaintext PII (email + requested_ip)
// and its key is ATTACKER-CHOSEN: POST /v1/auth/login is
// unauthenticated and inserts a permanent row for any well-formed
// address, and a link nobody clicks is never consumed. The sibling
// `login_code_lockouts` already had a reaper; this table did not, so
// the table grew monotonically on a disk-fixed host.
//
// This proves the two things only a real database can — and the
// predicate has to be right in both directions:
//
//  1. every EXPIRED row past the retention window is reaped, consumed
//     or not (both are unredeemable);
//  2. a LIVE (unexpired) row is NEVER reaped, at any age of created_at,
//     and the sweep does not widen into the neighbouring
//     login_code_lockouts table.
func TestMagicLinkTokenReaper(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	t.Run("SweepReapsExpiredRowsOnly", func(t *testing.T) {
		const retention = 48 * time.Hour
		type fixture struct {
			email        string
			expiresHours int  // >0 = expires that many hours in the FUTURE (live), <0 = expired that long ago
			consumed     bool // whether the row was consumed
			wantReaped   bool
		}
		fixtures := []fixture{
			// The attack residue: expired long ago, never consumed
			// (nobody clicked the link). Terminal — reapable.
			{email: "reap-old-unconsumed@example.com", expiresHours: -72, wantReaped: true},
			// Expired long ago AND consumed — still terminal, reapable.
			{email: "reap-old-consumed@example.com", expiresHours: -72, consumed: true, wantReaped: true},
			// LIVE token, freshly minted. Must survive: reaping it would
			// break a real user's in-flight sign-in.
			{email: "keep-live@example.com", expiresHours: 1, wantReaped: false},
			// Expired only recently, inside retention — kept so
			// classifyMagicLinkMiss can still tell a slow user "expired"
			// rather than "not found".
			{email: "keep-recently-expired@example.com", expiresHours: -1, wantReaped: false},
			// Exactly on the keep side of the boundary (expired 47h ago,
			// retention 48h).
			{email: "keep-boundary@example.com", expiresHours: -47, wantReaped: false},
		}
		for i, f := range fixtures {
			var consumedAt any
			if f.consumed {
				consumedAt = time.Now().UTC().Add(-time.Duration(-f.expiresHours) * time.Hour)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO magic_link_tokens
				     (token_hash, email, purpose, expires_at, consumed_at, requested_ip, created_at)
				 VALUES ($1, $2, 'login',
				         now() + make_interval(hours => $3),
				         $4, '203.0.113.9',
				         now() - interval '73 hours')`,
				[]byte{byte(i), 'm', 'l', 'r', '-', 't', 'o', 'k', 'e', 'n', '-', 'h', 'a', 's', 'h', '-', 'p', 'a', 'd', 'd', 'i', 'n', 'g', '-', '3', '2', 'b', 'y', 't', 'e', 's', '!'},
				f.email, f.expiresHours, consumedAt); err != nil {
				t.Fatalf("seed %s: %v", f.email, err)
			}
		}

		// A row in the NEIGHBOURING login_code_lockouts table, aged the
		// same way — the sweep must not widen into it.
		if _, err := tokens.RegisterFailedLoginCode(ctx,
			"bystander-lockout@example.com", 10, 24*time.Hour, 24*time.Hour); err != nil {
			t.Fatalf("seed bystander lockout: %v", err)
		}

		deleted, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC().Add(-retention))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if deleted != 2 {
			t.Errorf("deleted = %d, want 2 (the two expired-beyond-retention rows only)", deleted)
		}

		for _, f := range fixtures {
			var present bool
			if err := db.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM magic_link_tokens WHERE email = $1)`,
				f.email).Scan(&present); err != nil {
				t.Fatalf("check %s: %v", f.email, err)
			}
			gone := !present
			if gone != f.wantReaped {
				verb := map[bool]string{true: "reaped", false: "kept"}
				t.Errorf("%s was %s, want %s (expiresHours=%d consumed=%v)",
					f.email, verb[gone], verb[f.wantReaped], f.expiresHours, f.consumed)
			}
		}

		var bystanders int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM login_code_lockouts WHERE email = $1`,
			"bystander-lockout@example.com").Scan(&bystanders); err != nil {
			t.Fatalf("count bystander lockout: %v", err)
		}
		if bystanders != 1 {
			t.Errorf("login_code_lockouts rows for the bystander = %d, want 1 — the sweep reached into another table",
				bystanders)
		}
	})

	// The gauge's source. A count that does not see the rows makes the
	// growth signal useless.
	t.Run("CountSeesTheRows", func(t *testing.T) {
		before, err := tokens.CountMagicLinkTokens(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		for i, email := range []string{"count-a@example.com", "count-b@example.com"} {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO magic_link_tokens
				     (token_hash, email, purpose, expires_at, requested_ip)
				 VALUES ($1, $2, 'login', now() + interval '15 minutes', '203.0.113.10')`,
				[]byte{byte(100 + i), 'c', 'o', 'u', 'n', 't', '-', 'h', 'a', 's', 'h', '-', 'p', 'a', 'd', 'd', 'i', 'n', 'g', '-', 't', 'o', '-', '3', '2', '-', 'b', 'y', 't', 'e', 's', '.'},
				email); err != nil {
				t.Fatalf("seed %s: %v", email, err)
			}
		}
		after, err := tokens.CountMagicLinkTokens(ctx)
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
			     WHERE tablename = 'magic_link_tokens'
			       AND indexdef ILIKE '%(expires_at)%'
			)`).Scan(&exists); err != nil {
			t.Fatalf("check index: %v", err)
		}
		if !exists {
			t.Error("no index leading with expires_at — the retention sweep seq-scans a table an unauthenticated caller can grow")
		}
	})
}
