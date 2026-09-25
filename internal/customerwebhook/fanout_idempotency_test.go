package customerwebhook_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestFanoutPublishOnce_RerunNotifiesOnlyTheMissed — GH-968. An operator
// re-runs `emit-incident` after a partial fan-out. The subscribers already
// queued must not be queued a second time (a second SEV-1 page under a new
// delivery id that receiver dedupe cannot collapse); only the one the
// first run lost is enqueued.
func TestFanoutPublishOnce_RerunNotifiesOnlyTheMissed(t *testing.T) {
	a, b, missed := subscriber(), subscriber(), subscriber()
	store := newFanoutStore(a, b, missed)
	store.failFor[missed.ID] = errors.New("connection reset")
	f := newTestFanout(store)

	const evt = platform.WebhookEventIncidentSEV1
	const key = "db-outage@2026-09-01T10:00:00Z"
	ctx := context.Background()

	first, err := f.PublishOnce(ctx, evt, key, []byte(`{"at":"first"}`))
	if err == nil || first.Enqueued != 2 || first.Failed != 1 {
		t.Fatalf("first run = %+v, %v; want Enqueued 2, Failed 1 and an error", first, err)
	}

	delete(store.failFor, missed.ID)
	// The payload's emit timestamp differs between runs; the key does not.
	second, err := f.PublishOnce(ctx, evt, key, []byte(`{"at":"second"}`))
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if second.Enqueued != 1 || second.AlreadyEnqueued != 2 || second.Failed != 0 {
		t.Errorf("re-run = %+v, want {Enqueued:1 AlreadyEnqueued:2 Failed:0}", second)
	}

	perWebhook := map[uuid.UUID]int{}
	for _, d := range store.enqueued {
		perWebhook[d.WebhookID]++
		if want := customerwebhook.EventDeliveryID(evt, key, d.WebhookID); d.ID != want {
			t.Errorf("webhook %s delivery id = %s, want the event-derived %s", d.WebhookID, d.ID, want)
		}
	}
	for _, sub := range []platform.CustomerWebhook{a, b, missed} {
		if perWebhook[sub.ID] != 1 {
			t.Errorf("webhook %s has %d queued deliveries, want exactly 1", sub.ID, perWebhook[sub.ID])
		}
	}
}

// TestEventDeliveryID_SeparatesEventsAndSubscribers — the key must collapse
// only a re-run of the SAME event to the SAME subscriber.
func TestEventDeliveryID_SeparatesEventsAndSubscribers(t *testing.T) {
	w1, w2 := uuid.New(), uuid.New()
	base := customerwebhook.EventDeliveryID(platform.WebhookEventIncidentSEV1, "k", w1)
	if again := customerwebhook.EventDeliveryID(platform.WebhookEventIncidentSEV1, "k", w1); again != base {
		t.Errorf("same inputs gave %s then %s", base, again)
	}
	for name, other := range map[string]uuid.UUID{
		"other webhook": customerwebhook.EventDeliveryID(platform.WebhookEventIncidentSEV1, "k", w2),
		"other key":     customerwebhook.EventDeliveryID(platform.WebhookEventIncidentSEV1, "k2", w1),
		"other type":    customerwebhook.EventDeliveryID(platform.WebhookEventIncidentResolved, "k", w1),
	} {
		if other == base {
			t.Errorf("%s collides with the base id %s", name, base)
		}
	}
}

func TestFanoutPublishOnce_RefusesEmptyKey(t *testing.T) {
	store := newFanoutStore(subscriber())
	if _, err := newTestFanout(store).PublishOnce(context.Background(),
		platform.WebhookEventIncidentSEV1, "", []byte(`{}`)); err == nil {
		t.Fatal("PublishOnce accepted an empty event key")
	}
	if got := store.enqueuedCount(); got != 0 {
		t.Errorf("enqueued %d rows for an empty key, want 0", got)
	}
}
