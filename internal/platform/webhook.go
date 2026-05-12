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
// At-rest protection is the Postgres `customer_webhooks` row's
// standard column-encryption posture; the DB column rename is
// tracked separately as a follow-up.
//
// Customer surface: the plaintext key is returned exactly once
// from `POST /v1/dashboard/webhooks` at creation time and never
// served again. Re-rotation requires deleting + recreating the
// webhook so the customer gets a fresh visible key.
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
	// the new plaintext (shown once, not stored).
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
