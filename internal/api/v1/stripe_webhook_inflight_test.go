package v1_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// errTransientKeyStore stands in for the class of key-store failure a
// Stripe retry heals (a Redis blip, a Postgres fail-over).
var errTransientKeyStore = errors.New("transient key-store failure")

// C3-039 (audit-2026-07-23) — the handler half of the dedupe-claim fix.
//
// The store half (two concurrent deliveries, real Postgres, exactly one
// winner) lives in test/integration/stripe_event_claim_test.go. These tests
// pin what the WEBHOOK does with the store's three answers, which is where
// the money consequence actually lands:
//
//   - claim taken        → process the event
//   - already processed  → 200 duplicate-ack (stop retrying)
//   - in flight          → 409 (KEEP retrying)
//
// The third is the one worth a test of its own. Folding it into the
// duplicate-ack would be the natural-looking shortcut and is exactly wrong:
// a 200 takes the event out of Stripe's retry queue permanently, so if the
// in-flight processor then dies, a customer has paid and nothing was ever
// provisioned — the C3-016 failure mode, re-created by the fix for C3-039.

// TestStripeWebhook_ConcurrentDelivery_IsRetryable409 — a second copy of an
// event arrives while the first is still being processed.
func TestStripeWebhook_ConcurrentDelivery_IsRetryable409(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-concurrent": {{
				KeyID: "kid_conc", Identifier: "signup-concurrent",
				Tier: auth.TierAPIKey, RateLimitPerMin: 1000,
			}},
		},
	}
	events := newFakeStripeEventStore()
	// A delivery that is mid-flight: the row is claimed, processed_at is
	// still zero. This is the state the handler holds for the whole
	// duration of its own work.
	inFlight := platform.StripeEvent{
		StripeEventID: "evt_concurrent",
		Type:          "checkout.session.completed",
		ReceivedAt:    now,
	}
	if err := events.AppendStripeEvent(context.Background(), inFlight); err != nil {
		t.Fatalf("seed the in-flight claim: %v", err)
	}

	ts := newStripeTestServerWithEvents(t, mgr, events, now)
	body := `{"id":"evt_concurrent","type":"checkout.session.completed","data":{"object":{"id":"cs_conc","client_reference_id":"signup-concurrent","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — a 200 here dup-acks the event out of Stripe's retry queue "+
			"while the only processor of it may still die", resp.StatusCode)
	}
	if got := len(mgr.updates); got != 0 {
		t.Errorf("key updates = %d, want 0 — the concurrent delivery must not re-apply the upgrade", got)
	}
	if events.processedAt("evt_concurrent") != (time.Time{}) {
		t.Error("processed_at was stamped by the refused delivery — the in-flight processor owns that")
	}
	if !events.isClaimed("evt_concurrent") {
		t.Error("the refused delivery released someone else's claim")
	}
}

// TestStripeWebhook_ListKeysFailure_ReleasesClaim — the F-1322 path under the
// new claim. A transient key-store failure must leave the row UNCLAIMED, or
// Stripe's retry (which arrives well inside the 5-minute lease) would be
// answered 409 and the paid customer would stay un-upgraded until the lease
// expired. The claim must not become a new way to stall the exact recovery
// F-1322 was written to enable.
func TestStripeWebhook_ListKeysFailure_ReleasesClaim(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-release": {{
				KeyID: "kid_release", Identifier: "signup-release",
				Tier: auth.TierAPIKey, RateLimitPerMin: 1000,
			}},
		},
		listErr: errTransientKeyStore,
	}
	events := newFakeStripeEventStore()
	ts := newStripeTestServerWithEvents(t, mgr, events, now)
	body := `{"id":"evt_release","type":"checkout.session.completed","data":{"object":{"id":"cs_release","client_reference_id":"signup-release","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	sig := stripeSign(t, body, testStripeSecret, now)

	resp := postStripe(t, ts, body, sig)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so Stripe retries", resp.StatusCode)
	}
	if events.isClaimed("evt_release") {
		t.Fatal("the failed attempt kept its claim — the very next Stripe retry would get 409 " +
			"instead of re-running the upgrade (F-1322 regression)")
	}

	// And the retry, once the key store recovers, actually upgrades.
	mgr.mu.Lock()
	mgr.listErr = nil
	mgr.mu.Unlock()

	resp2 := postStripe(t, ts, body, sig)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", resp2.StatusCode)
	}
	if got := len(mgr.updates); got != 1 {
		t.Errorf("retry: key updates = %d, want 1", got)
	}
}

// TestStripeWebhook_BadMetadata_ReleasesClaim — the terminal 400 paths also
// conclude an attempt. Leaving the claim held would answer Stripe's next few
// retries 409 rather than 400, hiding a permanent configuration error behind
// a transient-looking status.
func TestStripeWebhook_BadMetadata_ReleasesClaim(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{keys: map[string][]auth.APIKeyRecord{}}
	events := newFakeStripeEventStore()
	ts := newStripeTestServerWithEvents(t, mgr, events, now)
	body := `{"id":"evt_badtier","type":"checkout.session.completed","data":{"object":{"id":"cs_badtier","client_reference_id":"signup-badtier","payment_status":"paid","metadata":{"tier":"platinum"}}}}`

	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if events.isClaimed("evt_badtier") {
		t.Error("a terminally-rejected event kept its claim")
	}
}
