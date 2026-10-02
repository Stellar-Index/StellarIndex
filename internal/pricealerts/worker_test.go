package pricealerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// ─── fakes ──────────────────────────────────────────────────────

type fakeAlertStore struct {
	mu       sync.Mutex
	enabled  []platform.PriceAlert
	listErr  error
	firedIDs []uuid.UUID
	// claimErr is returned by the next claimErrN calls to
	// ClaimPriceAlertFire, simulating a transient store write failure on
	// the fired-mark.
	claimErr  error
	claimErrN int
	// dbLastFired is the AUTHORITATIVE last_fired_at the conditional
	// UPDATE evaluates its predicate against — the row as it stands in
	// Postgres NOW, which is not necessarily the snapshot
	// ListEnabledPriceAlerts handed the evaluator at the top of the sweep.
	// Seeding it independently of `enabled` is how a test models a second
	// evaluator having claimed the same crossing in between (#368 M10).
	// Empty entry = SQL NULL = never fired.
	dbLastFired map[uuid.UUID]time.Time
	// dbDisarmed is the authoritative disarmed column; rearms counts
	// RearmPriceAlert calls that cleared it.
	dbDisarmed map[uuid.UUID]bool
	rearms     int
	// dbRule is the authoritative rule when an edit committed after the
	// sweep's snapshot; absent = the snapshot is current.
	dbRule map[uuid.UUID]platform.PriceAlert
}

func (s *fakeAlertStore) ListEnabledPriceAlerts(context.Context) ([]platform.PriceAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// ClaimPriceAlertFire models the conditional UPDATE, predicate and all:
// it claims only when the AUTHORITATIVE row (dbLastFired, dbRule) says the
// cooldown has elapsed and the rule is unchanged, never when the caller's
// snapshot does.
func (s *fakeAlertStore) ClaimPriceAlertFire(_ context.Context, snap platform.PriceAlert, firedAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErrN > 0 {
		s.claimErrN--
		return false, s.claimErr
	}
	id := snap.ID
	if cur, ok := s.dbRule[id]; ok && !sameRule(cur, snap) {
		return false, nil
	}
	cooldown := 0
	for _, a := range s.enabled {
		if a.ID == id {
			cooldown = a.CooldownSeconds
		}
	}
	if s.dbDisarmed[id] {
		return false, nil
	}
	// last_fired_at IS NULL OR last_fired_at + cooldown <= firedAt
	if last, ok := s.dbLastFired[id]; ok && !last.IsZero() &&
		last.Add(time.Duration(cooldown)*time.Second).After(firedAt) {
		return false, nil
	}
	s.firedIDs = append(s.firedIDs, id)
	if s.dbLastFired == nil {
		s.dbLastFired = map[uuid.UUID]time.Time{}
	}
	s.dbLastFired[id] = firedAt
	if s.dbDisarmed == nil {
		s.dbDisarmed = map[uuid.UUID]bool{}
	}
	s.dbDisarmed[id] = true
	// Reflect the postgres UPDATE so the next sweep's ListEnabled sees the
	// advanced cooldown clock (last_fired_at) — without this the fake would
	// never exercise coolingDown across sweeps.
	for i := range s.enabled {
		if s.enabled[i].ID == id {
			s.enabled[i].LastFiredAt = firedAt
			s.enabled[i].Disarmed = true
		}
	}
	return true, nil
}

// RearmPriceAlert models the compare-and-swap UPDATE: it clears disarmed
// only while the authoritative last_fired_at still equals the caller's.
func (s *fakeAlertStore) RearmPriceAlert(_ context.Context, id uuid.UUID, lastFiredAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dbDisarmed[id] || !s.dbLastFired[id].Equal(lastFiredAt) {
		return false, nil
	}
	s.dbDisarmed[id] = false
	s.rearms++
	for i := range s.enabled {
		if s.enabled[i].ID == id {
			s.enabled[i].Disarmed = false
		}
	}
	return true, nil
}

// sameRule mirrors the claim's rule predicate; thresholds compare as
// NUMERIC values, so "0.15" equals "0.150".
func sameRule(a, b platform.PriceAlert) bool {
	ta, okA := new(big.Rat).SetString(a.Threshold)
	tb, okB := new(big.Rat).SetString(b.Threshold)
	return a.Enabled && okA && okB && ta.Cmp(tb) == 0 &&
		a.BaseAsset == b.BaseAsset && a.QuoteAsset == b.QuoteAsset && a.Condition == b.Condition
}

type fakeWebhooks struct {
	mu       sync.Mutex
	byAcct   map[uuid.UUID][]platform.CustomerWebhook
	enqueued []platform.WebhookDelivery
	enqErr   error
	// enqErrFor fails EnqueueDelivery only for the named webhook, letting a
	// test model one bad target among several without failing every target.
	enqErrFor map[uuid.UUID]error
}

func (s *fakeWebhooks) ListWebhooksForAccount(_ context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	return s.byAcct[accountID], nil
}

func (s *fakeWebhooks) EnqueueDelivery(_ context.Context, d platform.WebhookDelivery) error {
	if err, ok := s.enqErrFor[d.WebhookID]; ok {
		return err
	}
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
		t.Errorf("ClaimPriceAlertFire not recorded: %+v", alerts.firedIDs)
	}
	if after := obstest.HistogramSampleCount(t, obs.PriceAlertEvalDurationSeconds, "outcome", "ok"); after <= before {
		t.Errorf("ok histogram did not advance (%d -> %d)", before, after)
	}
}

// TestEnqueueAll_ContinuesPastFailure proves a single bad webhook does not
// abort the fan-out: the two healthy targets still get their delivery
// enqueued, and the failure surfaces in the returned error rather than
// silently swallowing the remaining webhooks.
func TestEnqueueAll_ContinuesPastFailure(t *testing.T) {
	acct := uuid.New()
	good1 := priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))
	bad := priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))
	good2 := priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))
	hooks := &fakeWebhooks{
		enqErrFor: map[uuid.UUID]error{bad.ID: errors.New("delivery queue full")},
	}
	w := buildWorker(&fakeAlertStore{}, hooks, fakePrices{})

	enqueued, err := w.enqueueAll(context.Background(),
		[]platform.CustomerWebhook{good1, bad, good2}, []byte(`{}`))

	if err == nil {
		t.Fatal("want non-nil error for the failing webhook, got nil")
	}
	if enqueued != 2 {
		t.Fatalf("want 2 successful deliveries enqueued despite the middle failure, got %d", enqueued)
	}
	if len(hooks.enqueued) != 2 {
		t.Fatalf("want 2 deliveries recorded, got %d", len(hooks.enqueued))
	}
	gotIDs := map[uuid.UUID]bool{hooks.enqueued[0].WebhookID: true, hooks.enqueued[1].WebhookID: true}
	if !gotIDs[good1.ID] || !gotIDs[good2.ID] {
		t.Fatalf("want deliveries for both surviving webhooks, got %+v", hooks.enqueued)
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

// TestSweep_FiredMarkFlaky_NoDuplicateDelivery pins NTF-PA-01: a
// transient ClaimPriceAlertFire failure must NOT cause the crossing to be
// re-delivered to the same webhooks on the next tick. With the fired-mark
// advanced before the fan-out, each subscribed webhook receives the
// crossing at most once across repeated sweeps.
//
// Pre-fix (enqueue-then-mark): sweep 1 enqueues both webhooks, the mark
// fails, and sweep 2 re-enqueues both → 4 deliveries (duplicates on new,
// un-dedupable delivery ids). Post-fix: 2 deliveries, one per webhook.
func TestSweep_FiredMarkFlaky_NoDuplicateDelivery(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 3600, // a once-per-hour crossing notification
	}
	alerts := &fakeAlertStore{
		enabled:   []platform.PriceAlert{alert},
		claimErr:  errors.New("transient db error"),
		claimErrN: 1, // the FIRST fired-mark write fails
	}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {
			priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert)),
			priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert)),
		},
	}}
	prices := fakePrices{price: "0.20", bucket: now, ok: true}

	w := New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	// Two sweeps: the first hits the transient fired-mark failure; the
	// second re-evaluates the still-crossed alert.
	w.Sweep(context.Background())
	w.Sweep(context.Background())

	if len(hooks.enqueued) != 2 {
		t.Fatalf("want each of the 2 webhooks delivered exactly once (2 total), got %d — a flaky fired-mark caused duplicate fan-out", len(hooks.enqueued))
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

// TestSweep_StalePrice_NoFire proves the evaluator rejects a crossing
// built off a closed VWAP bucket older than its own freshness budget
// (GH-664): the condition is crossed and there is no cooldown in the
// way, but the bucket closed well outside maxPriceStaleness of "now", so
// the alert must not fire and the `stale` outcome must be recorded.
func TestSweep_StalePrice_NoFire(t *testing.T) {
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
	// "now" in buildWorker is fixed at 2026-07-05 12:00:00 UTC; a bucket
	// that closed 14 days earlier crosses the threshold but is far
	// outside any live-crossing freshness budget.
	prices := fakePrices{
		price:  "0.20",
		bucket: time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
		ok:     true,
	}

	before := obstest.CounterValue(obs.PriceAlertEvaluatedTotal.WithLabelValues(outcomeStale))
	buildWorker(alerts, hooks, prices).Sweep(context.Background())

	if len(hooks.enqueued) != 0 {
		t.Errorf("stale price → no fire, got %d deliveries", len(hooks.enqueued))
	}
	if len(alerts.firedIDs) != 0 {
		t.Errorf("stale price must not claim a fire, got %+v", alerts.firedIDs)
	}
	if after := obstest.CounterValue(obs.PriceAlertEvaluatedTotal.WithLabelValues(outcomeStale)); after-before != 1 {
		t.Errorf("stale outcome counter did not advance by 1 (%v -> %v)", before, after)
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

// TestSweep_CrossingAlreadyClaimedByAnotherEvaluator_NoFanOut pins #368
// M10: the cooldown gate is check-then-act, so it cannot be the thing
// that guarantees one notification per crossing.
//
// coolingDown reads a.LastFiredAt from the SNAPSHOT
// ListEnabledPriceAlerts took at the top of the sweep. Two evaluators
// running the same code — an operator who started a second aggregator, an
// R2/R3 standby, or the overlap window of a rolling deploy — take that
// snapshot before either has fired, so BOTH pass the gate on the same
// crossing. Pre-fix the mark was an unconditional UPDATE, so both also
// "succeeded" and both fanned out: the customer got two webhooks per
// crossing, with different delivery ids they cannot dedup on
// X-StellarIndex-Delivery-Id.
//
// Modelled here as the second instance: the alert's snapshot says "never
// fired" while the authoritative row already carries the winner's
// last_fired_at, one second old, inside a one-hour cooldown. The
// conditional UPDATE must refuse the claim and the fan-out must not
// happen.
//
// Proven red on the pre-fix code: 1 delivery enqueued, want 0.
func TestSweep_CrossingAlreadyClaimedByAnotherEvaluator_NoFanOut(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 3600,
		// The snapshot this evaluator swept with: never fired. This is
		// what makes coolingDown say "go ahead".
		LastFiredAt: time.Time{},
	}
	alerts := &fakeAlertStore{
		enabled: []platform.PriceAlert{alert},
		// The row as it actually stands in Postgres: the other evaluator
		// claimed this crossing a second ago, well inside the cooldown.
		dbLastFired: map[uuid.UUID]time.Time{alert.ID: now.Add(-time.Second)},
	}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := fakePrices{price: "0.20", bucket: now, ok: true}

	w := New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	w.Sweep(context.Background())

	if len(hooks.enqueued) != 0 {
		t.Errorf("enqueued %d delivery/deliveries, want 0 — the crossing was already claimed by another evaluator, so this one must not fan out (the customer would receive the same crossing twice, on delivery ids they cannot dedup)", len(hooks.enqueued))
	}
	if len(alerts.firedIDs) != 0 {
		t.Errorf("recorded %d claim(s), want 0 — the conditional UPDATE must match no row while the cooldown is unelapsed", len(alerts.firedIDs))
	}
	// And the winner's mark must be intact: a losing claim that still
	// stamped last_fired_at would slide the whole fleet's cooldown window
	// forward on every tick.
	if got := alerts.dbLastFired[alert.ID]; !got.Equal(now.Add(-time.Second)) {
		t.Errorf("last_fired_at = %v, want %v — a refused claim must not stamp the row", got, now.Add(-time.Second))
	}
}

// TestSweep_RuleEditedMidSweep_NoFanOut pins the claim to the rule the
// evaluator compared: the owner raised the threshold to 0.30 after the
// sweep's snapshot (0.15) was read, so a 0.20 price must not notify.
func TestSweep_RuleEditedMidSweep_NoFanOut(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	edited := alert
	edited.Threshold = "0.30"
	disabled := alert
	disabled.Enabled = false
	for name, cur := range map[string]platform.PriceAlert{"threshold raised": edited, "disabled": disabled} {
		t.Run(name, func(t *testing.T) {
			alerts := &fakeAlertStore{
				enabled: []platform.PriceAlert{alert},
				dbRule:  map[uuid.UUID]platform.PriceAlert{alert.ID: cur},
			}
			hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
				acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
			}}
			prices := fakePrices{price: "0.20", bucket: now, ok: true}
			New(alerts, hooks, prices, Options{
				Interval: time.Second, Logger: quietLogger(), Clock: func() time.Time { return now },
			}).Sweep(context.Background())

			if len(hooks.enqueued) != 0 || len(alerts.firedIDs) != 0 {
				t.Errorf("enqueued %d, claimed %d; want 0 and 0 — the row no longer holds the rule this sweep compared",
					len(hooks.enqueued), len(alerts.firedIDs))
			}
		})
	}
}

// stepPrices is a PriceReader a test moves between sweeps.
type stepPrices struct {
	price  string
	bucket time.Time
}

func (p *stepPrices) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, bool, error) {
	return p.price, p.bucket, true, nil
}

// TestSweep_FiresOncePerCrossing pins the "crosses its threshold" contract:
// a condition that keeps holding across sweeps, every one past the
// cooldown, is one crossing and one delivery. Only a fresh price on the
// other side re-arms the alert, and the next crossing fires again.
func TestSweep_FiresOncePerCrossing(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: platform.MinAlertCooldownSeconds,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	prices := &stepPrices{price: "0.20"}
	w := New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	sweepAt := func(price string) {
		now = now.Add(10 * time.Minute) // always past the 300 s cooldown
		prices.price, prices.bucket = price, now.Add(-time.Minute)
		w.Sweep(context.Background())
	}

	for range 3 {
		sweepAt("0.20")
	}
	if len(hooks.enqueued) != 1 {
		t.Fatalf("enqueued %d deliveries over 3 sweeps with the condition held, want 1 — one crossing is one notification", len(hooks.enqueued))
	}

	sweepAt("0.10")
	if alerts.rearms != 1 {
		t.Fatalf("re-arms = %d after a fresh price below the threshold, want 1", alerts.rearms)
	}
	sweepAt("0.20")
	sweepAt("0.20")
	if len(hooks.enqueued) != 2 {
		t.Errorf("enqueued %d deliveries after a second crossing, want 2", len(hooks.enqueued))
	}
}

// TestSweep_StalePriceDoesNotRearm: a disarmed alert re-arms only on a
// FRESH price that clears the condition; a stale bucket says nothing about
// the market now, so re-arming on it could re-fire a crossing never left.
func TestSweep_StalePriceDoesNotRearm(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	fired := now.Add(-time.Hour)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: platform.MinAlertCooldownSeconds,
		LastFiredAt:     fired, Disarmed: true,
	}
	alerts := &fakeAlertStore{
		enabled:     []platform.PriceAlert{alert},
		dbLastFired: map[uuid.UUID]time.Time{alert.ID: fired},
		dbDisarmed:  map[uuid.UUID]bool{alert.ID: true},
	}
	prices := fakePrices{price: "0.10", bucket: now.Add(-maxPriceStaleness - time.Minute), ok: true}
	w := New(alerts, &fakeWebhooks{}, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	w.Sweep(context.Background())
	if alerts.rearms != 0 || !alerts.dbDisarmed[alert.ID] {
		t.Errorf("re-armed on a stale price (rearms=%d, disarmed=%v), want still disarmed", alerts.rearms, alerts.dbDisarmed[alert.ID])
	}
}

// TestSweep_RearmRefusedForANewerFire: a sweep whose snapshot predates
// another evaluator's fire must not re-arm that newer fire, or the next
// tick would notify the same crossing again.
func TestSweep_RearmRefusedForANewerFire(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: platform.MinAlertCooldownSeconds,
		LastFiredAt:     now.Add(-time.Hour), Disarmed: true,
	}
	alerts := &fakeAlertStore{
		enabled:     []platform.PriceAlert{alert},
		dbLastFired: map[uuid.UUID]time.Time{alert.ID: now.Add(-time.Second)},
		dbDisarmed:  map[uuid.UUID]bool{alert.ID: true},
	}
	prices := fakePrices{price: "0.10", bucket: now.Add(-time.Minute), ok: true}
	w := New(alerts, &fakeWebhooks{}, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	w.Sweep(context.Background())
	if alerts.rearms != 0 || !alerts.dbDisarmed[alert.ID] {
		t.Errorf("re-armed a fire newer than the snapshot (rearms=%d), want refused", alerts.rearms)
	}
}

// TestSweep_FanOutFailedCompletely_RearmsAndRetries: a claim whose fan-out
// enqueued nothing hands the crossing back, so it is delivered on the first
// tick past the cooldown instead of being lost while the condition holds.
func TestSweep_FanOutFailedCompletely_RearmsAndRetries(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: platform.MinAlertCooldownSeconds,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{
		byAcct: map[uuid.UUID][]platform.CustomerWebhook{
			acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
		},
		enqErr: errors.New("delivery queue down"),
	}
	prices := &stepPrices{price: "0.20"}
	w := New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})
	sweepAfter := func(d time.Duration) {
		now = now.Add(d)
		prices.bucket = now.Add(-time.Minute)
		w.Sweep(context.Background())
	}

	sweepAfter(0)
	if alerts.rearms != 1 || alerts.dbDisarmed[alert.ID] {
		t.Fatalf("after a fully failed fan-out: rearms=%d disarmed=%v, want re-armed", alerts.rearms, alerts.dbDisarmed[alert.ID])
	}
	if got := alerts.dbLastFired[alert.ID]; !got.Equal(now) {
		t.Errorf("last_fired_at = %v, want the claim's stamp %v kept for the cooldown", got, now)
	}

	hooks.enqErr = nil
	sweepAfter(time.Minute) // inside the 300 s cooldown
	if len(hooks.enqueued) != 0 {
		t.Fatalf("enqueued %d inside the cooldown, want 0", len(hooks.enqueued))
	}
	sweepAfter(10 * time.Minute)
	if len(hooks.enqueued) != 1 {
		t.Fatalf("enqueued %d after the cooldown, want the retried crossing delivered once", len(hooks.enqueued))
	}
}

// TestSweep_FanOutFailedPartially_StaysDisarmed: once any webhook received
// the crossing it is spent; re-arming would re-notify that webhook.
func TestSweep_FanOutFailedPartially_StaysDisarmed(t *testing.T) {
	acct := uuid.New()
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	alert := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: platform.MinAlertCooldownSeconds,
	}
	good := priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))
	bad := priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{alert}}
	hooks := &fakeWebhooks{
		byAcct:    map[uuid.UUID][]platform.CustomerWebhook{acct: {good, bad}},
		enqErrFor: map[uuid.UUID]error{bad.ID: errors.New("delivery queue full")},
	}
	prices := &stepPrices{price: "0.20"}
	w := New(alerts, hooks, prices, Options{
		Interval: time.Second,
		Logger:   quietLogger(),
		Clock:    func() time.Time { return now },
	})

	for range 2 {
		prices.bucket = now.Add(-time.Minute)
		w.Sweep(context.Background())
		now = now.Add(10 * time.Minute)
	}
	if alerts.rearms != 0 || !alerts.dbDisarmed[alert.ID] {
		t.Errorf("after a partial fan-out: rearms=%d disarmed=%v, want still disarmed", alerts.rearms, alerts.dbDisarmed[alert.ID])
	}
	if len(hooks.enqueued) != 1 || hooks.enqueued[0].WebhookID != good.ID {
		t.Errorf("enqueued %+v, want exactly one delivery to the healthy webhook", hooks.enqueued)
	}
}
