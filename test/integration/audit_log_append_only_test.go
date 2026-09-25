//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
)

// appendOnlyFixture is one account with a user, one signup-race orphan
// the reaper will delete, and an audit row pointing at each.
type appendOnlyFixture struct {
	accounts *postgresstore.AccountStore
	liveRow  uuid.UUID // account_id + actor_user_id both point at live rows
	reapRow  uuid.UUID // account_id points at the orphan
	userID   uuid.UUID
}

// TestAuditLogAppendOnly pins migration 0179 (GH #969): the app role
// cannot rewrite, detach, delete or truncate the audit trail, while the
// ON DELETE SET NULL foreign keys still let the signup reaper delete an
// account (and a user be deleted) without losing its audit rows.
func TestAuditLogAppendOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := seedAppendOnlyFixture(t, ctx, db)

	t.Run("RefusesTampering", func(t *testing.T) {
		before := auditSnapshot(t, ctx, db)
		for name, stmt := range map[string]string{
			"rewrite action":      `UPDATE audit_log SET action = 'nothing.happened' WHERE id = $1`,
			"rewrite metadata":    `UPDATE audit_log SET metadata = '{}'::jsonb WHERE id = $1`,
			"detach live account": `UPDATE audit_log SET account_id = NULL WHERE id = $1`,
			"detach live actor":   `UPDATE audit_log SET actor_user_id = NULL WHERE id = $1`,
			"repoint account":     `UPDATE audit_log SET account_id = (SELECT id FROM accounts WHERE slug = 'audit-orphan') WHERE id = $1`,
			"delete row":          `DELETE FROM audit_log WHERE id = $1`,
		} {
			_, err := db.ExecContext(ctx, stmt, f.liveRow)
			requireAppendOnlyRefusal(t, name, err)
		}
		_, err := db.ExecContext(ctx, `TRUNCATE audit_log`)
		requireAppendOnlyRefusal(t, "truncate", err)
		if after := auditSnapshot(t, ctx, db); after != before {
			t.Errorf("audit_log changed under refused statements:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("ReaperDeleteNullsAccountAndKeepsRow", func(t *testing.T) {
		n, err := f.accounts.ReapSuspendedOrphans(ctx, signupreaper.SignupRaceReasonPrefix, time.Now().UTC().Add(time.Hour))
		if err != nil {
			t.Fatalf("reap: %v — the ON DELETE SET NULL cascade onto audit_log must stay permitted", err)
		}
		if n != 1 {
			t.Fatalf("reaped %d accounts, want 1", n)
		}
		requireAuditRow(t, ctx, db, f.reapRow, "signup.orphan", false, false)
	})

	t.Run("UserDeleteNullsActorAndKeepsRow", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
			t.Fatalf("delete user: %v — the ON DELETE SET NULL cascade onto audit_log must stay permitted", err)
		}
		requireAuditRow(t, ctx, db, f.liveRow, "key.mint", true, false)
	})
}

func seedAppendOnlyFixture(t *testing.T, ctx context.Context, db *sql.DB) appendOnlyFixture {
	t.Helper()
	store := postgresstore.New(db)
	f := appendOnlyFixture{accounts: postgresstore.NewAccountStore(store)}
	live, err := f.accounts.Create(ctx, platform.Account{
		Name: "Audit Keep", Slug: "audit-keep", BillingEmail: "keep@audit.example",
		Tier: platform.TierPro, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create live account: %v", err)
	}
	orphan, err := f.accounts.Create(ctx, platform.Account{
		Name: "Audit Orphan", Slug: "audit-orphan", BillingEmail: "orphan@audit.example",
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create orphan account: %v", err)
	}
	if err := f.accounts.Suspend(ctx, orphan.ID, signupreaper.SignupRaceReasonPrefix+" orphan speculative account orphan@audit.example"); err != nil {
		t.Fatalf("suspend orphan: %v", err)
	}
	user, err := postgresstore.NewUserStore(store).CreateUser(ctx, platform.User{
		AccountID: live.ID, Email: "owner@audit.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f.userID, f.liveRow, f.reapRow = user.ID, uuid.New(), uuid.New()
	audit := postgresstore.NewAuditStore(store)
	for _, e := range []platform.AuditEntry{
		{
			ID: f.liveRow, AccountID: live.ID, ActorUserID: user.ID, ActorKind: platform.ActorUser,
			Action: "key.mint", IP: net.ParseIP("203.0.113.7"),
		},
		{ID: f.reapRow, AccountID: orphan.ID, ActorKind: platform.ActorSystem, Action: "signup.orphan"},
	} {
		if err := audit.Append(ctx, e); err != nil {
			t.Fatalf("append %s: %v", e.Action, err)
		}
	}
	return f
}

func requireAppendOnlyRefusal(t *testing.T, name string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want the append-only trigger's 42501 refusal", name, err)
	}
}

// auditSnapshot renders every audit_log row, so any change to any column
// of any row shows up as a string difference.
func auditSnapshot(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)::text FROM audit_log a`).Scan(&s); err != nil {
		t.Fatalf("snapshot audit_log: %v", err)
	}
	return s.String
}

func requireAuditRow(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID, wantAction string, wantAccount, wantActor bool) {
	t.Helper()
	var action string
	var account, actor sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT action, account_id::text, actor_user_id::text FROM audit_log WHERE id = $1`, id).
		Scan(&action, &account, &actor)
	if err != nil {
		t.Fatalf("audit row %s after cascade: %v — the row must survive, only unlinked", id, err)
	}
	if action != wantAction {
		t.Errorf("action = %q, want %q", action, wantAction)
	}
	if account.Valid != wantAccount || actor.Valid != wantActor {
		t.Errorf("account_id set = %v (want %v), actor_user_id set = %v (want %v)",
			account.Valid, wantAccount, actor.Valid, wantActor)
	}
}
