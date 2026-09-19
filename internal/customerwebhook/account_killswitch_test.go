// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// SEC-06 / RLT-420: the account kill switch was INBOUND-ONLY. Suspending
// or closing an account stopped its API keys authenticating, but nothing
// in the webhook fan-out, claim or delivery path read account status —
// so a suspended or closed customer kept RECEIVING our data at the
// endpoints they had registered, and rows kept being queued against
// them.
//
// These tests pin the delivery end of the gate. The store end (the
// resolver, the two enqueue writers and the claim query, all SQL) is
// pinned against real Postgres in
// test/integration/customerwebhook_account_killswitch_test.go.

// WebhookAccountStatus completes the DeliveryStore contract for the
// package's in-memory fake. That fake models the customer_webhooks table
// only and has no notion of an owning account, so every account it knows
// is active — which is what the pre-existing tests around it assume.
// Non-active statuses are driven through [killSwitchStore] below.
func (s *fakeStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	return platform.AccountActive, nil
}

// killSwitchStore is the package fake with an owning account bolted on:
// every webhook it serves belongs to an account with `status`, or to one
// whose status cannot be read when `err` is set.
type killSwitchStore struct {
	*fakeStore
	status platform.AccountStatus
	err    error
	// calls counts WebhookAccountStatus lookups, so a test can tell "the
	// gate ran and allowed it" from "the gate was never consulted".
	calls atomic.Int64
}

func (s *killSwitchStore) WebhookAccountStatus(context.Context, uuid.UUID) (platform.AccountStatus, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.status, nil
}

// killSwitchFixture stands up an endpoint that counts the POSTs it
// receives, an enabled webhook pointing at it, and one due delivery,
// then drains a single tick with the account in `status`. It returns the
// store and the number of requests the customer's endpoint actually
// received.
func killSwitchFixture(
	t *testing.T, status platform.AccountStatus, statusErr error,
) (*killSwitchStore, uuid.UUID, *int64) {
	t.Helper()

	var posts int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&posts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	base := newFakeStore()
	webhookID := uuid.New()
	base.addWebhook(platform.CustomerWebhook{
		ID:         webhookID,
		AccountID:  uuid.New(),
		URL:        ts.URL,
		SecretHash: []byte("test-secret-bytes"),
		Enabled:    true,
	})
	deliveryID := uuid.New()
	base.enqueue(platform.WebhookDelivery{
		ID:            deliveryID,
		WebhookID:     webhookID,
		EventType:     string(platform.WebhookEventIncidentSEV1),
		Payload:       []byte(`{"incident_id":"abc"}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})

	store := &killSwitchStore{fakeStore: base, status: status, err: statusErr}
	// The production client's SSRF guard rejects 127.0.0.1, which is
	// where httptest lives; the package's other tests use the same
	// permissive client for that reason.
	w := customerwebhook.New(store, customerwebhook.Options{
		PollInterval: 30 * time.Millisecond,
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	return store, deliveryID, &posts
}

// TestWorker_SuspendedAccountIsNeverPOSTed is the core regression. Before
// the fix the worker's only gate was `wh.Enabled` — the CUSTOMER's
// switch — so a suspended account's enabled webhook was signed and
// POSTed exactly like an active one's.
func TestWorker_SuspendedAccountIsNeverPOSTed(t *testing.T) {
	store, deliveryID, posts := killSwitchFixture(t, platform.AccountSuspended, nil)

	if got := atomic.LoadInt64(posts); got != 0 {
		t.Errorf("customer endpoint received %d POST(s) for a SUSPENDED account, want 0 — "+
			"the account kill switch does not reach the outbound path", got)
	}
	if store.calls.Load() == 0 {
		t.Error("WebhookAccountStatus was never consulted; the delivery path has no account gate")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.delivered[deliveryID]; ok {
		t.Error("delivery marked delivered for a suspended account")
	}
	// Suspension is reversible (AccountStore.Unsuspend), so the row is
	// PARKED for lease expiry, not destroyed: nothing is recorded against
	// it and it is still there to deliver if the account is reinstated.
	if fails := store.failures[deliveryID]; len(fails) != 0 {
		t.Errorf("suspended delivery recorded %d outcome(s), want 0 (park, do not destroy): %+v",
			len(fails), fails)
	}
}

// TestWorker_ClosedAccountIsTerminallyFailed — a closed account is not
// coming back, so its queued events are terminally failed rather than
// parked, and still never POSTed.
func TestWorker_ClosedAccountIsTerminallyFailed(t *testing.T) {
	store, deliveryID, posts := killSwitchFixture(t, platform.AccountClosed, nil)

	if got := atomic.LoadInt64(posts); got != 0 {
		t.Errorf("customer endpoint received %d POST(s) for a CLOSED account, want 0", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	fails := store.failures[deliveryID]
	if len(fails) != 1 {
		t.Fatalf("closed-account delivery recorded %d outcome(s), want exactly 1 terminal: %+v",
			len(fails), fails)
	}
	if !fails[0].terminal {
		t.Errorf("closed-account delivery was rescheduled for %s, want terminal", fails[0].nextAt)
	}
	if fails[0].msg != "owning account is closed" {
		t.Errorf("recorded reason = %q, want %q", fails[0].msg, "owning account is closed")
	}
}

// TestWorker_UnreadableAccountStatusFailsClosed — an unresolved status is
// not evidence of an active account. The delivery is withheld and left
// for lease expiry (a Postgres blip delays deliveries; it must not POST
// to a customer we may have just suspended, nor destroy the row).
func TestWorker_UnreadableAccountStatusFailsClosed(t *testing.T) {
	store, deliveryID, posts := killSwitchFixture(t, "", errors.New("connection reset"))

	if got := atomic.LoadInt64(posts); got != 0 {
		t.Errorf("customer endpoint received %d POST(s) on an UNREADABLE account status, want 0", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.failures[deliveryID]) != 0 {
		t.Errorf("unreadable status recorded an outcome, want the row left for lease expiry: %+v",
			store.failures[deliveryID])
	}
	if _, ok := store.delivered[deliveryID]; ok {
		t.Error("delivery marked delivered despite an unreadable account status")
	}
}

// TestWorker_ActiveAccountStillDelivers is the other half of the gate:
// the fix must not cost an active customer their events.
func TestWorker_ActiveAccountStillDelivers(t *testing.T) {
	store, deliveryID, posts := killSwitchFixture(t, platform.AccountActive, nil)

	if got := atomic.LoadInt64(posts); got != 1 {
		t.Errorf("active account's endpoint received %d POST(s), want 1", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if status, ok := store.delivered[deliveryID]; !ok || status != http.StatusOK {
		t.Errorf("active delivery not marked delivered: delivered=%v", store.delivered)
	}
}

// storeWithoutAccountGate satisfies DeliveryStore and nothing else — it
// cannot answer the account kill switch.
type storeWithoutAccountGate struct{}

func (storeWithoutAccountGate) ListPendingDeliveries(context.Context, int) ([]platform.WebhookDelivery, error) {
	return nil, nil
}

func (storeWithoutAccountGate) GetWebhook(context.Context, uuid.UUID) (platform.CustomerWebhook, error) {
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (storeWithoutAccountGate) MarkDelivered(context.Context, uuid.UUID, int) error { return nil }

func (storeWithoutAccountGate) MarkAttemptFailed(
	context.Context, uuid.UUID, string, int, time.Time,
) error {
	return nil
}

// TestNew_RefusesAStoreThatCannotAnswerTheKillSwitch — the requirement is
// enforced at construction. A store that silently skipped the account
// check would restore the inbound-only kill switch without a single test
// going red.
func TestNew_RefusesAStoreThatCannotAnswerTheKillSwitch(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a store with no account-status reader; " +
				"the kill switch must not be bypassable")
		}
	}()
	_ = customerwebhook.New(storeWithoutAccountGate{}, customerwebhook.Options{})
}

// TestFanout_SuppressedEnqueueIsNotALostEvent — the enqueue gate's
// refusal is a POLICY decision, not a lost customer event. Before the
// fix every enqueue error alike was counted as a permanently lost event,
// returned as a Publish error, and fed the alertable loss counter — so
// the kill switch doing its job would have paged an operator.
//
// The positive half (the refusal lands in PublishResult.Suppressed) is
// asserted end-to-end against real Postgres in
// test/integration/customerwebhook_account_killswitch_test.go; keeping
// it out of this file lets the file compile — and so fail on its
// assertions rather than on a build error — against the pre-fix code.
func TestFanout_SuppressedEnqueueIsNotALostEvent(t *testing.T) {
	live, gone := subscriber(), subscriber()
	store := newFanoutStore(live, gone)
	store.failFor[gone.ID] = suppressedEnqueueErr{}

	before := fanoutFailures(t, platform.WebhookEventIncidentSEV1, obs.FanoutFailureEnqueue)
	res, err := newTestFanout(store).Publish(
		context.Background(), platform.WebhookEventIncidentSEV1, []byte(`{"a":1}`))
	after := fanoutFailures(t, platform.WebhookEventIncidentSEV1, obs.FanoutFailureEnqueue)

	if err != nil {
		t.Errorf("Publish returned an error for a suppressed delivery: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d for a suppressed delivery, want 0 — "+
			"Failed means the customer LOST the event and is alerted on", res.Failed)
	}
	if res.Enqueued != 1 {
		t.Errorf("Enqueued = %d, want 1 (the still-active subscriber)", res.Enqueued)
	}
	if after != before {
		t.Errorf("lost-event counter moved %v→%v for a deliberate suppression", before, after)
	}
}

// suppressedEnqueueErr mimics what the Postgres store returns when the
// owning account went non-active between the resolve and the insert.
type suppressedEnqueueErr struct{}

func (suppressedEnqueueErr) Error() string           { return "account is suspended, not active" }
func (suppressedEnqueueErr) WebhookSuppressed() bool { return true }
