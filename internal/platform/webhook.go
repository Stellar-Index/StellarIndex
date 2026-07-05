package platform

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// WebhookEventType is the closed set of customer-deliverable event
// kinds. Adding a new type requires a corresponding entry in the
// /v1/account/webhooks/events customer-facing docs. The events
// column is text[] so future values can be tolerated by readers
// that haven't been updated.
type WebhookEventType string

const (
	// WebhookEventIncidentSEV1 fires when a SEV-1 incident has been
	// declared (status-page page-level event). Triggered by
	// Alertmanager via an internal inbound-webhook receiver that
	// fans the event out to every customer subscribed to it.
	// F-1270 (audit-2026-05-12).
	WebhookEventIncidentSEV1 WebhookEventType = "incident.sev1"

	// WebhookEventIncidentResolved fires when a previously-active
	// incident has cleared. Same incident_id as the corresponding
	// SEV-1 event so consumers can correlate.
	WebhookEventIncidentResolved WebhookEventType = "incident.resolved"

	// WebhookEventAnomalyFreeze fires when the aggregator engages a
	// freeze on a (asset, quote) the customer cares about.
	WebhookEventAnomalyFreeze WebhookEventType = "anomaly.freeze"

	// WebhookEventDivergenceFiring fires when a price-divergence
	// warning starts or clears. Body carries `firing: true|false`.
	WebhookEventDivergenceFiring WebhookEventType = "divergence.firing"

	// WebhookEventPriceAlert fires when one of the account's
	// registered price-threshold alerts crosses its condition
	// (BACKLOG #60). Unlike the operational events above, this is a
	// PER-ACCOUNT event: the aggregator's price-alert evaluator
	// enqueues it only to the owning account's subscribed webhooks
	// (via ListWebhooksForAccount, not the global fan-out), so one
	// account's alerts never reach another's. Body shape:
	// PriceAlertWebhookPayload (alert_id, pair, condition, threshold,
	// observed_price, bucket, at).
	WebhookEventPriceAlert WebhookEventType = "price.alert"
)

// CustomerWebhook is an outbound HTTPS endpoint a customer
// registers to receive event notifications. Stripe-shape:
// signed deliveries (HMAC-SHA-256 of payload), exponential
// retry over 72h.
//
// F-1244 (codex audit-2026-05-12): the persisted signing-key
// field is misnamed `SecretHash` for historical reasons. Despite
// the name, the value is the LITERAL HMAC key — the delivery
// worker calls `hmac.New(sha256.New, wh.SecretHash)` directly.
// A hash-only design isn't possible without changing the wire
// protocol (the receiver needs the same shared secret to verify).
//
// At-rest protection: the bytes are persisted as `bytea` in the
// `customer_webhooks.secret_hash` column WITHOUT application-
// layer encryption. Operators rely on the database's own at-rest
// encryption (Postgres TDE / cloud-provider disk encryption) +
// the Redis ACL lockdown (F-1254) for defence in depth. The
// audit (F-1244 codex 2026-05-13) explicitly called out an
// earlier docstring that claimed a "standard column-encryption
// posture"; that prose was misleading because no per-row
// envelope-encryption layer ships in this repo. The current
// posture is honest: the row IS recoverable by anyone with
// SELECT on `customer_webhooks`.
//
// Customer surface: the plaintext key is returned exactly once
// from `POST /v1/dashboard/webhooks` at creation time and never
// served back through any API surface again. Re-rotation
// requires deleting + recreating the webhook so the customer
// gets a fresh visible key — there is NO "rotate-in-place"
// path because every such design either re-exposes the old
// secret to the operator-side audit log or breaks the
// "exactly-once visibility" property the audit required.
type CustomerWebhook struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Name      string
	URL       string
	// SecretHash carries the HMAC signing key bytes (NOT a hash —
	// see struct doc above). Renamed-but-not-yet-migrated; kept
	// as `SecretHash` to avoid a Postgres column rename in the
	// same change-set that introduces the truthful comment.
	SecretHash []byte
	Events     []string
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// WebhookDelivery is one attempt to deliver an event to a
// customer webhook. We track every attempt (delivered or
// failed) so the dashboard can render the delivery log.
type WebhookDelivery struct {
	ID                 uuid.UUID
	WebhookID          uuid.UUID
	EventType          string
	Payload            json.RawMessage
	AttemptCount       int
	NextAttemptAt      time.Time // zero = no further retry scheduled (delivered or exhausted)
	DeliveredAt        time.Time
	LastError          string
	LastResponseStatus int
	CreatedAt          time.Time
}

// IsTerminal reports whether the delivery has stopped retrying:
// either it was delivered, or the retry budget is exhausted
// (signalled by NextAttemptAt being zero with DeliveredAt also
// zero — caller distinguishes by checking DeliveredAt).
func (d WebhookDelivery) IsTerminal() bool {
	return !d.DeliveredAt.IsZero() || d.NextAttemptAt.IsZero()
}

// WebhookStore persists [CustomerWebhook] and [WebhookDelivery].
type WebhookStore interface {
	// CreateWebhook registers a new outbound endpoint, enforcing
	// the per-account `maxPerAccount` cap atomically. Returns
	// [ErrWebhookQuotaExceeded] when the cap is met — the cap
	// check + insert happen in a single SQL statement so
	// concurrent callers can't both pass a pre-check and each
	// append a row past the cap. F-1248 (codex audit-2026-05-12).
	CreateWebhook(ctx context.Context, w CustomerWebhook, maxPerAccount int) (CustomerWebhook, error)

	// GetWebhook by ID.
	GetWebhook(ctx context.Context, id uuid.UUID) (CustomerWebhook, error)

	// ListWebhooksForAccount returns every webhook (enabled +
	// disabled) for the account.
	ListWebhooksForAccount(ctx context.Context, accountID uuid.UUID) ([]CustomerWebhook, error)

	// ListWebhooksSubscribedTo returns every enabled webhook
	// (across all accounts) subscribed to `eventType`. Used by
	// the fan-out service to enqueue one delivery per subscriber
	// when a product event fires. F-1249 (codex audit-2026-05-12).
	ListWebhooksSubscribedTo(ctx context.Context, eventType WebhookEventType) ([]CustomerWebhook, error)

	// UpdateWebhook writes mutable fields (name, url, events,
	// enabled). Secret rotation is a separate explicit method.
	UpdateWebhook(ctx context.Context, w CustomerWebhook) error

	// RotateWebhookSecret replaces the signing secret. Returns
	// the new plaintext.
	//
	// F-1244 (codex audit-2026-05-13): the prior docstring said
	// "shown once, not stored". That was misleading — like create,
	// the new secret IS persisted as the canonical
	// `customer_webhooks.secret_hash` bytea so the delivery
	// worker can sign future requests. "Shown once" is the
	// customer-facing visibility property: the plaintext is
	// returned by the API call exactly once and never served
	// back through any subsequent read. The recoverability
	// posture is identical to Create — see the
	// [CustomerWebhook] struct doc for the at-rest model.
	//
	// Note: as of 2026-05-13 the Postgres implementation of
	// RotateWebhookSecret is intentionally not wired (callers
	// rotate by deleting + recreating). The interface is kept
	// in the contract so the v2 dashboard can plug the in-place
	// rotation path without re-shaping the store boundary.
	RotateWebhookSecret(ctx context.Context, id uuid.UUID) (newSecret string, err error)

	// DeleteWebhook hard-deletes (cascades to deliveries).
	DeleteWebhook(ctx context.Context, id uuid.UUID) error

	// AppendDelivery records one attempt. Called by the
	// delivery worker after each send.
	AppendDelivery(ctx context.Context, d WebhookDelivery) (WebhookDelivery, error)

	// UpdateDelivery rewrites the attempt-state fields after a
	// retry. Idempotent.
	UpdateDelivery(ctx context.Context, d WebhookDelivery) error

	// ListDeliveries returns recent attempts for the webhook,
	// most-recent first. Used by the dashboard delivery log.
	ListDeliveries(ctx context.Context, webhookID uuid.UUID, limit int) ([]WebhookDelivery, error)

	// ─── Worker-side queue surface (F-1270 audit-2026-05-12) ─────

	// EnqueueDelivery inserts one pending delivery row keyed off
	// an existing webhook. The worker then drains the queue via
	// ListPendingDeliveries. attempt_count starts at 0;
	// NextAttemptAt zero is normalised to "now" so the first
	// poll picks it up immediately.
	EnqueueDelivery(ctx context.Context, d WebhookDelivery) error

	// ListPendingDeliveries returns up to `limit` deliveries
	// whose next_attempt_at is in the past, ordered FIFO. The
	// delivery worker calls this on each poll tick.
	ListPendingDeliveries(ctx context.Context, limit int) ([]WebhookDelivery, error)

	// MarkDelivered records a successful POST: stamps
	// delivered_at=now, clears next_attempt_at, records the
	// response_status. Idempotent.
	MarkDelivered(ctx context.Context, id uuid.UUID, responseStatus int) error

	// MarkAttemptFailed records a failed POST + schedules the
	// next retry. nextAttemptAt zero = permanently failed (drops
	// out of the pending-listing predicate; consumers see the
	// row via ListDeliveries with delivered_at unset).
	MarkAttemptFailed(ctx context.Context, id uuid.UUID, errMsg string, responseStatus int, nextAttemptAt time.Time) error
}
