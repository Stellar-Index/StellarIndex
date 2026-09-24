//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Q146 — the store half of the sessions and webhook_deliveries
// retention sweeps. Both predicates must be right in both directions:
// every terminal row past the cutoff goes, and no live row (an active
// session, a delivery still queued or parked) goes at any age.
func TestPlatformRetentionReaper(t *testing.T) {
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
	users := postgresstore.NewUserStore(pg)
	webhooks := postgresstore.NewWebhookStore(pg)

	t.Run("SweepEndedSessions", func(t *testing.T) {
		assertSessionSweep(t, ctx, db, accounts, users)
	})
	t.Run("SweepFinishedDeliveries", func(t *testing.T) {
		assertDeliverySweep(t, ctx, db, accounts, webhooks)
	})
}

type retentionFixture struct {
	tag        string
	wantReaped bool
}

func assertSessionSweep(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, users *postgresstore.UserStore,
) {
	t.Helper()
	acct, err := accounts.Create(ctx, platform.Account{
		Name: "Retention Co", Slug: "retention-co",
		BillingEmail: "billing@retention.example",
		Tier:         platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "owner@retention.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Offsets in days relative to now; the cutoff is now - 90d.
	seeds := []struct {
		retentionFixture
		expiresDays int
		revokedDays *int
	}{
		{retentionFixture{"expired-long-ago", true}, -100, nil},
		{retentionFixture{"revoked-long-ago", true}, 20, intPtr(-95)},
		{retentionFixture{"live", false}, 20, nil},
		{retentionFixture{"expired-recently", false}, -10, nil},
		{retentionFixture{"revoked-recently", false}, 20, intPtr(-5)},
	}
	for i, s := range seeds {
		var revoked any
		if s.revokedDays != nil {
			revoked = time.Now().UTC().AddDate(0, 0, *s.revokedDays)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sessions (token_hash, user_id, expires_at, revoked_at,
			                       ip_first_seen, ip_last_seen, user_agent, created_at)
			 VALUES ($1, $2, now() + make_interval(days => $3), $4,
			         '203.0.113.7', '203.0.113.7', $5, now() - interval '200 days')`,
			[]byte(fmt.Sprintf("retention-session-token-hash-%02d", i)),
			user.ID, s.expiresDays, revoked, s.tag); err != nil {
			t.Fatalf("seed session %s: %v", s.tag, err)
		}
	}

	deleted, err := users.SweepEndedSessions(ctx, time.Now().UTC().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("SweepEndedSessions: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the two sessions ended beyond retention)", deleted)
	}
	fixtures := make([]retentionFixture, 0, len(seeds))
	for _, s := range seeds {
		fixtures = append(fixtures, s.retentionFixture)
	}
	assertReaped(t, ctx, db, `SELECT EXISTS (SELECT 1 FROM sessions WHERE user_agent = $1)`, fixtures)
}

func assertDeliverySweep(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	_, hookID := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://retention.example/hook")

	// Ages in days; the cutoff is now - 30d.
	seeds := []struct {
		retentionFixture
		createdDays int
		delivered   bool
		nextAttempt bool
	}{
		{retentionFixture{"delivered-old", true}, -40, true, false},
		{retentionFixture{"dead-lettered-old", true}, -40, false, false},
		{retentionFixture{"queued-old", false}, -40, false, true},
		{retentionFixture{"delivered-recent", false}, -5, true, false},
		{retentionFixture{"dead-lettered-recent", false}, -5, false, false},
	}
	for _, s := range seeds {
		var deliveredAt, nextAttemptAt any
		if s.delivered {
			deliveredAt = time.Now().UTC().AddDate(0, 0, s.createdDays)
		}
		if s.nextAttempt {
			nextAttemptAt = time.Now().UTC().Add(-time.Minute)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO webhook_deliveries
			     (webhook_id, event_type, payload, attempt_count,
			      next_attempt_at, delivered_at, created_at)
			 VALUES ($1, $2, $3, 1, $4, $5,
			         now() + make_interval(days => $6))`,
			hookID, string(killSwitchEvent), fmt.Sprintf(`{"tag":%q}`, s.tag),
			nextAttemptAt, deliveredAt, s.createdDays); err != nil {
			t.Fatalf("seed delivery %s: %v", s.tag, err)
		}
	}

	deleted, err := webhooks.SweepFinishedDeliveries(ctx, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("SweepFinishedDeliveries: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the two finished deliveries beyond retention)", deleted)
	}
	fixtures := make([]retentionFixture, 0, len(seeds))
	for _, s := range seeds {
		fixtures = append(fixtures, s.retentionFixture)
	}
	assertReaped(t, ctx, db,
		`SELECT EXISTS (SELECT 1 FROM webhook_deliveries WHERE payload->>'tag' = $1)`, fixtures)
}

func assertReaped(t *testing.T, ctx context.Context, db *sql.DB, existsQ string, fixtures []retentionFixture) {
	t.Helper()
	var wrong []string
	for _, f := range fixtures {
		var present bool
		if err := db.QueryRowContext(ctx, existsQ, f.tag).Scan(&present); err != nil {
			t.Fatalf("check %s: %v", f.tag, err)
		}
		if present == f.wantReaped {
			wrong = append(wrong, fmt.Sprintf("%s(wantReaped=%v)", f.tag, f.wantReaped))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("retention sweep got these rows wrong: %s", strings.Join(wrong, ", "))
	}
}

func intPtr(n int) *int { return &n }
