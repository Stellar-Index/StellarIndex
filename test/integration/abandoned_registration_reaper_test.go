//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Invariant: no unused anonymous /v1/register account outlives
// accounterasure.AbandonedRegistrationRetention. Seeds the register shape
// plus one near-miss per predicate and runs the real sweep.
func TestAbandonedRegistrationSweep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb, _ := startRedis(t, ctx)
	accounts := postgresstore.NewAccountStore(postgresstore.New(db))
	eraser := &accounterasure.Eraser{Store: accounts, Redis: rdb}

	old := time.Now().Add(-accounterasure.AbandonedRegistrationRetention - 24*time.Hour)
	seed := func(slug string, created time.Time, used, member, liveRecord, withKey bool, tier string) {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx, `INSERT INTO accounts (name, slug, billing_email, tier, created_at)
			VALUES ($1, $1, 'victim@example.com', $2, $3) RETURNING id`, slug, tier, created).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
		if member {
			mustExec(t, ctx, db, `INSERT INTO users (account_id, email, role) VALUES ($1, $2, 'owner')`, id, slug+"@example.com")
		}
		if !withKey {
			return
		}
		hash := sha256.Sum256([]byte("sip_" + slug))
		mustExec(t, ctx, db, `INSERT INTO api_keys (id, account_id, name, key_hash, key_prefix, tier, rate_limit_per_min, last_used_at)
			VALUES ($1, $2, 'registration key', $3, 'sip_00000000', 'apikey', 60, $4)`,
			"kid_"+hex.EncodeToString(hash[:6]), id, hash[:], map[bool]any{true: time.Now(), false: nil}[used])
		if liveRecord {
			if err := rdb.Set(ctx, cachekeys.APIKey(hex.EncodeToString(hash[:])).String(), "{}", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("abandoned", old, false, false, false, true, "free")
	seed("young", time.Now().Add(-24*time.Hour), false, false, false, true, "free")
	seed("used", old, true, false, false, true, "free")
	seed("member", old, false, true, false, true, "free")
	seed("live-record", old, false, false, true, true, "free")
	seed("no-key", old, false, false, false, false, "free")
	seed("promoted", old, false, false, false, true, "pro")

	cutoff := time.Now().Add(-accounterasure.AbandonedRegistrationRetention)
	n, err := eraser.SweepAbandonedRegistrations(ctx, accounts, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("sweep: erased %d, err %v; want 1, nil", n, err)
	}
	remaining := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT slug FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		remaining[s] = true
	}
	_ = rows.Close()
	if remaining["abandoned"] {
		t.Error("abandoned registration survived the sweep")
	}
	for _, s := range []string{"young", "used", "member", "live-record", "no-key", "promoted"} {
		if !remaining[s] {
			t.Errorf("%s was erased; only an unused, expired, member-less free registration may be", s)
		}
	}
	var keys int
	abandonedHash := sha256.Sum256([]byte("sip_abandoned"))
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE key_hash = $1`, abandonedHash[:]).Scan(&keys); err != nil || keys != 0 {
		t.Errorf("abandoned account's api_keys rows = %d (err %v), want 0", keys, err)
	}
	if n, err := eraser.SweepAbandonedRegistrations(ctx, accounts, cutoff); err != nil || n != 0 {
		t.Errorf("second sweep: erased %d, err %v; want 0, nil", n, err)
	}

	// A member who joins between the plan and the erase must stop the erase.
	seed("raced", old, false, false, false, true, "free")
	var racedID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE slug = 'raced'`).Scan(&racedID); err != nil {
		t.Fatal(err)
	}
	plan, err := accounts.PlanErasure(ctx, racedID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, db, `INSERT INTO users (account_id, email, role) VALUES ($1, 'raced@example.com', 'owner')`, racedID)
	_, err = accounts.EraseAccount(ctx, postgresstore.ErasureRequest{Plan: plan, ErasedSubject: "erased:x", Actor: platform.ActorSystem, RequireNoUsers: true})
	if !errors.Is(err, platform.ErrConflict) {
		t.Errorf("erase after a member joined: err %v, want ErrConflict", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = $1 AND status = 'active'`, racedID).Scan(&left); err != nil || left != 1 {
		t.Errorf("raced account active rows = %d (err %v), want 1", left, err)
	}
}
