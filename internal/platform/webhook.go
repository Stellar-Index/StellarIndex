package platform

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// WebhookEventType is the closed set of customer-deliverable event kinds; a new type needs an entry in the customer-facing events docs.
type WebhookEventType string

const (
	// WebhookEventIncidentSEV1 fires when a SEV-1 incident has been
	// declared (status-page page-level event). Triggered by
	// Alertmanager via an internal inbound-webhook receiver that
	// fans the event out to every customer subscribed to it.
	WebhookEventIncidentSEV1 WebhookEventType = "incident.sev1"

	// WebhookEventIncidentResolved fires when a previously-active
	// incident has cleared. Same incident_id as the corresponding
	// SEV-1 event so consumers can correlate.
	WebhookEventIncidentResolved WebhookEventType = "incident.resolved"

	// WebhookEventAnomalyFreeze fires when the aggregator engages a
	// freeze on an (asset, quote). It goes to EVERY subscribed webhook:
	// there is no per-account pair filter.
	WebhookEventAnomalyFreeze WebhookEventType = "anomaly.freeze"

	// WebhookEventDivergenceFiring fires when a price-divergence
	// warning starts; a clear sends nothing. Body is DivergenceFiringWebhookPayload.
	WebhookEventDivergenceFiring WebhookEventType = "divergence.firing"

	// WebhookEventPriceAlert fires once per crossing of a registered price alert; PER-ACCOUNT, enqueued only to the owning account's webhooks (ListWebhooksForAccount), never the global fan-out.
	WebhookEventPriceAlert WebhookEventType = "price.alert"
)

// WebhookEventTypes returns every [WebhookEventType] in declaration order.
// It is the one membership list: subscription validation, metric seeding
// and their guards all read it, and TestWebhookEventTypesListsEveryConstant
// fails when a constant above is missing from it.
func WebhookEventTypes() []WebhookEventType {
	return []WebhookEventType{
		WebhookEventIncidentSEV1,
		WebhookEventIncidentResolved,
		WebhookEventAnomalyFreeze,
		WebhookEventDivergenceFiring,
		WebhookEventPriceAlert,
	}
}

// IsWebhookEventType reports whether s names a member of [WebhookEventTypes].
func IsWebhookEventType(s string) bool {
	for _, e := range WebhookEventTypes() {
		if string(e) == s {
			return true
		}
	}
	return false
}

// CustomerWebhook is an outbound HTTPS endpoint for event notifications, signed with HMAC-SHA-256 and retried with exponential backoff.
// The signing key is the LITERAL HMAC key (the receiver needs the same secret), sealed at rest with [WebhookKeySealer] into `signing_key_sealed`; the legacy `secret_hash` holds a raw key only for rows written without a seal key.
// Each plaintext key is returned exactly once (creation, rotate-secret) and never served again.
type CustomerWebhook struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Name      string
	URL       string
	// SigningKey is the plaintext HMAC key: set on create and by
	// GetWebhook, nil from the list reads.
	SigningKey []byte
	// PreviousSigningKey is the key held before the last rotation, also signed with until PreviousSecretExpiresAt; set by GetWebhook only, nil when no overlap is open.
	PreviousSigningKey      []byte
	PreviousSecretExpiresAt time.Time
	Events                  []string
	Enabled                 bool
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// ActivePreviousSigningKey returns PreviousSigningKey while its overlap
// window is open at now, else nil.
func (w CustomerWebhook) ActivePreviousSigningKey(now time.Time) []byte {
	if len(w.PreviousSigningKey) == 0 || !now.Before(w.PreviousSecretExpiresAt) {
		return nil
	}
	return w.PreviousSigningKey
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

// ErrDeliveryAlreadyEnqueued reports an EnqueueDelivery whose ID is already
// queued for that webhook: the event reached the subscriber on an earlier
// attempt, so nothing was inserted and nothing was lost.
var ErrDeliveryAlreadyEnqueued = errors.New("platform: webhook delivery already enqueued")

// WebhookStore persists [CustomerWebhook] and [WebhookDelivery].
type WebhookStore interface {
	// CreateWebhook registers an endpoint, enforcing `maxPerAccount` atomically in one SQL statement ([ErrWebhookQuotaExceeded]); a cap <= 0 admits nothing, and a duplicate w.URL on the account returns [ErrConflict].
	CreateWebhook(ctx context.Context, w CustomerWebhook, maxPerAccount int) (CustomerWebhook, error)

	// GetWebhook by ID, with SigningKey unsealed. A key that cannot
	// be unsealed is an error wrapping [ErrWebhookKeyUnsealable]
	// returned beside the row's other fields, so key-free callers
	// (ownership checks, edits, deletes) can still use them.
	GetWebhook(ctx context.Context, id uuid.UUID) (CustomerWebhook, error)

	// ListWebhooksForAccount returns every webhook (enabled +
	// disabled) for the account, without SigningKey.
	ListWebhooksForAccount(ctx context.Context, accountID uuid.UUID) ([]CustomerWebhook, error)

	// ListWebhooksSubscribedTo returns every enabled webhook
	// (across all accounts) subscribed to `eventType`, without
	// SigningKey. Used by the fan-out service to enqueue one
	// delivery per subscriber when a product event fires.
	ListWebhooksSubscribedTo(ctx context.Context, eventType WebhookEventType) ([]CustomerWebhook, error)

	// UpdateWebhook writes mutable fields (name, url, events,
	// enabled). Secret rotation is a separate explicit method.
	// Returns [ErrConflict] when the new url duplicates another of
	// the account's webhooks.
	UpdateWebhook(ctx context.Context, w CustomerWebhook) error

	// RotateWebhookSecret makes newSecret the signing key IN PLACE, keeping the old one valid until previousExpiresAt (a rotation inside an open overlap replaces the older previous key); both are stored like create stores them, [ErrNotFound] if absent.
	RotateWebhookSecret(ctx context.Context, id uuid.UUID, newSecret []byte, previousExpiresAt time.Time) error

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

	// ─── Worker-side queue surface ─────

	// EnqueueDelivery inserts one pending delivery for an existing webhook (zero NextAttemptAt means now).
	// d.ID is the idempotency key: a repeat for the same webhook inserts nothing and returns [ErrDeliveryAlreadyEnqueued]; a zero ID gets a random one.
	EnqueueDelivery(ctx context.Context, d WebhookDelivery) error

	// ListPendingDeliveries claims up to `limit` due deliveries, FIFO per webhook with every webhook's oldest row ahead of any second, so one backlog cannot fill the batch.
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
