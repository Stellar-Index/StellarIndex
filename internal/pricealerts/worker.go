package pricealerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// DefaultInterval is the sweep cadence when Options.Interval is unset.
const DefaultInterval = 30 * time.Second

// AlertStore is the read/mark seam the evaluator needs from the
// platform price-alert store. Satisfied by
// postgresstore.PriceAlertStore.
type AlertStore interface {
	ListEnabledPriceAlerts(ctx context.Context) ([]platform.PriceAlert, error)
	MarkPriceAlertFired(ctx context.Context, id uuid.UUID, firedAt time.Time) error
}

// WebhookEnqueuer is the account-scoped delivery seam. The evaluator
// lists the owning account's webhooks, filters to the ones subscribed
// to `price.alert`, and enqueues one delivery each. Satisfied by
// postgresstore.WebhookStore.
type WebhookEnqueuer interface {
	ListWebhooksForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error)
	EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error
}

// PriceReader returns the latest CLOSED 1-minute VWAP for a pair.
// Satisfied in production by an adapter over
// timescale.Store.LatestClosedVWAP1mForPair (which combines both stored
// orientations). ok=false with a nil error means "no closed bucket in
// scope" — a benign no-op, not a failure.
type PriceReader interface {
	LatestVWAP(ctx context.Context, base, quote canonical.Asset) (price string, bucketClose time.Time, ok bool, err error)
}

// Options configures the [Worker].
type Options struct {
	// Interval is the sweep cadence. <= 0 falls back to DefaultInterval.
	Interval time.Duration
	Logger   *slog.Logger
	// Clock lets tests pin "now". Defaults to time.Now().UTC.
	Clock func() time.Time
}

// Worker sweeps enabled price alerts on a ticker and enqueues
// `price.alert` webhook deliveries when a threshold is crossed.
type Worker struct {
	alerts   AlertStore
	webhooks WebhookEnqueuer
	prices   PriceReader
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a Worker. Panics if any store seam is nil (a wiring bug —
// the caller must gate construction on the platform schema being
// present).
func New(alerts AlertStore, webhooks WebhookEnqueuer, prices PriceReader, opts Options) *Worker {
	if alerts == nil || webhooks == nil || prices == nil {
		panic("pricealerts: New requires non-nil alerts, webhooks and prices")
	}
	w := &Worker{
		alerts:   alerts,
		webhooks: webhooks,
		prices:   prices,
		interval: opts.Interval,
		logger:   opts.Logger,
		now:      opts.Clock,
	}
	if w.interval <= 0 {
		w.interval = DefaultInterval
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w
}

// Run drives the sweep loop until ctx is cancelled. Sweeps once
// immediately, then every Interval (usage.Rollup shape). Returns
// ctx.Err() on cancellation.
func (w *Worker) Run(ctx context.Context) error {
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	w.logger.Info("price-alert evaluator started", "interval", w.interval)
	for {
		w.Sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep runs one evaluation pass and records the paired metric. Exported
// so tests can drive a single pass deterministically. Errors are
// swallowed after being recorded (best-effort background worker) — the
// outcome is what the metric + alert rule read.
func (w *Worker) Sweep(ctx context.Context) {
	start := w.now()
	outcome := w.sweepOnce(ctx)
	w.observe(outcome, start)
}

// sweepOnce performs the work and returns the sweep outcome label
// ("ok" | "list_error" | "partial_error").
func (w *Worker) sweepOnce(ctx context.Context) string {
	alerts, err := w.alerts.ListEnabledPriceAlerts(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "ok"
		}
		w.logger.Warn("price-alert sweep: list enabled alerts failed", "err", err)
		return "list_error"
	}
	now := w.now()
	hadError := false
	for _, a := range alerts {
		if err := w.evaluateOne(ctx, a, now); err != nil {
			if errors.Is(err, context.Canceled) {
				return "ok"
			}
			hadError = true
			w.logger.Warn("price-alert evaluate failed",
				"err", err, "alert_id", a.ID, "account_id", a.AccountID,
				"base", a.BaseAsset, "quote", a.QuoteAsset)
		}
	}
	if hadError {
		return "partial_error"
	}
	return "ok"
}

// evaluateOne evaluates a single alert against the latest closed VWAP.
// Returns a non-nil error only for genuine failures (bad row, price-read
// error, enqueue error) — a benign no-price / not-crossed / cooling-down
// / no-subscribed-webhook outcome returns nil.
func (w *Worker) evaluateOne(ctx context.Context, a platform.PriceAlert, now time.Time) error {
	base, err := canonical.ParseAsset(a.BaseAsset)
	if err != nil {
		return fmt.Errorf("parse base asset %q: %w", a.BaseAsset, err)
	}
	quote, err := canonical.ParseAsset(a.QuoteAsset)
	if err != nil {
		return fmt.Errorf("parse quote asset %q: %w", a.QuoteAsset, err)
	}

	priceStr, bucketClose, ok, err := w.prices.LatestVWAP(ctx, base, quote)
	if err != nil {
		return fmt.Errorf("read latest vwap: %w", err)
	}
	if !ok {
		// No closed bucket in scope — benign (like divergence no_vwap).
		return nil
	}

	crossed, err := conditionCrossed(a.Condition, priceStr, a.Threshold)
	if err != nil {
		return err
	}
	if !crossed {
		return nil
	}
	if w.coolingDown(a, now) {
		return nil
	}

	hooks, err := w.subscribedHooks(ctx, a.AccountID)
	if err != nil {
		return err
	}
	if len(hooks) == 0 {
		// Condition holds but the account has no webhook subscribed to
		// price.alert. Don't mark fired — the moment they wire one up,
		// the next tick delivers. Not an error.
		w.logger.Debug("price alert crossed but no subscribed webhook",
			"alert_id", a.ID, "account_id", a.AccountID)
		return nil
	}
	payload, err := buildPayload(a, base, quote, priceStr, bucketClose, now)
	if err != nil {
		return fmt.Errorf("build payload: %w", err)
	}
	// Advance the fired-mark BEFORE enqueuing (NTF-PA-01). The persisted
	// LastFiredAt is this crossing's idempotency key: when the mark only
	// landed AFTER every EnqueueDelivery was durable, a transient mark
	// failure left LastFiredAt unchanged and the next tick re-delivered
	// the whole crossing — brand-new delivery ids the customer can't dedup
	// on the X-StellarIndex-Delivery-Id header, silently breaking the
	// once-per-cooldown-window guarantee. Marking first means a failure
	// mid-fan-out cannot re-notify the webhooks that already received the
	// crossing: the cooldown gate reads the durable mark on the next tick.
	// A mark failure here aborts before any EnqueueDelivery, so nothing is
	// sent twice; the residual trade is at-most-once on the narrow
	// mark-ok/enqueue-fail window, which we accept over duplicate fan-out.
	if err := w.alerts.MarkPriceAlertFired(ctx, a.ID, now); err != nil {
		return fmt.Errorf("mark fired: %w", err)
	}
	enqueued, err := w.enqueueAll(ctx, hooks, payload)
	if err != nil {
		return err
	}
	w.logger.Info("price alert fired",
		"alert_id", a.ID, "account_id", a.AccountID,
		"pair", base.String()+"/"+quote.String(),
		"condition", string(a.Condition), "threshold", a.Threshold,
		"observed", priceStr, "deliveries", enqueued)
	return nil
}

// coolingDown reports whether the alert fired recently enough that its
// cooldown window has not yet elapsed.
func (w *Worker) coolingDown(a platform.PriceAlert, now time.Time) bool {
	if a.CooldownSeconds <= 0 || a.LastFiredAt.IsZero() {
		return false
	}
	return now.Sub(a.LastFiredAt) < time.Duration(a.CooldownSeconds)*time.Second
}

// subscribedHooks returns the account's enabled webhooks that are
// subscribed to the price.alert event — the fan-out targets for a
// crossing. Resolving the targets BEFORE the fired-mark lets
// evaluateOne skip the mark entirely when there is nothing to deliver to
// (no subscriber yet), while still marking-before-enqueue when there is.
func (w *Worker) subscribedHooks(ctx context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	hooks, err := w.webhooks.ListWebhooksForAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("list account webhooks: %w", err)
	}
	var targets []platform.CustomerWebhook
	for _, h := range hooks {
		if h.Enabled && subscribed(h.Events, string(platform.WebhookEventPriceAlert)) {
			targets = append(targets, h)
		}
	}
	return targets, nil
}

// enqueueAll enqueues one price.alert delivery per target webhook and
// returns the count enqueued. A single per-webhook enqueue failure aborts
// and returns the error; the alert is already marked fired (see
// evaluateOne), so the crossing is not re-delivered to the webhooks that
// already received it on the next tick.
func (w *Worker) enqueueAll(ctx context.Context, hooks []platform.CustomerWebhook, payload []byte) (int, error) {
	enqueued := 0
	for _, h := range hooks {
		d := platform.WebhookDelivery{
			WebhookID: h.ID,
			EventType: string(platform.WebhookEventPriceAlert),
			Payload:   payload,
		}
		if err := w.webhooks.EnqueueDelivery(ctx, d); err != nil {
			return enqueued, fmt.Errorf("enqueue delivery for webhook %s: %w", h.ID, err)
		}
		enqueued++
	}
	return enqueued, nil
}

// observe records the paired counter + histogram for one sweep.
func (w *Worker) observe(outcome string, start time.Time) {
	obs.PriceAlertEvalTotal.WithLabelValues(outcome).Inc()
	obs.PriceAlertEvalDurationSeconds.WithLabelValues(outcome).
		Observe(time.Since(start).Seconds())
}

// ─── pure helpers ───────────────────────────────────────────────

// conditionCrossed compares the observed price string against the
// threshold string using big.Rat (ADR-0003 — never float). "above"
// fires at observed >= threshold; "below" at observed <= threshold.
func conditionCrossed(cond platform.AlertCondition, observed, threshold string) (bool, error) {
	obsRat, ok := new(big.Rat).SetString(observed)
	if !ok {
		return false, fmt.Errorf("unparseable observed price %q", observed)
	}
	thRat, ok := new(big.Rat).SetString(threshold)
	if !ok {
		return false, fmt.Errorf("unparseable threshold %q", threshold)
	}
	cmp := obsRat.Cmp(thRat)
	switch cond {
	case platform.AlertAbove:
		return cmp >= 0, nil
	case platform.AlertBelow:
		return cmp <= 0, nil
	default:
		return false, fmt.Errorf("unknown condition %q", cond)
	}
}

// subscribed reports whether events contains want.
func subscribed(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

// PriceAlertPayload is the JSON body POSTed to the customer's webhook
// when a price alert fires. Documented in the OpenAPI spec as
// PriceAlertWebhookPayload.
type PriceAlertPayload struct {
	Event         string `json:"event"`          // always "price.alert"
	AlertID       string `json:"alert_id"`       // the price_alerts row id
	Pair          string `json:"pair"`           // "<base>/<quote>" canonical
	BaseAsset     string `json:"base_asset"`     // canonical base asset id
	QuoteAsset    string `json:"quote_asset"`    // canonical quote asset id
	Condition     string `json:"condition"`      // "above" | "below"
	Threshold     string `json:"threshold"`      // decimal-as-string (ADR-0003)
	ObservedPrice string `json:"observed_price"` // the closed-bucket VWAP that crossed it
	Bucket        string `json:"bucket"`         // RFC3339 close time of the observed 1m bucket
	At            string `json:"at"`             // RFC3339Nano send time
}

func buildPayload(a platform.PriceAlert, base, quote canonical.Asset, priceStr string, bucketClose, now time.Time) ([]byte, error) {
	p := PriceAlertPayload{
		Event:         string(platform.WebhookEventPriceAlert),
		AlertID:       a.ID.String(),
		Pair:          base.String() + "/" + quote.String(),
		BaseAsset:     base.String(),
		QuoteAsset:    quote.String(),
		Condition:     string(a.Condition),
		Threshold:     a.Threshold,
		ObservedPrice: priceStr,
		Bucket:        bucketClose.UTC().Format(time.RFC3339),
		At:            now.UTC().Format(time.RFC3339Nano),
	}
	return json.Marshal(p)
}
