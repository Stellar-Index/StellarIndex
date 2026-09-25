//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// GH-968 — EnqueueDelivery dropped the caller's delivery ID and nothing
// else was unique, so re-running `stellarindex-ops emit-incident` after a
// partial fan-out queued a second copy, under a fresh delivery id, for
// every subscriber the first run had already reached. This executes the
// INSERT … ON CONFLICT path and the fan-out re-run against real Postgres.
func TestCustomerWebhookIdempotentEnqueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	t.Run("a repeated delivery id inserts nothing and says so", func(t *testing.T) {
		_, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-direct.example/hook")
		_, otherHook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-other.example/hook")
		d := dueDelivery(hook, "idem-direct")
		d.ID = uuid.New()
		if err := webhooks.EnqueueDelivery(ctx, d); err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		if err := webhooks.EnqueueDelivery(ctx, d); !errors.Is(err, platform.ErrDeliveryAlreadyEnqueued) {
			t.Fatalf("second enqueue err = %v, want ErrDeliveryAlreadyEnqueued", err)
		}
		collide := dueDelivery(otherHook, "idem-collide")
		collide.ID = d.ID
		err := webhooks.EnqueueDelivery(ctx, collide)
		if err == nil || errors.Is(err, platform.ErrDeliveryAlreadyEnqueued) ||
			errors.Is(err, postgresstore.ErrWebhookAccountInactive) {
			t.Fatalf("id reused on another webhook: err = %v, want a collision error", err)
		}
		if got := queuedFor(t, ctx, db, hook); got != 1 {
			t.Errorf("rows for webhook = %d, want 1", got)
		}
		if got := queuedFor(t, ctx, db, otherHook); got != 0 {
			t.Errorf("rows for the colliding webhook = %d, want 0", got)
		}
	})

	t.Run("a zero id still gets a fresh row each time", func(t *testing.T) {
		_, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-nil.example/hook")
		for range 2 {
			if err := webhooks.EnqueueDelivery(ctx, dueDelivery(hook, "idem-nil")); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}
		if got := queuedFor(t, ctx, db, hook); got != 2 {
			t.Errorf("rows = %d, want 2", got)
		}
	})

	t.Run("a fan-out re-run reaches only the subscriber it missed", func(t *testing.T) {
		assertFanoutRerunIsIdempotent(t, ctx, db, accounts, webhooks)
	})
}

func assertFanoutRerunIsIdempotent(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	// Only this sub-test's hooks may be subscribed to the event, so give
	// every earlier hook a different event.
	if _, err := db.ExecContext(ctx, `UPDATE customer_webhooks SET events = ARRAY['price.alert']`); err != nil {
		t.Fatalf("unsubscribe earlier hooks: %v", err)
	}
	_, reached := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-reached.example/hook")
	_, missed := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://idem-missed.example/hook")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const key = "idem-incident@2026-09-01T10:00:00Z"
	flaky := &failOnceFor{WebhookStore: webhooks, webhookID: missed}
	first, err := customerwebhook.NewFanout(flaky, logger).
		PublishOnce(ctx, killSwitchEvent, key, []byte(`{"at":"run-1"}`))
	if err == nil || first.Enqueued != 1 || first.Failed != 1 {
		t.Fatalf("first run = %+v, %v; want Enqueued 1, Failed 1 and an error", first, err)
	}
	second, err := customerwebhook.NewFanout(webhooks, logger).
		PublishOnce(ctx, killSwitchEvent, key, []byte(`{"at":"run-2"}`))
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if second.Enqueued != 1 || second.AlreadyEnqueued != 1 || second.Failed != 0 {
		t.Errorf("re-run = %+v, want {Enqueued:1 AlreadyEnqueued:1 Failed:0}", second)
	}
	for name, hook := range map[string]uuid.UUID{"reached": reached, "missed": missed} {
		if got := queuedFor(t, ctx, db, hook); got != 1 {
			t.Errorf("%s webhook has %d rows, want exactly 1", name, got)
		}
	}
}

// failOnceFor fails the first EnqueueDelivery for one webhook with a
// transient-looking error, as a dropped connection mid-fan-out would.
type failOnceFor struct {
	*postgresstore.WebhookStore
	webhookID uuid.UUID
	failed    bool
}

func (f *failOnceFor) EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error {
	if d.WebhookID == f.webhookID && !f.failed {
		f.failed = true
		return errors.New("injected: connection reset")
	}
	return f.WebhookStore.EnqueueDelivery(ctx, d)
}

func queuedFor(t *testing.T, ctx context.Context, db *sql.DB, webhookID uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1`, webhookID).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}
