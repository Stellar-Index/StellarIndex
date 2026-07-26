package platform

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SubscriptionPlan identifies which Stripe-side plan an account
// is on. Distinct from [Tier] (runtime authorisation tier);
// SubscriptionPlan reflects what they're paying for.
type SubscriptionPlan string

const (
	PlanStarter    SubscriptionPlan = "starter"
	PlanPro        SubscriptionPlan = "pro"
	PlanBusiness   SubscriptionPlan = "business"
	PlanEnterprise SubscriptionPlan = "enterprise"
)

// Subscription mirrors Stripe subscription state. One active
// row per account (current_period_end > now()); historical
// rows roll over via UPSERT on stripe_subscription_id.
type Subscription struct {
	ID                   uuid.UUID
	AccountID            uuid.UUID
	StripeSubscriptionID string
	Plan                 SubscriptionPlan
	CurrentPeriodStart   time.Time
	CurrentPeriodEnd     time.Time
	CancelAtPeriodEnd    bool
	CanceledAt           time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// IsActive reports whether the subscription should be granting
// the customer their plan's capabilities right now.
func (s Subscription) IsActive(now time.Time) bool {
	if !s.CanceledAt.IsZero() && !now.Before(s.CanceledAt) {
		return false
	}
	return now.Before(s.CurrentPeriodEnd)
}

// StripeEvent is the deduplication / audit row for inbound
// Stripe webhook events. Idempotency: Stripe retries on
// non-200; we look up by stripe_event_id and skip processing
// if processed_at is non-zero.
type StripeEvent struct {
	StripeEventID string
	Type          string
	ReceivedAt    time.Time
	ProcessedAt   time.Time // zero = received but not yet handled
	Error         string
	Payload       json.RawMessage
}

// BillingStore persists [Subscription] and [StripeEvent].
type BillingStore interface {
	// UpsertSubscription writes the Stripe-derived state. Called
	// from webhook handlers; idempotent on stripe_subscription_id.
	UpsertSubscription(ctx context.Context, s Subscription) error

	// GetActiveSubscriptionForAccount returns the row whose
	// current_period_end is in the future. ErrNotFound when the
	// account has no active subscription (either Free tier or
	// fully cancelled).
	GetActiveSubscriptionForAccount(ctx context.Context, accountID uuid.UUID) (Subscription, error)

	// AppendStripeEvent CLAIMS the dedupe row for processing. nil
	// means the caller now holds the claim and must process the
	// event. Returns ErrAlreadyProcessed when a prior delivery
	// COMPLETED (skip and dup-ack), and ErrEventInFlight when
	// another delivery holds a live claim (do NOT process; return a
	// retryable response so the event stays in Stripe's retry
	// queue). C3-039: the claim is what stops two concurrent
	// deliveries of one paid event from both being processed —
	// processed_at cannot, because it is only stamped once the work
	// is done.
	AppendStripeEvent(ctx context.Context, e StripeEvent) error

	// MarkStripeEventProcessed sets processed_at to now() on a
	// previously-appended event. Called after the handler
	// successfully applied the event's effect.
	MarkStripeEventProcessed(ctx context.Context, stripeEventID string) error

	// MarkStripeEventFailed records the error for diagnosis;
	// processed_at stays zero so the next retry triggers a
	// fresh attempt.
	MarkStripeEventFailed(ctx context.Context, stripeEventID string, err string) error

	// MarkStripeEventDeadLettered records that the event was
	// acknowledged but this system provisioned NOTHING for it —
	// money landed at Stripe and no credential exists to show for
	// it. Sticky: processed_at stays zero so a manual re-send can
	// still complete the work, and only
	// [BillingStore.MarkStripeEventProcessed] closes the row (by
	// stamping dead_letter_resolved_at).
	//
	// C3-016 (audit-2026-07-23). Distinct from MarkStripeEventFailed,
	// which marks a transient, still-retrying failure and whose
	// `error` column the processed-mark clears.
	MarkStripeEventDeadLettered(ctx context.Context, stripeEventID string, reason DeadLetterReason) error
}

// DeadLetterReason classifies why a Stripe event provisioned nothing.
// The value is persisted verbatim into
// `stripe_event_log.dead_letter_reason`, so an alert can name the class
// without parsing free text.
type DeadLetterReason string

const (
	// DeadLetterNoKeys — the paid checkout session named an identifier
	// that holds no API keys ("customer paid but never signed up").
	// Retrying the webhook cannot help; a human must reconcile
	// (refund, or provision after the customer signs up).
	DeadLetterNoKeys DeadLetterReason = "no_keys_for_identifier"

	// DeadLetterKeyUpgradeFailed — the customer HAS keys but every
	// per-key budget update failed, so the paid tier was never
	// applied. Usually transient (key-store outage); the row stays
	// reprocessable so a re-delivery completes it.
	DeadLetterKeyUpgradeFailed DeadLetterReason = "key_upgrade_failed"
)
