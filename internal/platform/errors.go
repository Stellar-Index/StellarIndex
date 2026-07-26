package platform

import "errors"

// Sentinel errors for the platform stores. Wrap with %w so
// callers can errors.Is() — never compare error strings.

var (
	// ErrNotFound is returned by every Get*/Consume*/Accept*
	// method when the requested record doesn't exist (or, for
	// magic-link tokens, has already been consumed).
	ErrNotFound = errors.New("platform: not found")

	// ErrTokenExpired is returned by ConsumeMagicLinkToken /
	// AcceptInvite when the row exists but its expires_at is
	// in the past. Distinct from ErrNotFound so the handler
	// can render "this link expired, request a fresh one"
	// rather than the generic "invalid link" message.
	ErrTokenExpired = errors.New("platform: token expired")

	// ErrConflict signals a uniqueness violation (duplicate
	// email, duplicate stripe_customer_id, key_hash collision).
	// Callers handle by surfacing the appropriate user-facing
	// message — typically "an account with that email already
	// exists" for users, or retry-with-new-id for keys.
	ErrConflict = errors.New("platform: conflict")

	// ErrAlreadyProcessed is returned by AppendStripeEvent when a
	// prior delivery of the stripe_event_id COMPLETED. Handlers
	// skip processing and dup-ack (Stripe retried; we already
	// applied the effect).
	ErrAlreadyProcessed = errors.New("platform: stripe event already processed")

	// ErrEventInFlight is returned by AppendStripeEvent when the
	// event is unfinished but another delivery holds a live claim
	// on it (C3-039, audit-2026-07-23). It is emphatically NOT a
	// duplicate-ack: the in-flight processor may still die, so the
	// handler must return a RETRYABLE response and leave the event
	// in Stripe's retry queue. A 200 here would remove the last
	// chance to provision what a customer already paid for.
	ErrEventInFlight = errors.New("platform: stripe event delivery already in flight")

	// ErrLastOwner is returned when an operation would leave an
	// account with zero owner-role users — demotion + deletion
	// both refuse rather than orphan the account.
	ErrLastOwner = errors.New("platform: cannot remove the last owner")

	// ErrWebhookQuotaExceeded is returned by CreateWebhook when
	// the per-account cap is already met. The store enforces the
	// cap atomically (single CTE-gated INSERT) so concurrent
	// requests at the boundary see this error rather than slip
	// through a raceable pre-check. F-1248 (codex audit-2026-05-12).
	ErrWebhookQuotaExceeded = errors.New("platform: webhook quota exceeded")

	// ErrAPIKeyQuotaExceeded mirrors ErrWebhookQuotaExceeded for
	// the dashboard API-key store. F-1257 (codex audit-2026-05-12):
	// the 25-active-key/account cap is enforced atomically inside
	// the INSERT to defeat the same race the webhook cap had.
	ErrAPIKeyQuotaExceeded = errors.New("platform: api key quota exceeded")

	// ErrPriceAlertQuotaExceeded mirrors ErrWebhookQuotaExceeded for
	// the price-alert store (BACKLOG #60). The per-account cap is
	// enforced atomically inside the CreatePriceAlert INSERT (same
	// advisory-lock + CTE-gate shape) so concurrent creates at the
	// boundary see this error rather than slip through a raceable
	// pre-check.
	ErrPriceAlertQuotaExceeded = errors.New("platform: price alert quota exceeded")
)
