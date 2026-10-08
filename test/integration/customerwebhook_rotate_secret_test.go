//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestCustomerWebhookRotateSecretKeepsQueue executes migration 0200 up and
// down and the in-place rotation it backs: the key changes on the
// SAME row, the outgoing key is kept until its expiry, and the webhook's
// queued deliveries survive — the delete + recreate it replaces cascaded
// them away.
func TestCustomerWebhookRotateSecretKeepsQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 199)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "rotate")
	hookID := insertRawWebhook(t, ctx, db, acct, "https://hooks.example/rotate")
	applyMigrations(t, dsn)

	webhooks := postgresstore.NewWebhookStore(postgresstore.New(db))
	before, err := webhooks.GetWebhook(ctx, hookID)
	if err != nil {
		t.Fatalf("get pre-0200 webhook: %v", err)
	}
	if before.PreviousSigningKey != nil || !before.PreviousSecretExpiresAt.IsZero() {
		t.Fatalf("pre-0200 webhook reads previous = (%x, %v), want none", before.PreviousSigningKey, before.PreviousSecretExpiresAt)
	}
	oldKey := before.SigningKey

	queued := uuid.New()
	if err := webhooks.EnqueueDelivery(ctx, platform.WebhookDelivery{
		ID: queued, WebhookID: hookID, EventType: string(platform.WebhookEventIncidentSEV1),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	newKey := []byte("wsec_rotated")
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := webhooks.RotateWebhookSecret(ctx, hookID, newKey, expiry); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	after, err := webhooks.GetWebhook(ctx, hookID)
	if err != nil {
		t.Fatalf("get after rotate: %v", err)
	}
	if string(after.SigningKey) != string(newKey) {
		t.Errorf("secret_hash = %q, want the new key", after.SigningKey)
	}
	if string(after.PreviousSigningKey) != string(oldKey) || !after.PreviousSecretExpiresAt.Equal(expiry) {
		t.Errorf("previous = (%x, %v), want the old key %x until %v", after.PreviousSigningKey, after.PreviousSecretExpiresAt, oldKey, expiry)
	}
	deliveries, err := webhooks.ListDeliveries(ctx, hookID, 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].ID != queued || deliveries[0].IsTerminal() {
		t.Errorf("deliveries after rotate = %+v, want the one queued row still pending", deliveries)
	}

	// A second rotation keeps only the most recent outgoing key.
	if err := webhooks.RotateWebhookSecret(ctx, hookID, []byte("wsec_third"), expiry); err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if got, _ := webhooks.GetWebhook(ctx, hookID); string(got.PreviousSigningKey) != string(newKey) {
		t.Errorf("previous after second rotate = %q, want %q", got.PreviousSigningKey, newKey)
	}

	if err := webhooks.RotateWebhookSecret(ctx, uuid.New(), newKey, expiry); !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("rotate of a missing webhook: err = %v, want ErrNotFound", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE customer_webhooks SET previous_secret_expires_at = NULL WHERE id = $1`, hookID); err == nil {
		t.Error("previous_secret without an expiry was accepted, want the pair CHECK to refuse it")
	}

	applyMigrationsUpTo(t, dsn, 199)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'customer_webhooks'
		   AND column_name IN ('previous_secret', 'previous_secret_expires_at')`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("0200 down left %d previous_secret columns in place", cols)
	}
	applyMigrations(t, dsn)
}
