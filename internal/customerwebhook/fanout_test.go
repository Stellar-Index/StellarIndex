package customerwebhook_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-023 (audit-2026-07-23).
//
// `Fanout.Publish` used to have NO return value: a per-endpoint enqueue
// failure logged WARN + continued, and a whole-fan-out failure (subscriber
// list unavailable) logged WARN + returned. The producer — the aggregator's
// freeze and divergence hot paths, and `stellarindex-ops emit-incident` —
// could not tell a fan-out that reached five subscribers from one that
// reached none.
//
// That loss is PERMANENT in a way a delivery failure is not: no
// `webhook_deliveries` row was ever written, so the retry worker has nothing
// to drain and nothing downstream re-derives it. These tests pin the two
// halves of the fix — the (result, error) contract callers act on, and the
// zero-seeded counter an operator alerts on.

// fanoutStore is an in-memory FanoutStore whose two methods can be made to
// fail independently, and per-webhook for the enqueue path.
type fanoutStore struct {
	mu        sync.Mutex
	subs      []platform.CustomerWebhook
	listErr   error
	failFor   map[uuid.UUID]error // webhook id → enqueue error
	enqueued  []platform.WebhookDelivery
	listCalls int
}

func newFanoutStore(subs ...platform.CustomerWebhook) *fanoutStore {
	return &fanoutStore{subs: subs, failFor: map[uuid.UUID]error{}}
}

func (s *fanoutStore) ListWebhooksSubscribedTo(
	_ context.Context, _ platform.WebhookEventType,
) ([]platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]platform.CustomerWebhook(nil), s.subs...), nil
}

func (s *fanoutStore) EnqueueDelivery(_ context.Context, d platform.WebhookDelivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failFor[d.WebhookID]; err != nil {
		return err
	}
	s.enqueued = append(s.enqueued, d)
	return nil
}

func (s *fanoutStore) enqueuedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.enqueued)
}

func newTestFanout(store customerwebhook.FanoutStore) *customerwebhook.Fanout {
	return customerwebhook.NewFanout(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func subscriber() platform.CustomerWebhook {
	return platform.CustomerWebhook{ID: uuid.New(), Enabled: true}
}

func fanoutFailures(t *testing.T, eventType platform.WebhookEventType, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.CustomerWebhookFanoutFailuresTotal.
		WithLabelValues(string(eventType), reason))
}

// TestFanoutPublish_TotalEnqueueFailureIsReported — the headline defect: the
// store rejects every insert, so every subscribed customer loses the event,
// and pre-fix the caller received nothing at all.
func TestFanoutPublish_TotalEnqueueFailureIsReported(t *testing.T) {
	a, b := subscriber(), subscriber()
	store := newFanoutStore(a, b)
	store.failFor[a.ID] = errors.New("deadlock detected")
	store.failFor[b.ID] = errors.New("deadlock detected")
	f := newTestFanout(store)

	const evt = platform.WebhookEventAnomalyFreeze
	before := fanoutFailures(t, evt, obs.FanoutFailureEnqueue)

	res, err := f.Publish(context.Background(), evt, []byte(`{"event":"anomaly.freeze"}`))
	if err == nil {
		t.Fatal("Publish returned nil error after losing every delivery — the caller cannot act on a silent total failure")
	}
	if res.Subscribers != 2 {
		t.Errorf("Subscribers = %d, want 2", res.Subscribers)
	}
	if res.Enqueued != 0 {
		t.Errorf("Enqueued = %d, want 0", res.Enqueued)
	}
	if res.Failed != 2 {
		t.Errorf("Failed = %d, want 2", res.Failed)
	}
	// One increment per LOST delivery, not per fan-out.
	if got, want := fanoutFailures(t, evt, obs.FanoutFailureEnqueue), before+2; got != want {
		t.Errorf("customer_webhook_fanout_failures_total{event_type=%q,reason=%q} = %v, want %v",
			evt, obs.FanoutFailureEnqueue, got, want)
	}
}

// TestFanoutPublish_PartialFailureIsReported — the subtler half: four of five
// customers got their event. The fifth lost it permanently, so a partial
// fan-out is an error too, with the counts to tell the caller how bad it was.
func TestFanoutPublish_PartialFailureIsReported(t *testing.T) {
	good1, good2, bad := subscriber(), subscriber(), subscriber()
	store := newFanoutStore(good1, bad, good2)
	store.failFor[bad.ID] = errors.New("unique violation")
	f := newTestFanout(store)

	const evt = platform.WebhookEventDivergenceFiring
	before := fanoutFailures(t, evt, obs.FanoutFailureEnqueue)

	res, err := f.Publish(context.Background(), evt, []byte(`{"event":"divergence.firing"}`))
	if err == nil {
		t.Fatal("Publish returned nil error while one subscriber's delivery was lost")
	}
	if res.Subscribers != 3 || res.Enqueued != 2 || res.Failed != 1 {
		t.Errorf("result = %+v, want {Subscribers:3 Enqueued:2 Failed:1}", res)
	}
	// The healthy subscribers must still have been enqueued — the error
	// reports loss, it does not abort the fan-out.
	if got := store.enqueuedCount(); got != 2 {
		t.Errorf("enqueued rows = %d, want 2 (a failure for one subscriber must not abort the rest)", got)
	}
	if got, want := fanoutFailures(t, evt, obs.FanoutFailureEnqueue), before+1; got != want {
		t.Errorf("fanout_failures{reason=enqueue} = %v, want %v", got, want)
	}
}

// TestFanoutPublish_ListFailureIsReported — the subscriber set is unknown, so
// nothing was enqueued for anybody.
func TestFanoutPublish_ListFailureIsReported(t *testing.T) {
	store := newFanoutStore(subscriber())
	store.listErr = errors.New("connection refused")
	f := newTestFanout(store)

	const evt = platform.WebhookEventIncidentSEV1
	before := fanoutFailures(t, evt, obs.FanoutFailureListSubscribers)

	res, err := f.Publish(context.Background(), evt, []byte(`{"event":"incident.sev1"}`))
	if err == nil {
		t.Fatal("Publish returned nil error when the subscriber list could not be read")
	}
	if !errors.Is(err, store.listErr) {
		t.Errorf("error does not wrap the store failure: %v", err)
	}
	if res.Subscribers != 0 || res.Enqueued != 0 || res.Failed != 0 {
		t.Errorf("result = %+v, want all-zero on a short-circuit", res)
	}
	if got, want := fanoutFailures(t, evt, obs.FanoutFailureListSubscribers), before+1; got != want {
		t.Errorf("fanout_failures{reason=list_subscribers} = %v, want %v", got, want)
	}
}

// TestFanoutPublish_InvalidPayloadIsReported — a malformed payload loses the
// event for every subscriber before the store is even consulted.
func TestFanoutPublish_InvalidPayloadIsReported(t *testing.T) {
	store := newFanoutStore(subscriber())
	f := newTestFanout(store)

	const evt = platform.WebhookEventIncidentResolved
	before := fanoutFailures(t, evt, obs.FanoutFailureInvalidPayload)

	res, err := f.Publish(context.Background(), evt, []byte(`{not json`))
	if err == nil {
		t.Fatal("Publish returned nil error for a non-JSON payload")
	}
	if res.Enqueued != 0 {
		t.Errorf("Enqueued = %d, want 0", res.Enqueued)
	}
	if store.listCalls != 0 {
		t.Errorf("store consulted %d times for an invalid payload, want 0", store.listCalls)
	}
	if got, want := fanoutFailures(t, evt, obs.FanoutFailureInvalidPayload), before+1; got != want {
		t.Errorf("fanout_failures{reason=invalid_payload} = %v, want %v", got, want)
	}
}

// TestFanoutPublish_SuccessIsSilent — the healthy path must return nil and
// leave every failure series untouched, or the alert built on the counter
// fires forever and the error return trains callers to ignore it.
func TestFanoutPublish_SuccessIsSilent(t *testing.T) {
	a, b := subscriber(), subscriber()
	store := newFanoutStore(a, b)
	f := newTestFanout(store)

	const evt = platform.WebhookEventAnomalyFreeze
	before := map[string]float64{}
	for _, reason := range []string{
		obs.FanoutFailureEnqueue, obs.FanoutFailureListSubscribers, obs.FanoutFailureInvalidPayload,
	} {
		before[reason] = fanoutFailures(t, evt, reason)
	}

	res, err := f.Publish(context.Background(), evt, []byte(`{"event":"anomaly.freeze"}`))
	if err != nil {
		t.Fatalf("Publish returned %v on the healthy path", err)
	}
	if res.Subscribers != 2 || res.Enqueued != 2 || res.Failed != 0 {
		t.Errorf("result = %+v, want {Subscribers:2 Enqueued:2 Failed:0}", res)
	}
	for reason, want := range before {
		if got := fanoutFailures(t, evt, reason); got != want {
			t.Errorf("fanout_failures{reason=%q} moved on the healthy path: %v → %v", reason, want, got)
		}
	}
}

// TestFanoutPublish_NoSubscribersIsSuccess — nobody asked for this event, so
// nothing was lost. Must not be an error (the freeze path fires constantly
// with zero subscribers on most deployments).
func TestFanoutPublish_NoSubscribersIsSuccess(t *testing.T) {
	f := newTestFanout(newFanoutStore())

	res, err := f.Publish(context.Background(), platform.WebhookEventAnomalyFreeze,
		[]byte(`{"event":"anomaly.freeze"}`))
	if err != nil {
		t.Fatalf("Publish returned %v for a zero-subscriber fan-out", err)
	}
	if res.Subscribers != 0 || res.Enqueued != 0 || res.Failed != 0 {
		t.Errorf("result = %+v, want all-zero", res)
	}
}

// TestFanoutPublish_NilReceiverIsTyped — the "webhook subsystem not wired in
// this deployment" case is distinguishable from a real loss.
func TestFanoutPublish_NilReceiverIsTyped(t *testing.T) {
	var f *customerwebhook.Fanout
	res, err := f.Publish(context.Background(), platform.WebhookEventAnomalyFreeze, []byte(`{}`))
	if !errors.Is(err, customerwebhook.ErrFanoutNotConfigured) {
		t.Errorf("err = %v, want ErrFanoutNotConfigured", err)
	}
	if res.Subscribers != 0 {
		t.Errorf("result = %+v, want all-zero", res)
	}
}

// TestFanoutFailureSeries_ZeroSeeded — every platform.WebhookEventType is
// pre-seeded against every reason. internal/obs cannot import
// internal/platform (layering), so the seed list there is a literal copy of
// this enum; this test is what keeps the two from drifting. A missing series
// reads as "no data" on the alert, which is exactly the silence C3-023 is
// about.
func TestFanoutFailureSeries_ZeroSeeded(t *testing.T) {
	for _, evt := range []platform.WebhookEventType{
		platform.WebhookEventIncidentSEV1,
		platform.WebhookEventIncidentResolved,
		platform.WebhookEventAnomalyFreeze,
		platform.WebhookEventDivergenceFiring,
		platform.WebhookEventPriceAlert,
	} {
		for _, reason := range []string{
			obs.FanoutFailureInvalidPayload,
			obs.FanoutFailureListSubscribers,
			obs.FanoutFailureEnqueue,
		} {
			// Deliberately NOT via WithLabelValues — that would CREATE
			// the child and make the assertion vacuous. Gather what the
			// registry already holds.
			if !seriesPresent(t, string(evt), reason) {
				t.Errorf("series {event_type=%q,reason=%q} is not pre-seeded in internal/obs — "+
					"a new WebhookEventType was added without extending the seed list", evt, reason)
			}
		}
	}
}

// seriesPresent reports whether the (event_type, reason) series already
// exists in the default registry's gathered output — i.e. whether it was
// seeded at package init rather than created by this test's own lookup.
func seriesPresent(t *testing.T, eventType, reason string) bool {
	t.Helper()
	families, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "stellarindex_customer_webhook_fanout_failures_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			var gotEvent, gotReason string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "event_type":
					gotEvent = lp.GetValue()
				case "reason":
					gotReason = lp.GetValue()
				}
			}
			if gotEvent == eventType && gotReason == reason {
				return true
			}
		}
	}
	return false
}
