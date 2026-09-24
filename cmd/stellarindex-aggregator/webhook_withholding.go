package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Metric labels for the withholding gates on this binary's webhook surfaces.
const (
	freezeWebhookGateSurface     = "freeze_webhook"
	divergenceWebhookGateSurface = "divergence_webhook"
)

// webhookPublisher is the fan-out seam the webhook hooks publish through.
// *customerwebhook.Fanout satisfies it.
type webhookPublisher interface {
	Publish(ctx context.Context, eventType platform.WebhookEventType, payload []byte) (customerwebhook.PublishResult, error)
}

// priceWithholding is the pair of [pricing_guard] gates this binary
// consults before any customer-facing publication of an aggregated price.
// Both are nil-receiver safe (nil withholds nothing).
type priceWithholding struct {
	substance *pricingguard.SubstanceGate
	scam      *pricingguard.ScamGate
}

func (g priceWithholding) withheld(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return pricingguard.PriceWithheld(ctx, g.substance, g.scam, base, quote, surface)
}

// anomalyFreezeHook fans an `anomaly.freeze` out to subscribed customers.
// frozen_value is the pair's aggregated price, so a market /v1/price
// withholds is not delivered: the freeze itself stays durable in
// freeze_events, only the customer copy carrying the price is skipped.
func anomalyFreezeHook(logger *slog.Logger, pub webhookPublisher, gates priceWithholding) timescale.FreezeHook {
	return func(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) {
		if gates.withheld(ctx, asset, quote, freezeWebhookGateSurface) {
			logger.Info("anomaly.freeze webhook withheld: pair's price is withheld by pricing_guard",
				"asset", asset.String(), "quote", quote.String())
			return
		}
		payload := customerwebhook.MarshalPayload(logger, anomalyFreezeWebhookPayload{
			Event:       string(platform.WebhookEventAnomalyFreeze),
			Asset:       asset.String(),
			Quote:       quote.String(),
			FrozenValue: frozenValue,
			Reason:      string(decision.Reason),
			At:          time.Now().UTC().Format(time.RFC3339Nano),
		})
		if payload == nil {
			return
		}
		// A lost fan-out is permanent (no delivery row exists to
		// retry), so it is logged at ERROR with the pair — not discarded.
		// The freeze is already durable, so the hook never fails it.
		res, ferr := pub.Publish(ctx, platform.WebhookEventAnomalyFreeze, payload)
		if ferr != nil {
			logger.Error("customer-webhook fan-out lost an anomaly.freeze event",
				"err", ferr, "asset", asset.String(), "quote", quote.String(),
				"subscribers", res.Subscribers, "enqueued", res.Enqueued,
				"failed", res.Failed)
		}
	}
}

// divergenceFiringHook fans a `divergence.firing` out to subscribed
// customers. our_price is the pair's aggregated price, so a market
// /v1/price withholds is not delivered, for the same reason as
// [anomalyFreezeHook].
func divergenceFiringHook(logger *slog.Logger, pub webhookPublisher, gates priceWithholding) divergence.WarningHook {
	return func(ctx context.Context, pair canonical.Pair, cached divergence.CachedResult) {
		if gates.withheld(ctx, pair.Base, pair.Quote, divergenceWebhookGateSurface) {
			logger.Info("divergence.firing webhook withheld: pair's price is withheld by pricing_guard",
				"pair", pair.String())
			return
		}
		payload := customerwebhook.MarshalPayload(logger, divergenceFiringWebhookPayload{
			Event:         string(platform.WebhookEventDivergenceFiring),
			Pair:          pair.String(),
			OurPrice:      formatDivergencePrice(cached.OurPrice),
			Median:        formatDivergencePrice(cached.Median),
			DivergencePct: cached.DivergencePct,
			SuccessCount:  cached.SuccessCount,
			Sources:       divergenceSourceNames(cached.Sources),
			At:            cached.ComputedAt.Format(time.RFC3339Nano),
		})
		if payload == nil {
			return
		}
		// The divergence run is durable, the customer's copy is
		// not, so a lost fan-out is an ERROR line rather than silence.
		res, ferr := pub.Publish(ctx, platform.WebhookEventDivergenceFiring, payload)
		if ferr != nil {
			logger.Error("customer-webhook fan-out lost a divergence.firing event",
				"err", ferr, "pair", pair.String(),
				"subscribers", res.Subscribers, "enqueued", res.Enqueued,
				"failed", res.Failed)
		}
	}
}
