package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/RatesEngine/rates-engine/internal/platform"
)

// BillingStore implements [platform.BillingStore] against the
// `subscriptions` + `stripe_event_log` tables in migration 0027.
//
// F-1227 + F-1231 (audit-2026-05-12): Stripe-event dedupe trio
// (`AppendStripeEvent` / `MarkStripeEventProcessed` /
// `MarkStripeEventFailed`) plus the subscription mirror surface
// (`UpsertSubscription` / `GetActiveSubscriptionForAccount`).
// Webhook handler wiring is in `internal/api/v1/stripe_webhook.go`;
// the customer-facing /v1/account/subscription read path on top of
// `GetActiveSubscriptionForAccount` is a separate small piece of
// work tracked outside the 2026-05-12 audit.
type BillingStore struct{ s *Store }

// NewBillingStore returns the Postgres-backed implementation.
func NewBillingStore(s *Store) *BillingStore { return &BillingStore{s: s} }

// Compile-time interface conformance.
var _ platform.BillingStore = (*BillingStore)(nil)

// AppendStripeEvent inserts the dedupe row. Returns
// [platform.ErrAlreadyProcessed] when the stripe_event_id is
// already present so the webhook handler skips re-processing.
func (b *BillingStore) AppendStripeEvent(ctx context.Context, e platform.StripeEvent) error {
	if e.StripeEventID == "" {
		return errors.New("postgresstore: AppendStripeEvent: StripeEventID is empty")
	}
	payload := e.Payload
	if len(payload) == 0 {
		// jsonb column is NOT NULL — empty object satisfies the
		// schema for events we don't archive the body of.
		payload = []byte(`{}`)
	}
	const q = `
		INSERT INTO stripe_event_log
		    (stripe_event_id, type, received_at, payload)
		VALUES ($1, $2, COALESCE(NULLIF($3, '0001-01-01 00:00:00+00'::timestamptz), now()), $4)
	`
	_, err := b.s.db.ExecContext(ctx, q,
		e.StripeEventID, e.Type, e.ReceivedAt, payload,
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pgErrUniqueViolation {
			return platform.ErrAlreadyProcessed
		}
		return fmt.Errorf("postgresstore: AppendStripeEvent %s: %w", e.StripeEventID, err)
	}
	return nil
}

// MarkStripeEventProcessed sets processed_at = now() on the
// dedupe row. No-op when the row doesn't exist (e.g. dedupe
// store cleared mid-flight) — Stripe will retry the original
// event and the AppendStripeEvent path re-creates the row.
func (b *BillingStore) MarkStripeEventProcessed(ctx context.Context, stripeEventID string) error {
	if stripeEventID == "" {
		return errors.New("postgresstore: MarkStripeEventProcessed: stripeEventID is empty")
	}
	const q = `
		UPDATE stripe_event_log
		   SET processed_at = now(),
		       error        = NULL
		 WHERE stripe_event_id = $1
		   AND processed_at IS NULL
	`
	if _, err := b.s.db.ExecContext(ctx, q, stripeEventID); err != nil {
		return fmt.Errorf("postgresstore: MarkStripeEventProcessed %s: %w", stripeEventID, err)
	}
	return nil
}

// MarkStripeEventFailed records the error on the dedupe row;
// processed_at stays NULL so the next retry triggers a fresh
// attempt. Operators can query
// `SELECT * FROM stripe_event_log WHERE error IS NOT NULL` to
// find chronically-failing events.
func (b *BillingStore) MarkStripeEventFailed(ctx context.Context, stripeEventID, errMsg string) error {
	if stripeEventID == "" {
		return errors.New("postgresstore: MarkStripeEventFailed: stripeEventID is empty")
	}
	const q = `
		UPDATE stripe_event_log
		   SET error = $2
		 WHERE stripe_event_id = $1
	`
	if _, err := b.s.db.ExecContext(ctx, q, stripeEventID, errMsg); err != nil {
		return fmt.Errorf("postgresstore: MarkStripeEventFailed %s: %w", stripeEventID, err)
	}
	return nil
}

// UpsertSubscription mirrors Stripe subscription state into the
// local `subscriptions` table. Idempotent on stripe_subscription_id:
// re-running the same webhook (Stripe at-least-once delivery)
// updates period boundaries + cancel flags without creating
// duplicate rows. F-1231 (audit-2026-05-12).
//
// Wire-up at the Stripe webhook layer requires resolving the
// session's stripe_customer_id → accounts.id (via the unique
// `accounts_stripe_customer_idx`); the per-account UPSERT below
// is the store-layer half of that work.
func (b *BillingStore) UpsertSubscription(ctx context.Context, sub platform.Subscription) error {
	if sub.AccountID == uuid.Nil {
		return errors.New("postgresstore: UpsertSubscription: AccountID is empty")
	}
	if sub.StripeSubscriptionID == "" {
		return errors.New("postgresstore: UpsertSubscription: StripeSubscriptionID is empty")
	}
	if sub.Plan == "" {
		return errors.New("postgresstore: UpsertSubscription: Plan is empty")
	}
	const q = `
		INSERT INTO subscriptions (
		    account_id, stripe_subscription_id, plan,
		    current_period_start, current_period_end,
		    cancel_at_period_end, canceled_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (stripe_subscription_id) DO UPDATE SET
		    plan                 = EXCLUDED.plan,
		    current_period_start = EXCLUDED.current_period_start,
		    current_period_end   = EXCLUDED.current_period_end,
		    cancel_at_period_end = EXCLUDED.cancel_at_period_end,
		    canceled_at          = EXCLUDED.canceled_at,
		    updated_at           = now()
	`
	var canceledAt any
	if !sub.CanceledAt.IsZero() {
		canceledAt = sub.CanceledAt
	}
	if _, err := b.s.db.ExecContext(ctx, q,
		sub.AccountID,
		sub.StripeSubscriptionID,
		string(sub.Plan),
		sub.CurrentPeriodStart,
		sub.CurrentPeriodEnd,
		sub.CancelAtPeriodEnd,
		canceledAt,
	); err != nil {
		return fmt.Errorf("postgresstore: UpsertSubscription %s: %w", sub.StripeSubscriptionID, err)
	}
	return nil
}

// GetActiveSubscriptionForAccount returns the row whose
// current_period_end is in the future for the given account.
// Returns [platform.ErrNotFound] when the account has no active
// subscription (Free tier OR fully cancelled). F-1231.
//
// "Active" matches [platform.Subscription.IsActive]: not canceled
// AND current_period_end > now(). If multiple rows match (a brief
// upgrade-window race), the most-recently-updated row wins.
func (b *BillingStore) GetActiveSubscriptionForAccount(ctx context.Context, accountID uuid.UUID) (platform.Subscription, error) {
	const q = `
		SELECT id, account_id, stripe_subscription_id, plan,
		       current_period_start, current_period_end,
		       cancel_at_period_end, canceled_at,
		       created_at, updated_at
		  FROM subscriptions
		 WHERE account_id = $1
		   AND current_period_end > now()
		   AND (canceled_at IS NULL OR canceled_at > now())
		 ORDER BY updated_at DESC
		 LIMIT 1
	`
	var (
		sub        platform.Subscription
		plan       string
		canceledAt sql.NullTime
	)
	err := b.s.db.QueryRowContext(ctx, q, accountID).Scan(
		&sub.ID,
		&sub.AccountID,
		&sub.StripeSubscriptionID,
		&plan,
		&sub.CurrentPeriodStart,
		&sub.CurrentPeriodEnd,
		&sub.CancelAtPeriodEnd,
		&canceledAt,
		&sub.CreatedAt,
		&sub.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.Subscription{}, platform.ErrNotFound
	}
	if err != nil {
		return platform.Subscription{}, fmt.Errorf("postgresstore: GetActiveSubscriptionForAccount %s: %w", accountID, err)
	}
	sub.Plan = platform.SubscriptionPlan(plan)
	if canceledAt.Valid {
		sub.CanceledAt = canceledAt.Time
	}
	return sub, nil
}
