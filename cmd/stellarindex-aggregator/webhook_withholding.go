package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Metric labels for the withholding gates on this binary's webhook surfaces.
const (
	freezeWebhookGateSurface     = "freeze_webhook"
	divergenceWebhookGateSurface = "divergence_webhook"
)

// webhookPublisher is the fan-out seam the webhook hooks publish through.
// *customerwebhook.Fanout satisfies it.
type webhookPublisher interface {
	PublishOnce(ctx context.Context, eventType platform.WebhookEventType, eventKey string, payload []byte) (customerwebhook.PublishResult, error)
}

// anomalyFreezeHook fans `anomaly.freeze` out to customers; frozen_value is the aggregated price, so a market /v1/price withholds is not
// delivered
// (the freeze stays durable in freeze_events; only the customer copy carrying the price is skipped).
func anomalyFreezeHook(logger *slog.Logger, pub webhookPublisher, gates pricingguard.Gate) timescale.FreezeHook {
	return func(ctx context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) {
		if gates.PriceWithheld(ctx, asset, quote, freezeWebhookGateSurface) {
			logger.Info("anomaly.freeze webhook withheld: pair's price is withheld by pricing_guard",
				"asset", asset.String(), "quote", quote.String())
			return
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		payload := customerwebhook.MarshalPayload(logger, anomalyFreezeWebhookPayload{
			Event:       string(platform.WebhookEventAnomalyFreeze),
			Asset:       asset.String(),
			Quote:       quote.String(),
			FrozenValue: frozenValue,
			Reason:      string(decision.Reason),
			At:          at,
		})
		if payload == nil {
			return
		}
		// A lost fan-out is permanent (no delivery row exists to
		// retry), so it is logged at ERROR with the pair — not discarded.
		// The freeze is already durable, so the hook never fails it.
		// Keyed on the freeze so a second pass enqueues only the subscribers the first one missed.
		key := asset.String() + "|" + quote.String() + "|" + at
		res, ferr := pub.PublishOnce(ctx, platform.WebhookEventAnomalyFreeze, key, payload)
		if ferr != nil {
			logger.Error("customer-webhook fan-out lost an anomaly.freeze event",
				"err", ferr, "asset", asset.String(), "quote", quote.String(),
				"subscribers", res.Subscribers, "enqueued", res.Enqueued,
				"failed", res.Failed)
		}
	}
}

// Sizing of the off-tick fan-out: a market-wide move freezes tens of pairs in one tick, far under the buffer.
const (
	offTickFanoutBuffer  = 512
	offTickFanoutTimeout = 2 * time.Minute
	// Shutdown waits this long for queued fan-outs, well inside the unit's stop timeout.
	offTickShutdownWait = 15 * time.Second
)

var errOffTickFanoutFull = errors.New("off-tick webhook fan-out queue is full")

type offTickJob struct {
	eventType platform.WebhookEventType
	key       string
	payload   []byte
}

// offTickPublisher queues a fan-out for one worker goroutine, so the caller's tick neither waits on one INSERT per
// subscriber nor cuts the fan-out short when its own deadline fires.
type offTickPublisher struct {
	pub     webhookPublisher
	logger  *slog.Logger
	jobs    chan offTickJob
	timeout time.Duration
	done    chan struct{}
}

func newOffTickPublisher(pub webhookPublisher, logger *slog.Logger) *offTickPublisher {
	return &offTickPublisher{
		pub: pub, logger: logger,
		jobs:    make(chan offTickJob, offTickFanoutBuffer),
		timeout: offTickFanoutTimeout,
		done:    make(chan struct{}),
	}
}

// PublishOnce queues the fan-out and returns at once; the result is logged by the worker. A full queue is the only
// error, and it means the event is lost.
func (p *offTickPublisher) PublishOnce(
	_ context.Context, eventType platform.WebhookEventType, eventKey string, payload []byte,
) (customerwebhook.PublishResult, error) {
	select {
	case p.jobs <- offTickJob{eventType: eventType, key: eventKey, payload: payload}:
		return customerwebhook.PublishResult{}, nil
	default:
		obs.CustomerWebhookFanoutFailuresTotal.WithLabelValues(string(eventType), obs.FanoutFailureQueueFull).Inc()
		return customerwebhook.PublishResult{}, errOffTickFanoutFull
	}
}

// run publishes queued fan-outs until ctx ends, then publishes what is already queued and closes done.
func (p *offTickPublisher) run(ctx context.Context) {
	defer close(p.done)
	defer worker.Recover(p.logger, "webhook-fanout")
	for {
		select {
		case j := <-p.jobs:
			p.publish(ctx, j)
		case <-ctx.Done():
			for {
				select {
				case j := <-p.jobs:
					p.publish(ctx, j)
				default:
					return
				}
			}
		}
	}
}

// wait blocks until run has returned or d has passed.
func (p *offTickPublisher) wait(d time.Duration) {
	select {
	case <-p.done:
	case <-time.After(d):
	}
}

// publish runs one fan-out on its own budget, and once more when subscribers were missed: the event key makes the
// second pass enqueue only those.
func (p *offTickPublisher) publish(ctx context.Context, j offTickJob) {
	var (
		res customerwebhook.PublishResult
		err error
	)
	for range 2 {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
		res, err = p.pub.PublishOnce(pctx, j.eventType, j.key, j.payload)
		cancel()
		if err == nil {
			return
		}
	}
	p.logger.Error("customer-webhook fan-out lost an event",
		"err", err, "event_type", j.eventType, "event_key", j.key,
		"subscribers", res.Subscribers, "enqueued", res.Enqueued, "failed", res.Failed)
}

// divergenceFiringHook fans `divergence.firing` out to customers; our_price is the aggregated price, so a withheld market is not
// delivered,
// as in anomalyFreezeHook.
func divergenceFiringHook(logger *slog.Logger, pub webhookPublisher, gates pricingguard.Gate) divergence.WarningHook {
	return func(ctx context.Context, pair canonical.Pair, cached divergence.CachedResult) error {
		if gates.PriceWithheld(ctx, pair.Base, pair.Quote, divergenceWebhookGateSurface) {
			logger.Info("divergence.firing webhook withheld: pair's price is withheld by pricing_guard",
				"pair", pair.String())
			return nil
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
			return nil
		}
		// The divergence run is durable, the customer's copy is not, so a lost fan-out is an ERROR line rather than silence.
		// Keyed on the episode so a retry after a partial failure only enqueues the subscribers it missed.
		since := cached.FiringSince
		if since.IsZero() {
			since = cached.ComputedAt
		}
		key := pair.String() + "|" + since.UTC().Format(time.RFC3339Nano)
		res, ferr := pub.PublishOnce(ctx, platform.WebhookEventDivergenceFiring, key, payload)
		if ferr != nil {
			logger.Error("customer-webhook fan-out lost a divergence.firing event",
				"err", ferr, "pair", pair.String(),
				"subscribers", res.Subscribers, "enqueued", res.Enqueued,
				"failed", res.Failed)
			return ferr
		}
		return nil
	}
}
