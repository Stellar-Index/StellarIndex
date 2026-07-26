// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// FanoutStore is the subset of [platform.WebhookStore] the fan-out
// service uses. Pulled out as a narrow interface so producers can
// substitute fakes in unit tests without standing up a full store.
type FanoutStore interface {
	ListWebhooksSubscribedTo(ctx context.Context, eventType platform.WebhookEventType) ([]platform.CustomerWebhook, error)
	EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error
}

// Fanout enqueues one webhook delivery per subscribed enabled
// webhook when a product event fires. Producers (freeze sink,
// incident creator, divergence service) call Fanout.Publish with
// the event type + JSON payload; the service looks up every
// subscriber and inserts a pending delivery row, which the worker
// then drains.
//
// F-1249 (codex audit-2026-05-12): pre-fix the codebase shipped
// the dashboard CRUD + worker + runbook but no production caller
// inserted delivery rows. Customers could register hooks that
// never fired.
//
// The event that triggered fan-out is already durable in its own
// table (freeze_events, incidents, divergence_runs), so a fan-out
// failure never blocks the producer. It is NOT, however, invisible:
// C3-023 (audit-2026-07-23) found `Publish` returning nothing at
// all, so a fan-out that enqueued zero of five subscribers looked
// identical to a successful one from the caller's side and the only
// trace was a WARN line. Publish now reports a [PublishResult] plus
// an error whenever any delivery was lost, and every loss also
// increments [obs.CustomerWebhookFanoutFailuresTotal].
type Fanout struct {
	store  FanoutStore
	logger *slog.Logger
	clock  func() time.Time
}

// PublishResult summarises one fan-out. Subscribers is what the
// store returned; Enqueued + Failed partition it (a short-circuit
// before the loop — invalid payload, list error — leaves all three
// zero).
type PublishResult struct {
	Subscribers int
	Enqueued    int
	Failed      int
}

// NewFanout constructs a Fanout. nil store returns nil — the
// producer call sites short-circuit on nil Fanout, matching the
// "webhook subsystem optional in this deployment" posture for
// Redis-less / Postgres-less binaries.
func NewFanout(store FanoutStore, logger *slog.Logger) *Fanout {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Fanout{store: store, logger: logger, clock: time.Now}
}

// Publish enqueues one delivery per subscribed enabled webhook.
// `payload` must be valid JSON (caller-marshalled) and small
// enough to live comfortably in a Postgres jsonb column.
//
// Returns the per-fan-out counts plus a non-nil error whenever at
// least one subscribed customer LOST the event — an unmarshallable
// payload, a subscriber-list failure, or any per-endpoint enqueue
// failure. A lost enqueue is permanent: unlike a delivery attempt
// there is no retry row to drain, so nothing downstream will ever
// re-derive it. Zero subscribers is a successful no-op (nil error,
// zero counts), not a failure.
//
// The error never obliges the caller to fail its own work — the
// triggering event is durable in its own table — but it MUST be
// acted on (C3-023): the aggregator's hot paths log it at ERROR with
// the event type + counts, and `stellarindex-ops emit-incident`
// surfaces it to the operator's shell. Every loss is also counted on
// [obs.CustomerWebhookFanoutFailuresTotal] so a fan-out that is
// silently dropping one customer's events is alertable rather than
// merely greppable.
func (f *Fanout) Publish(
	ctx context.Context, eventType platform.WebhookEventType, payload []byte,
) (PublishResult, error) {
	if f == nil {
		return PublishResult{}, ErrFanoutNotConfigured
	}
	if !json.Valid(payload) {
		f.logger.Warn("customerwebhook.fanout: payload is not valid JSON; skipping",
			"event_type", eventType)
		obs.CustomerWebhookFanoutFailuresTotal.
			WithLabelValues(string(eventType), obs.FanoutFailureInvalidPayload).Inc()
		return PublishResult{}, fmt.Errorf(
			"customerwebhook: fan-out %s: payload is not valid JSON", eventType)
	}
	subs, err := f.store.ListWebhooksSubscribedTo(ctx, eventType)
	if err != nil {
		f.logger.Warn("customerwebhook.fanout: list subscribers failed",
			"event_type", eventType, "err", err)
		obs.CustomerWebhookFanoutFailuresTotal.
			WithLabelValues(string(eventType), obs.FanoutFailureListSubscribers).Inc()
		return PublishResult{}, fmt.Errorf(
			"customerwebhook: fan-out %s: list subscribers: %w", eventType, err)
	}
	res := PublishResult{Subscribers: len(subs)}
	if len(subs) == 0 {
		return res, nil
	}
	for _, sub := range subs {
		d := platform.WebhookDelivery{
			ID:        uuid.New(),
			WebhookID: sub.ID,
			EventType: string(eventType),
			Payload:   payload,
			// NextAttemptAt zero is normalised by the store to "now"
			// so the worker's next poll picks it up immediately.
		}
		if err := f.store.EnqueueDelivery(ctx, d); err != nil {
			f.logger.Warn("customerwebhook.fanout: enqueue failed",
				"event_type", eventType, "webhook_id", sub.ID, "err", err)
			// One increment per LOST delivery, not per fan-out: the
			// count an operator needs is "how many customer events
			// vanished", which is per-subscriber.
			obs.CustomerWebhookFanoutFailuresTotal.
				WithLabelValues(string(eventType), obs.FanoutFailureEnqueue).Inc()
			res.Failed++
			continue
		}
		res.Enqueued++
	}
	if res.Failed > 0 {
		f.logger.Warn("customerwebhook.fanout: partial fan-out",
			"event_type", eventType, "enqueued", res.Enqueued, "failed", res.Failed)
		return res, fmt.Errorf(
			"customerwebhook: fan-out %s: %d of %d deliveries lost",
			eventType, res.Failed, res.Subscribers)
	}
	return res, nil
}

// MarshalPayload is a tiny convenience for callers that already
// have a Go struct: marshal it to JSON and return the bytes, or
// log + return nil if marshalling fails (callers should fan-out
// or skip based on the nil check).
func MarshalPayload(logger *slog.Logger, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		if logger != nil {
			logger.Warn("customerwebhook.MarshalPayload: marshal failed", "err", err)
		}
		return nil
	}
	return b
}

// ErrFanoutNotConfigured is what [Fanout.Publish] returns on a nil
// receiver — the "webhook subsystem optional in this deployment"
// posture [NewFanout] documents. Callers that already short-circuit
// on `if f != nil` never see it; callers that don't can tell
// "not wired" apart from "wired and lost the event" with
// errors.Is.
var ErrFanoutNotConfigured = errors.New("customerwebhook: fanout not configured")
