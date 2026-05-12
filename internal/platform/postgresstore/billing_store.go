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
// F-1227 (audit-2026-05-12): the Stripe webhook handler needs the
// `AppendStripeEvent` / `MarkStripeEventProcessed` /
// `MarkStripeEventFailed` triple wired so a delayed Stripe
// re-delivery doesn't silently re-upgrade after a manual
// downgrade. Subscription mirror methods (UpsertSubscription /
// GetActiveSubscriptionForAccount) are stubbed for v1 and will
// land alongside the customer-facing /v1/account/subscription
// surface (F-1231).
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

// UpsertSubscription is the Phase-2 surface that mirrors Stripe
// subscription state into the local `subscriptions` table.
// Stubbed today — F-1231 (audit-2026-05-12) tracks the full
// implementation; until then the webhook handler only updates
// per-key rate limits via UpdateRateLimit.
func (b *BillingStore) UpsertSubscription(ctx context.Context, sub platform.Subscription) error {
	return errors.New("postgresstore: UpsertSubscription not yet implemented (F-1231)")
}

// GetActiveSubscriptionForAccount paired stub with
// UpsertSubscription — see F-1231.
func (b *BillingStore) GetActiveSubscriptionForAccount(ctx context.Context, accountID uuid.UUID) (platform.Subscription, error) {
	return platform.Subscription{}, sql.ErrNoRows
}
