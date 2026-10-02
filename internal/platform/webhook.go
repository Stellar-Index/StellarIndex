package platform

import (
	"context"
	"encoding/json"
	"errors"
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
	// freeze on an (asset, quote). It goes to EVERY subscribed webhook:
	// there is no per-account pair filter.
	WebhookEventAnomalyFreeze WebhookEventType = "anomaly.freeze"

	// WebhookEventDivergenceFiring fires when a price-divergence
	// warning starts; a clear sends nothing. Body is DivergenceFiringWebhookPayload.
	WebhookEventDivergenceFiring WebhookEventType = "divergence.firing"

	// WebhookEventPriceAlert fires when one of the account's
	// registered price-threshold alerts crosses its condition, once per
	// crossing (BACKLOG #60). Unlike the operational events above, this is a
	// PER-ACCOUNT event: the aggregator's price-alert evaluator
	// enqueues it only to the owning account's subscribed webhooks
	// (via ListWebhooksForAccount, not the global fan-out), so one
	// account's alerts never reach another's. Body shape:
	// PriceAlertWebhookPayload (alert_id, pair, condition, threshold,
	// observed_price, bucket, at).
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

// CustomerWebhook is an outbound HTTPS endpoint a customer
// registers to receive event notifications. Stripe-shape:
// signed deliveries (HMAC-SHA-256 of payload), exponential
// retry over ~8h (15 attempts, 30s doubling to a 1h cap).
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
// Customer surface: each plaintext key is returned exactly once —
// from `POST /v1/dashboard/webhooks` at creation and from
// `POST /v1/dashboard/webhooks/{id}/rotate-secret` at rotation — and
// never served back through any API surface again.
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
	// PreviousSecret is the key SecretHash held before the last rotation;
	// deliveries are also signed with it until PreviousSecretExpiresAt so
	// a receiver can switch keys without rejecting deliveries. Both are
	// zero when no rotation overlap is open.
	PreviousSecret          []byte
	PreviousSecretExpiresAt time.Time
	Events                  []string
	Enabled                 bool
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// ActivePreviousSecret returns PreviousSecret while its overlap window is
// open at now, else nil.
func (w CustomerWebhook) ActivePreviousSecret(now time.Time) []byte {
	if len(w.PreviousSecret) == 0 || !now.Before(w.PreviousSecretExpiresAt) {
		return nil
	}
	return w.PreviousSecret
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
	// CreateWebhook registers a new outbound endpoint, enforcing
	// the per-account `maxPerAccount` cap atomically. Returns
	// [ErrWebhookQuotaExceeded] when the cap is met — the cap
	// check + insert happen in a single SQL statement so
	// concurrent callers can't both pass a pre-check and each
	// append a row past the cap. F-1248 (codex audit-2026-05-12).
	// A cap <= 0 admits nothing (TierAnon's ladder value). Returns
	// [ErrConflict] when the account already registered w.URL.
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
	// Returns [ErrConflict] when the new url duplicates another of
	// the account's webhooks.
	UpdateWebhook(ctx context.Context, w CustomerWebhook) error

	// RotateWebhookSecret makes newSecret the signing key IN PLACE: the
	// current key moves to PreviousSecret, valid until previousExpiresAt,
	// and the row, its queued deliveries and its delivery log are kept.
	// A rotation inside an open overlap replaces the older previous key.
	// Like create, the key is persisted raw — see [CustomerWebhook].
	// Returns [ErrNotFound] when the webhook does not exist.
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

	// ─── Worker-side queue surface (F-1270 audit-2026-05-12) ─────

	// EnqueueDelivery inserts one pending delivery row keyed off
	// an existing webhook. The worker then drains the queue via
	// ListPendingDeliveries. attempt_count starts at 0;
	// NextAttemptAt zero is normalised to "now" so the first
	// poll picks it up immediately.
	//
	// d.ID is the row's primary key and so its idempotency key: a
	// second enqueue with an ID already queued for the same webhook
	// inserts nothing and returns an error wrapping
	// [ErrDeliveryAlreadyEnqueued]. A zero ID gets a fresh random one.
	EnqueueDelivery(ctx context.Context, d WebhookDelivery) error

	// ListPendingDeliveries claims up to `limit` deliveries whose
	// next_attempt_at is in the past: FIFO within each webhook, with
	// every due webhook's oldest row ahead of any webhook's second so
	// one endpoint's backlog cannot fill the batch. The delivery
	// worker calls this on each poll tick.
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
