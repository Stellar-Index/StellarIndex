//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

const dupWebhookURL = "https://hooks.example/stellarindex"

// TestCustomerWebhooksURLUnique pins migration 0180 (GH #828): one webhook
// per (account, url), surfaced by the store as platform.ErrConflict; the
// migration refuses — rather than merges or deletes — pre-existing
// duplicates; and a quota of 0 (TierAnon) admits nothing.
func TestCustomerWebhooksURLUnique(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 179)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)
	acct := uniqueURLAccount(t, ctx, accounts, "a")

	t.Run("MigrationRefusesExistingDuplicates", func(t *testing.T) {
		first := insertRawWebhook(t, ctx, db, acct, dupWebhookURL)
		second := insertRawWebhook(t, ctx, db, acct, dupWebhookURL)
		err := execUpMigration(t, ctx, db, "0180_customer_webhooks_account_url_unique.up.sql")
		if err == nil || !strings.Contains(err.Error(), "0180:") {
			t.Fatalf("0180 over duplicates: err = %v, want its pre-flight refusal", err)
		}
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM customer_webhooks WHERE id IN ($1, $2)`, first, second).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 2 {
			t.Fatalf("refused migration left %d of the 2 duplicate rows, want both untouched", n)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM customer_webhooks WHERE id = $1`, second); err != nil {
			t.Fatalf("resolve duplicate: %v", err)
		}
	})

	applyMigrations(t, dsn)

	t.Run("StoreMapsDuplicateToConflict", func(t *testing.T) {
		_, err := webhooks.CreateWebhook(ctx, uniqueURLHook(acct, dupWebhookURL), 10)
		if !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("second create of the same url: err = %v, want ErrConflict", err)
		}
		other := uniqueURLAccount(t, ctx, accounts, "b")
		if _, err := webhooks.CreateWebhook(ctx, uniqueURLHook(other, dupWebhookURL), 10); err != nil {
			t.Fatalf("same url on a different account: %v, want allowed", err)
		}
		sibling, err := webhooks.CreateWebhook(ctx, uniqueURLHook(acct, "https://hooks.example/other"), 10)
		if err != nil {
			t.Fatalf("create sibling: %v", err)
		}
		sibling.URL = dupWebhookURL
		if err := webhooks.UpdateWebhook(ctx, sibling); !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("update onto a sibling's url: err = %v, want ErrConflict", err)
		}
	})

	t.Run("ZeroQuotaAdmitsNothing", func(t *testing.T) {
		anon := uniqueURLAccount(t, ctx, accounts, "anon")
		_, err := webhooks.CreateWebhook(ctx, uniqueURLHook(anon, "https://hooks.example/anon"), 0)
		if !errors.Is(err, platform.ErrWebhookQuotaExceeded) {
			t.Fatalf("maxPerAccount=0: err = %v, want ErrWebhookQuotaExceeded", err)
		}
		listed, err := webhooks.ListWebhooksForAccount(ctx, anon)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != 0 {
			t.Fatalf("maxPerAccount=0 stored %d webhooks, want 0", len(listed))
		}
	})
}

func uniqueURLAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, tag string) uuid.UUID {
	t.Helper()
	suffix := tag + "-" + strings.ToLower(uuid.New().String()[:8])
	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "Webhook URL " + suffix,
		Slug:         "wu-" + suffix,
		BillingEmail: "wu-" + suffix + "@k.example",
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return acct.ID
}

func uniqueURLHook(accountID uuid.UUID, url string) platform.CustomerWebhook {
	signing := sha256.Sum256([]byte("url-unique-signing-material"))
	return platform.CustomerWebhook{
		AccountID:  accountID,
		Name:       "hook",
		URL:        url,
		SecretHash: signing[:],
		Events:     []string{string(platform.WebhookEventIncidentSEV1)},
		Enabled:    true,
	}
}

// insertRawWebhook bypasses the store: the pre-0180 schema is the only
// place a duplicate can exist.
func insertRawWebhook(t *testing.T, ctx context.Context, db *sql.DB, accountID uuid.UUID, url string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO customer_webhooks (account_id, name, url, secret_hash, events)
		VALUES ($1, 'dup', $2, '\x00'::bytea, ARRAY['incident.sev1'])
		RETURNING id`, accountID, url).Scan(&id); err != nil {
		t.Fatalf("insert raw webhook: %v", err)
	}
	return id
}

// execUpMigration runs one up file on a dedicated connection and rolls
// back whatever transaction a failure leaves open.
func execUpMigration(t *testing.T, ctx context.Context, db *sql.DB, name string) error {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_, execErr := conn.ExecContext(ctx, string(body))
	if execErr != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
	}
	return execErr
}
