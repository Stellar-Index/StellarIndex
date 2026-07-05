package pricealerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/obstest"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// ─── fakes ──────────────────────────────────────────────────────

type fakeAlertStore struct {
	mu       sync.Mutex
	enabled  []platform.PriceAlert
	listErr  error
	firedIDs []uuid.UUID
}

func (s *fakeAlertStore) ListEnabledPriceAlerts(context.Context) ([]platform.PriceAlert, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	// Mirror production: only enabled rows are returned.
	var out []platform.PriceAlert
	for _, a := range s.enabled {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeAlertStore) MarkPriceAlertFired(_ context.Context, id uuid.UUID, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firedIDs = append(s.firedIDs, id)
	return nil
}

type fakeWebhooks struct {
	mu       sync.Mutex
	byAcct   map[uuid.UUID][]platform.CustomerWebhook
	enqueued []platform.WebhookDelivery
	enqErr   error
}

func (s *fakeWebhooks) ListWebhooksForAccount(_ context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	return s.byAcct[accountID], nil
}

func (s *fakeWebhooks) EnqueueDelivery(_ context.Context, d platform.WebhookDelivery) error {
	if s.enqErr != nil {
		return s.enqErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued = append(s.enqueued, d)
	return nil
}

type fakePrices struct {
	price  string
	bucket time.Time
	ok     bool
	err    error
}

func (p fakePrices) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, bool, error) {
	return p.price, p.bucket, p.ok, p.err
}

// ─── helpers ────────────────────────────────────────────────────

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func priceAlertWebhook(acct uuid.UUID, enabled bool, events ...string) platform.CustomerWebhook {
	return platform.CustomerWebhook{
		ID:        uuid.New(),
		AccountID: acct,
		URL:       "https://hooks.example.com/x",
		Events:    events,
		Enabled:   enabled,
	}
}

func buildWorker(alerts *fakeAlertStore, hooks *fakeWebhooks, prices PriceReader) *Worker {
	return New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
	})
}

// ─── tests ──────────────────────────────────────────────────────

func TestSweep_Fires(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.20", bucket: time.Date(2026, 7, 5, 11, 59, 0, 0, time.UTC), ok: true}

	before := obstest.HistogramSampleCount(t, obs.PriceAlertEvalDurationSeconds, "outcome", "ok")
	buildWorker(alerts, hooks, prices).Sweep(context.Background())

	if len(hooks.enqueued) != 1 {
		t.Fatalf("want 1 enqueued delivery, got %d", len(hooks.enqueued))
	}
	if hooks.enqueued[0].EventType != string(platform.WebhookEventPriceAlert) {
		t.Errorf("event type = %q", hooks.enqueued[0].EventType)
	}
	if len(alerts.firedIDs) != 1 || alerts.firedIDs[0] != alert.ID {
		t.Errorf("MarkPriceAlertFired not recorded: %+v", alerts.firedIDs)
	}
	if after := obstest.HistogramSampleCount(t, obs.PriceAlertEvalDurationSeconds, "outcome", "ok"); after <= before {
		t.Errorf("ok histogram did not advance (%d -> %d)", before, after)
	}
}

func TestSweep_NoFire_BelowThreshold(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.10", bucket: time.Now(), ok: true} // below 0.15

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 0 {
		t.Errorf("should not fire below threshold, got %d deliveries", len(hooks.enqueued))
	}
	if len(alerts.firedIDs) != 0 {
		t.Errorf("should not MarkFired")
	}
}

func TestSweep_Below_Fires(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertBelow, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.10", bucket: time.Now(), ok: true} // below 0.15 → below fires

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 1 {
		t.Errorf("below condition should fire, got %d deliveries", len(hooks.enqueued))
	}
}

func TestSweep_CooldownSuppresses(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
		LastFiredAt:     now.Add(-10 * time.Second), // fired 10s ago, cooldown 300s
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.20", bucket: now, ok: true}

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 0 {
		t.Errorf("cooldown should suppress the fire, got %d deliveries", len(hooks.enqueued))
	}
}

func TestSweep_CooldownElapsed_Fires(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
		LastFiredAt:     now.Add(-10 * time.Minute), // well past cooldown
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.20", bucket: now, ok: true}

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 1 {
		t.Errorf("cooldown elapsed should fire, got %d deliveries", len(hooks.enqueued))
	}
}

func TestSweep_DisabledNotEvaluated(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: false, // disabled
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.20", bucket: time.Now(), ok: true}

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 0 {
		t.Errorf("disabled alert must not fire, got %d deliveries", len(hooks.enqueued))
	}
}

func TestSweep_NoSubscribedWebhook_DoesNotMarkFired(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	// Webhook subscribes to a DIFFERENT event only.
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventIncidentSEV1))},
	}}
	prices := fakePrices{price: "0.20", bucket: time.Now(), ok: true}

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 0 {
		t.Errorf("no price.alert subscriber → no delivery, got %d", len(hooks.enqueued))
	}
	if len(alerts.firedIDs) != 0 {
		t.Errorf("must not MarkFired when nothing was enqueued (so it delivers once a webhook is added)")
	}
}

func TestSweep_NoPrice_NoFire(t *testing.T) {
	acct := uuid.New()
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{ok: false} // no closed bucket

	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if len(hooks.enqueued) != 0 {
		t.Errorf("no price → no fire, got %d deliveries", len(hooks.enqueued))
	}
}

func TestSweep_ListError_RecordsOutcome(t *testing.T) {
	alerts := &fakeAlertStore{listErr: errors.New("db down")}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{}}
	prices := fakePrices{ok: false}

	before := obstest.HistogramSampleCount(t, obs.PriceAlertEvalDurationSeconds, "outcome", "list_error")
	buildWorker(alerts, hooks, prices).Sweep(context.Background())
	if after := obstest.HistogramSampleCount(t, obs.PriceAlertEvalDurationSeconds, "outcome", "list_error"); after <= before {
		t.Errorf("list_error histogram did not advance (%d -> %d)", before, after)
	}
}

func TestConditionCrossed(t *testing.T) {
	cases := []struct {
		cond      platform.AlertCondition
		observed  string
		threshold string
		want      bool
	}{
		{platform.AlertAbove, "0.20", "0.15", true},
		{platform.AlertAbove, "0.15", "0.15", true}, // inclusive
		{platform.AlertAbove, "0.10", "0.15", false},
		{platform.AlertBelow, "0.10", "0.15", true},
		{platform.AlertBelow, "0.15", "0.15", true}, // inclusive
		{platform.AlertBelow, "0.20", "0.15", false},
		// big-decimal precision beyond float64 (ADR-0003).
		{platform.AlertAbove, "1200.000000000000000001", "1200", true},
	}
	for _, tc := range cases {
		got, err := conditionCrossed(tc.cond, tc.observed, tc.threshold)
		if err != nil {
			t.Fatalf("conditionCrossed(%v,%s,%s): %v", tc.cond, tc.observed, tc.threshold, err)
		}
		if got != tc.want {
			t.Errorf("conditionCrossed(%v,%s,%s) = %v, want %v", tc.cond, tc.observed, tc.threshold, got, tc.want)
		}
	}
}
