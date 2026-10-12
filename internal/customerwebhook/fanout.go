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

// FanoutStore is the narrow subset of [platform.WebhookStore] the fan-out uses, so producers can substitute fakes in unit tests.
type FanoutStore interface {
	ListWebhooksSubscribedTo(ctx context.Context, eventType platform.WebhookEventType) ([]platform.CustomerWebhook, error)
	EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error
}

// Fanout enqueues one delivery per subscribed enabled webhook when a product event fires; a fan-out failure never blocks the producer (the event is already durable) but must be visible: Publish returns a [PublishResult] plus an error on any lost delivery and increments [obs.CustomerWebhookFanoutFailuresTotal].
type Fanout struct {
	store  FanoutStore
	logger *slog.Logger
	clock  func() time.Time
}

// PublishResult summarises one fan-out: Subscribers is what the store returned and Enqueued + Failed + Suppressed partition it (an early short-circuit leaves them all zero).
type PublishResult struct {
	Subscribers int
	Enqueued    int
	Failed      int

	// Suppressed counts deliveries the store REFUSED by policy (the owning account was suspended
	// or closed between subscriber resolve and insert). It is not a Failed: Failed means a
	// customer LOST the event and is alertable, so folding the kill switch in would page needlessly.
	Suppressed int

	// AlreadyEnqueued counts subscribers the store already held this
	// event for — a [Fanout.PublishOnce] re-run after a partial fan-out.
	// Neither lost nor newly queued, so it is neither Failed nor Enqueued.
	AlreadyEnqueued int
}

// NewFanout constructs a Fanout; a nil store returns nil and producer call sites short-circuit on a nil Fanout (webhooks optional in this deployment).
func NewFanout(store FanoutStore, logger *slog.Logger) *Fanout {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Fanout{store: store, logger: logger, clock: time.Now}
}

// Publish enqueues one delivery per subscribed enabled webhook (payload: caller-marshalled JSON); it returns a non-nil error whenever a subscribed customer LOST the event (permanent, no retry row), which callers MUST log at ERROR; suspended or closed accounts are counted in [PublishResult.Suppressed], not an error.
func (f *Fanout) Publish(
	ctx context.Context, eventType platform.WebhookEventType, payload []byte,
) (PublishResult, error) {
	return f.publish(ctx, eventType, payload, func(uuid.UUID) uuid.UUID { return uuid.New() })
}

// publish is the fan-out both entry points share; deliveryID names each
// subscriber's row.
func (f *Fanout) publish(
	ctx context.Context, eventType platform.WebhookEventType, payload []byte,
	deliveryID func(webhookID uuid.UUID) uuid.UUID,
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
	for _, sub := range subs {
		f.enqueueOne(ctx, eventType, payload, sub.ID, deliveryID(sub.ID), &res)
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

// PublishOnce is [Fanout.Publish] for an event with a stable identity: the delivery ID derives from (eventType, eventKey, webhook id) so a re-run enqueues only missed subscribers (the rest land in [PublishResult.AlreadyEnqueued]); eventKey must identify the event, not the attempt, and an empty key is refused.
func (f *Fanout) PublishOnce(
	ctx context.Context, eventType platform.WebhookEventType, eventKey string, payload []byte,
) (PublishResult, error) {
	if eventKey == "" {
		return PublishResult{}, fmt.Errorf("customerwebhook: fan-out %s: empty event key", eventType)
	}
	return f.publish(ctx, eventType, payload, func(webhookID uuid.UUID) uuid.UUID {
		return EventDeliveryID(eventType, eventKey, webhookID)
	})
}

// EventDeliveryID is the delivery ID [Fanout.PublishOnce] assigns one subscriber's copy of an event.
func EventDeliveryID(eventType platform.WebhookEventType, eventKey string, webhookID uuid.UUID) uuid.UUID {
	name := string(eventType) + "\x00" + eventKey + "\x00" + webhookID.String()
	return uuid.NewSHA1(deliveryIDNamespace, []byte(name))
}

// deliveryIDNamespace scopes [EventDeliveryID]'s name-based UUIDs.
// Changing it re-keys every event, so a re-run straddling the change
// would re-notify.
var deliveryIDNamespace = uuid.MustParse("5d1f7c52-8a3e-4f0b-9c61-2e7d4b8a0f13")

// enqueueOne enqueues one subscriber's delivery and records its outcome
// on res.
func (f *Fanout) enqueueOne(
	ctx context.Context, eventType platform.WebhookEventType, payload []byte,
	webhookID, id uuid.UUID, res *PublishResult,
) {
	d := platform.WebhookDelivery{
		ID:        id,
		WebhookID: webhookID,
		EventType: string(eventType),
		Payload:   payload,
		// NextAttemptAt zero is normalised by the store to "now"
		// so the worker's next poll picks it up immediately.
	}
	err := f.store.EnqueueDelivery(ctx, d)
	switch {
	case err == nil:
		res.Enqueued++
	case errors.Is(err, platform.ErrDeliveryAlreadyEnqueued):
		f.logger.Info("customerwebhook.fanout: delivery already enqueued",
			"event_type", eventType, "webhook_id", webhookID, "delivery_id", id)
		res.AlreadyEnqueued++
	case suppressed(err):
		// The account kill switch closed between the resolve and this
		// insert. Nothing was lost that should have been delivered, so
		// this is not a fan-out failure and must not touch the lost-event
		// counter.
		f.logger.Info("customerwebhook.fanout: delivery suppressed by account status",
			"event_type", eventType, "webhook_id", webhookID, "err", err)
		res.Suppressed++
	default:
		f.logger.Warn("customerwebhook.fanout: enqueue failed",
			"event_type", eventType, "webhook_id", webhookID, "err", err)
		// One increment per LOST delivery, not per fan-out: the count an
		// operator needs is "how many customer events vanished", which is
		// per-subscriber.
		obs.CustomerWebhookFanoutFailuresTotal.
			WithLabelValues(string(eventType), obs.FanoutFailureEnqueue).Inc()
		res.Failed++
	}
}

// deliverySuppressor is implemented by store errors that report an enqueue REFUSED by policy (the
// account kill switch: suspended or closed owner) rather than one LOST to a failure. It is matched
// behaviourally, like net.Error's Timeout, so the narrow [FanoutStore] seam needs no concrete
// store import.
type deliverySuppressor interface{ WebhookSuppressed() bool }

// suppressed reports whether err is a policy refusal (see
// [deliverySuppressor]) rather than a lost delivery.
func suppressed(err error) bool {
	var s deliverySuppressor
	return errors.As(err, &s) && s.WebhookSuppressed()
}

// MarshalPayload marshals v to JSON, or logs and returns nil on failure, which callers should check before fanning out.
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

// ErrFanoutNotConfigured is what [Fanout.Publish] returns on a nil receiver (webhooks optional), so errors.Is tells "not wired" from "wired and lost the event".
var ErrFanoutNotConfigured = errors.New("customerwebhook: fanout not configured")
