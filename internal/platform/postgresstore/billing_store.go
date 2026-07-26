package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
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

// stripeEventClaimLease bounds how long one delivery may hold the claim
// on a stripe_event_log row before another delivery may take it
// (C3-039). It has to exceed the slowest realistic run of
// `handleStripeWebhook` — which is bounded by the request context and
// does at most a handful of Postgres/Redis writes per key — while
// staying far under Stripe's own retry horizon (retries continue for 3
// days), so a processor killed mid-flight costs at most one lease
// before the next retry re-attempts.
//
// 5 minutes is ~2 orders of magnitude above the p99 handler runtime and
// ~1 order below the first Stripe retry, which is the widest safe gap
// available.
const stripeEventClaimLease = 5 * time.Minute

// stripeEventClaimQuery claims the dedupe row for processing in one
// round-trip, under the advisory lock taken by [AppendStripeEvent].
//
// Three arms, mutually exclusive by construction:
//
//	ins  — no row existed: insert it, claimed.
//	upd  — a row existed with no LIVE claim (never claimed, released by
//	       a failed/dead-lettered attempt, or a lease that expired
//	       because its processor died): re-claim it. F-1322's
//	       reprocessable case.
//	obs  — a row existed that neither arm touched: it is either
//	       finished (processed_at IS NOT NULL) or claimed by a live
//	       delivery. `processed_at` tells the caller which.
//
// The `NOT EXISTS (SELECT 1 FROM ins)` guard on `upd` keeps the two
// write arms disjoint; `obs` fires only when neither wrote.
const stripeEventClaimQuery = `
	WITH ins AS (
		INSERT INTO stripe_event_log
		    (stripe_event_id, type, received_at, payload, claimed_at)
		VALUES ($1, $2, COALESCE(NULLIF($3, '0001-01-01 00:00:00+00'::timestamptz), now()), $4, now())
		ON CONFLICT (stripe_event_id) DO NOTHING
		RETURNING processed_at, TRUE AS claimed
	), upd AS (
		UPDATE stripe_event_log
		   SET claimed_at = now()
		 WHERE stripe_event_id = $1
		   AND processed_at IS NULL
		   AND (claimed_at IS NULL OR claimed_at < now() - make_interval(secs => $5))
		   AND NOT EXISTS (SELECT 1 FROM ins)
		RETURNING processed_at, TRUE AS claimed
	)
	SELECT processed_at, claimed FROM ins
	UNION ALL
	SELECT processed_at, claimed FROM upd
	UNION ALL
	SELECT processed_at, FALSE AS claimed
	  FROM stripe_event_log
	 WHERE stripe_event_id = $1
	   AND NOT EXISTS (SELECT 1 FROM ins)
	   AND NOT EXISTS (SELECT 1 FROM upd)
`

// AppendStripeEvent claims the dedupe row for processing. It returns:
//
//	nil                            — the caller now HOLDS the claim and
//	                                 must process the event.
//	[platform.ErrAlreadyProcessed] — a prior delivery COMPLETED
//	                                 (processed_at IS NOT NULL); skip.
//	[platform.ErrEventInFlight]    — another delivery holds a live
//	                                 claim; the caller must NOT process
//	                                 and must ask Stripe to retry.
//
// F-1322: it originally returned ErrAlreadyProcessed on mere row
// EXISTENCE (any unique violation). A transient failure on the first
// delivery (e.g. a Redis blip in the key-upgrade step) left the row
// inserted with processed_at NULL; every Stripe retry then hit the
// unique violation, was dup-acked 200, and the upgrade work was never
// re-run — a paid customer was silently never upgraded.
//
// C3-039 (audit-2026-07-23): the F-1322 fix made an unfinished row
// reprocessable, which removed the only thing that arbitrated between
// two live deliveries. processed_at is stamped at the END of the
// handler's work, so during the work itself both a fresh insert and an
// existing-unfinished read returned nil and BOTH callers processed the
// same paid event. The claim (migration 0121) is the missing middle
// state; the advisory lock serialises the claim itself, on the same
// `hashtext('<domain>:' || key)` pattern the API-key, webhook and
// price-alert stores already use for their atomic gates.
//
// The lock is xact-scoped, so it covers the CLAIM only, not the
// handler's subsequent work — which is exactly right: holding a
// database lock across an HTTP handler's full body would convert a slow
// upstream into connection-pool exhaustion. The durable claim is what
// spans the work.
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

	tx, err := b.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresstore: AppendStripeEvent %s: begin tx: %w", e.StripeEventID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialise concurrent claims of the SAME event id. Without it the
	// loser of an `ON CONFLICT DO NOTHING` race reads its snapshot from
	// before the winner committed and sees no row at all. The keyspace
	// is prefixed so it cannot collide with 'apikey:' / 'webhook:' /
	// 'pricealert:'.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('stripe_event:' || $1::text))`,
		e.StripeEventID); err != nil {
		return fmt.Errorf("postgresstore: AppendStripeEvent %s: advisory lock: %w", e.StripeEventID, err)
	}

	var processedAt sql.NullTime
	var claimed bool
	if err := tx.QueryRowContext(ctx, stripeEventClaimQuery,
		e.StripeEventID, e.Type, e.ReceivedAt, payload,
		stripeEventClaimLease.Seconds(),
	).Scan(&processedAt, &claimed); err != nil {
		return fmt.Errorf("postgresstore: AppendStripeEvent %s: %w", e.StripeEventID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresstore: AppendStripeEvent %s: commit: %w", e.StripeEventID, err)
	}

	if claimed {
		return nil // this caller owns the row — proceed to process
	}
	if processedAt.Valid {
		return platform.ErrAlreadyProcessed // a prior delivery completed
	}
	// Unfinished, but someone else's claim is still live. Deliberately
	// NOT dup-acked: a 200 here would take the event out of Stripe's
	// retry queue permanently, so if the in-flight processor then died
	// the customer would have paid for nothing. The caller turns this
	// into a retryable response instead.
	return platform.ErrEventInFlight
}

// MarkStripeEventProcessed sets processed_at = now() on the
// dedupe row. No-op when the row doesn't exist (e.g. dedupe
// store cleared mid-flight) — Stripe will retry the original
// event and the AppendStripeEvent path re-creates the row.
//
// It also CLOSES an open dead-letter (C3-016): if an earlier delivery
// gave up with nothing provisioned and a later one completed the work,
// the money-in-nothing-provisioned state is resolved, not still open.
// dead_lettered_at itself is left standing so the incident stays
// visible in history — only the open-set predicate flips.
func (b *BillingStore) MarkStripeEventProcessed(ctx context.Context, stripeEventID string) error {
	if stripeEventID == "" {
		return errors.New("postgresstore: MarkStripeEventProcessed: stripeEventID is empty")
	}
	const q = `
		UPDATE stripe_event_log
		   SET processed_at            = now(),
		       error                   = NULL,
		       dead_letter_resolved_at = CASE
		           WHEN dead_lettered_at IS NOT NULL AND dead_letter_resolved_at IS NULL
		           THEN now() ELSE dead_letter_resolved_at END
		 WHERE stripe_event_id = $1
		   AND processed_at IS NULL
	`
	if _, err := b.s.db.ExecContext(ctx, q, stripeEventID); err != nil {
		return fmt.Errorf("postgresstore: MarkStripeEventProcessed %s: %w", stripeEventID, err)
	}
	return nil
}

// MarkStripeEventDeadLettered stamps the sticky
// money-in-nothing-provisioned state on the dedupe row (C3-016).
//
// processed_at is deliberately left NULL: per the F-1322 contract in
// AppendStripeEvent an unfinished row is REPROCESSABLE, so an operator
// re-sending the event from the Stripe dashboard (after the customer
// signs up, or once the key store is healthy) re-runs the provisioning
// instead of being dup-acked into a no-op. `error` carries the reason
// too so the pre-existing `WHERE error IS NOT NULL` operator query keeps
// finding these.
//
// dead_lettered_at is set once and never re-stamped, so the age of the
// open row is the age of the incident rather than of the last retry.
func (b *BillingStore) MarkStripeEventDeadLettered(ctx context.Context, stripeEventID string, reason platform.DeadLetterReason) error {
	if stripeEventID == "" {
		return errors.New("postgresstore: MarkStripeEventDeadLettered: stripeEventID is empty")
	}
	if reason == "" {
		return errors.New("postgresstore: MarkStripeEventDeadLettered: reason is empty")
	}
	// claimed_at is released (C3-039): this attempt has concluded and
	// provisioned nothing, so an operator re-sending the event from the
	// Stripe dashboard must be able to re-claim it immediately rather
	// than wait out this attempt's lease.
	const q = `
		UPDATE stripe_event_log
		   SET dead_lettered_at   = COALESCE(dead_lettered_at, now()),
		       dead_letter_reason = $2,
		       error              = $2,
		       claimed_at         = NULL
		 WHERE stripe_event_id = $1
	`
	if _, err := b.s.db.ExecContext(ctx, q, stripeEventID, string(reason)); err != nil {
		return fmt.Errorf("postgresstore: MarkStripeEventDeadLettered %s: %w", stripeEventID, err)
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
	// claimed_at is released (C3-039) for the same reason as the
	// dead-letter path: this attempt is over and did not finish, so the
	// next Stripe retry must be able to claim the row without waiting
	// out the dead attempt's lease. Without this a transient failure
	// would re-introduce a (bounded) version of the F-1322 stall.
	const q = `
		UPDATE stripe_event_log
		   SET error      = $2,
		       claimed_at = NULL
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

// CountOpenDeadLetters returns the number of dead-lettered,
// unresolved Stripe events — the C3-016 "money in, nothing
// provisioned" open set. Serves the boot/periodic re-seed of the
// stellarindex_stripe_dead_letters_open gauge so a process restart
// cannot zero away a real backlog (the gauge's Inc at open time only
// covers the current process's lifetime).
func (b *BillingStore) CountOpenDeadLetters(ctx context.Context) (int, error) {
	var n int
	err := b.s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM stripe_event_log
		WHERE dead_lettered_at IS NOT NULL
		  AND dead_letter_resolved_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("postgresstore: CountOpenDeadLetters: %w", err)
	}
	return n, nil
}
