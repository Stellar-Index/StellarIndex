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

// alertTimeoutDivisor sets each alert's evaluation budget to a fraction of
// the sweep interval. The sweep is serial, so without a per-alert deadline
// one slow alert (a never-traded pair whose VWAP probe walks evicted
// chunks) holds every other account's alerts for up to the 30 m
// background statement timeout.
const alertTimeoutDivisor = 3

// Per-alert outcomes recorded on stellarindex_price_alert_evaluated_total.
const (
	outcomeFired        = "fired"
	outcomeNotCrossed   = "not_crossed"
	outcomeNoPrice      = "no_price"
	outcomeCoolingDown  = "cooling_down"
	outcomeNoSubscriber = "no_subscriber"
	outcomeClaimLost    = "claim_lost"
	outcomeError        = "error"
	outcomeTimeout      = "timeout"
)

// AlertOutcomes is the complete per-alert outcome vocabulary.
var AlertOutcomes = []string{
	outcomeFired, outcomeNotCrossed, outcomeNoPrice, outcomeCoolingDown,
	outcomeNoSubscriber, outcomeClaimLost, outcomeError, outcomeTimeout,
}

// AlertStore is the read/mark seam the evaluator needs from the
// platform price-alert store. Satisfied by
// postgresstore.PriceAlertStore.
type AlertStore interface {
	ListEnabledPriceAlerts(ctx context.Context) ([]platform.PriceAlert, error)
	ClaimPriceAlertFire(ctx context.Context, id uuid.UUID, firedAt time.Time) (bool, error)
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
// orientations), tried across every canonical.AssetAliases spelling of
// both legs. ok=false with a nil error means "no closed bucket in
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
	alerts       AlertStore
	webhooks     WebhookEnqueuer
	prices       PriceReader
	interval     time.Duration
	alertTimeout time.Duration
	logger       *slog.Logger
	now          func() time.Time
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
	w.alertTimeout = w.interval / alertTimeoutDivisor
	for _, outcome := range AlertOutcomes {
		obs.PriceAlertEvaluatedTotal.WithLabelValues(outcome)
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
	// Seed the staleness clock so a first sweep that never completes
	// still ages the gauge instead of leaving it at the disabled-host 0.
	obs.PriceAlertLastSweepUnix.Set(float64(w.now().Unix()))
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
	obs.PriceAlertLastSweepUnix.Set(float64(w.now().Unix()))
}

// sweepOnce performs the work and returns the sweep outcome label
// ("ok" | "list_error" | "partial_error").
func (w *Worker) sweepOnce(ctx context.Context) string {
	listCtx, cancel := context.WithTimeout(ctx, w.interval)
	alerts, err := w.alerts.ListEnabledPriceAlerts(listCtx)
	cancel()
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
		outcome, err := w.evaluateOne(ctx, a, now)
		if errors.Is(err, context.Canceled) {
			return "ok"
		}
		obs.PriceAlertEvaluatedTotal.WithLabelValues(outcome).Inc()
		if err != nil {
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

// evaluateOne evaluates a single alert against the latest closed VWAP and
// returns its per-alert outcome. The error is non-nil only for a genuine
// failure (bad row, price-read, claim or enqueue error, or the alert's
// deadline) — a benign no-price / not-crossed / cooling-down /
// no-subscriber / claim-lost outcome returns nil.
//
// The reads and the fan-out each get their own alertTimeout budget from
// the sweep context. A crossing is claimed before it is enqueued, so a
// fan-out left with whatever the reads did not spend could claim the
// crossing and then run out of time before enqueueing it.
func (w *Worker) evaluateOne(ctx context.Context, a platform.PriceAlert, now time.Time) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, w.alertTimeout)
	c, outcome, err := w.checkCrossing(readCtx, a, now)
	cancel()
	if err != nil {
		return w.failureOutcome(readCtx, err)
	}
	if c == nil {
		return outcome, nil
	}
	fireCtx, cancel := context.WithTimeout(ctx, w.alertTimeout)
	defer cancel()
	outcome, err = w.fire(fireCtx, a, now, c)
	if err != nil {
		return w.failureOutcome(fireCtx, err)
	}
	return outcome, nil
}

// failureOutcome classes a stage error as the alert's own deadline or a
// plain error. The stage context is checked as well as the error chain
// because a driver may surface a cancelled statement without wrapping
// context.DeadlineExceeded.
func (w *Worker) failureOutcome(stageCtx context.Context, err error) (string, error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(stageCtx.Err(), context.DeadlineExceeded) {
		return outcomeTimeout, fmt.Errorf("exceeded %s per-alert budget: %w", w.alertTimeout, err)
	}
	return outcomeError, err
}

// crossing is an alert whose condition holds, outside cooldown, with at
// least one subscribed webhook to deliver it to.
type crossing struct {
	base, quote canonical.Asset
	priceStr    string
	bucketClose time.Time
	hooks       []platform.CustomerWebhook
}

// checkCrossing does every read an alert needs before it can fire. A nil
// crossing with a nil error means a benign outcome, returned as its label.
func (w *Worker) checkCrossing(ctx context.Context, a platform.PriceAlert, now time.Time) (*crossing, string, error) {
	base, err := canonical.ParseAsset(a.BaseAsset)
	if err != nil {
		return nil, "", fmt.Errorf("parse base asset %q: %w", a.BaseAsset, err)
	}
	quote, err := canonical.ParseAsset(a.QuoteAsset)
	if err != nil {
		return nil, "", fmt.Errorf("parse quote asset %q: %w", a.QuoteAsset, err)
	}

	priceStr, bucketClose, ok, err := w.prices.LatestVWAP(ctx, base, quote)
	if err != nil {
		return nil, "", fmt.Errorf("read latest vwap: %w", err)
	}
	if !ok {
		// No closed bucket in scope — benign (like divergence no_vwap).
		return nil, outcomeNoPrice, nil
	}

	crossed, err := conditionCrossed(a.Condition, priceStr, a.Threshold)
	if err != nil {
		return nil, "", err
	}
	if !crossed {
		return nil, outcomeNotCrossed, nil
	}
	if w.coolingDown(a, now) {
		return nil, outcomeCoolingDown, nil
	}

	hooks, err := w.subscribedHooks(ctx, a.AccountID)
	if err != nil {
		return nil, "", err
	}
	if len(hooks) == 0 {
		// Condition holds but the account has no webhook subscribed to
		// price.alert. Don't mark fired — the moment they wire one up,
		// the next tick delivers. Not an error.
		w.logger.Debug("price alert crossed but no subscribed webhook",
			"alert_id", a.ID, "account_id", a.AccountID)
		return nil, outcomeNoSubscriber, nil
	}
	return &crossing{base: base, quote: quote, priceStr: priceStr, bucketClose: bucketClose, hooks: hooks}, "", nil
}

// fire claims the crossing and fans it out to every subscribed webhook.
func (w *Worker) fire(ctx context.Context, a platform.PriceAlert, now time.Time, c *crossing) (string, error) {
	payload, err := buildPayload(a, c.base, c.quote, c.priceStr, c.bucketClose, now)
	if err != nil {
		return "", fmt.Errorf("build payload: %w", err)
	}
	// Claim the crossing BEFORE enqueuing (NTF-PA-01). The persisted
	// LastFiredAt is this crossing's idempotency key: when the mark only
	// landed AFTER every EnqueueDelivery was durable, a transient mark
	// failure left LastFiredAt unchanged and the next tick re-delivered
	// the whole crossing — brand-new delivery ids the customer can't dedup
	// on the X-StellarIndex-Delivery-Id header, silently breaking the
	// once-per-cooldown-window guarantee. Claiming first means a failure
	// mid-fan-out cannot re-notify the webhooks that already received the
	// crossing: the cooldown gate reads the durable mark on the next tick.
	// A claim failure here aborts before any EnqueueDelivery, so nothing is
	// sent twice; the residual trade is at-most-once on the narrow
	// claim-ok/enqueue-fail window, which we accept over duplicate fan-out.
	//
	// The claim is CONDITIONAL in SQL, and that is what makes coolingDown
	// above an optimisation rather than the guarantee: coolingDown reads
	// the LastFiredAt snapshot ListEnabledPriceAlerts took at the top of
	// this sweep, so two evaluators (a second aggregator, an R2/R3
	// standby, an overlapping deploy) both pass it on the same crossing.
	// Only one of them can win the row-locked UPDATE (#368 M10).
	claimed, err := w.alerts.ClaimPriceAlertFire(ctx, a.ID, now)
	if err != nil {
		return "", fmt.Errorf("claim fire: %w", err)
	}
	if !claimed {
		// Someone else owns this crossing's cooldown window, or the alert
		// was deleted mid-sweep. Either way the fan-out is not ours: this
		// is the guard working, not a failure.
		w.logger.Info("price alert crossing already claimed — skipping fan-out",
			"alert_id", a.ID, "account_id", a.AccountID)
		return outcomeClaimLost, nil
	}
	enqueued, err := w.enqueueAll(ctx, c.hooks, payload)
	w.logger.Info("price alert fired",
		"alert_id", a.ID, "account_id", a.AccountID,
		"pair", c.base.String()+"/"+c.quote.String(),
		"condition", string(a.Condition), "threshold", a.Threshold,
		"observed", c.priceStr, "deliveries", enqueued, "targets", len(c.hooks))
	return outcomeFired, err
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
// returns the count enqueued. It attempts every webhook even when an
// earlier one fails — the alert is already marked fired (see
// evaluateOne), so a crossing is never re-delivered to a webhook that
// already received it, and aborting the loop early would silently skip
// every webhook after the failing one instead of just the failing one.
// Per-webhook errors are joined and returned alongside the count so the
// caller can log which targets were missed without losing the rest.
func (w *Worker) enqueueAll(ctx context.Context, hooks []platform.CustomerWebhook, payload []byte) (int, error) {
	enqueued := 0
	var errs []error
	for _, h := range hooks {
		d := platform.WebhookDelivery{
			WebhookID: h.ID,
			EventType: string(platform.WebhookEventPriceAlert),
			Payload:   payload,
		}
		if err := w.webhooks.EnqueueDelivery(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("enqueue delivery for webhook %s: %w", h.ID, err))
			continue
		}
		enqueued++
	}
	return enqueued, errors.Join(errs...)
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
