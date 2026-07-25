package v1_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-016 (audit-2026-07-23) — Stripe dead-letter.
//
// Two webhook outcomes mean "money landed at Stripe and this system
// provisioned nothing", and both used to end at a log line + a
// processed-mark + 200. Once processed_at is stamped, Stripe stops
// retrying AND the handler's own dedupe dup-acks any manual re-send —
// so recovery depended on a human noticing a WARN. Worse, on the
// all-upgrades-failed path the processed-mark (`SET processed_at =
// now(), error = NULL`) erased the error the upgrade loop had just
// recorded.

// TestStripeWebhook_NoKeys_DeadLetters pins the paid-but-never-signed-up
// outcome: the event is dead-lettered with the right reason, and the
// dedupe row is left REPROCESSABLE so an operator re-sending it once the
// customer exists actually re-runs the provisioning.
func TestStripeWebhook_NoKeys_DeadLetters(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{keys: map[string][]auth.APIKeyRecord{}} // identifier holds nothing
	events := newFakeStripeEventStore()
	audit := &recordingAuditSink{}
	ts := newStripeTestServerWithOptions(t, mgr, events, audit, now)

	body := `{"id":"evt_nokeys","type":"checkout.session.completed","data":{"object":` +
		`{"id":"cs_nokeys","client_reference_id":"signup-ghost","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()

	// 200 is correct: re-delivering cannot help — the customer has never
	// signed up, so the webhook is not the blocked resource.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	open := events.openDeadLetters()
	got, ok := open["evt_nokeys"]
	if !ok {
		t.Fatalf("event was not dead-lettered; open set = %v — a paid customer with nothing provisioned "+
			"must leave a durable, queryable row, not just a log line", open)
	}
	if got != platform.DeadLetterNoKeys {
		t.Errorf("dead-letter reason = %q, want %q", got, platform.DeadLetterNoKeys)
	}
	if pa := events.processedAt("evt_nokeys"); !pa.IsZero() {
		t.Errorf("processed_at = %v, want zero — stamping it makes a manual Stripe re-send a no-op, "+
			"which is the only recovery path this outcome has", pa)
	}

	// The audit surface staff already read carries the incident too.
	var found bool
	for _, e := range audit.entries {
		if e.Action == "plan.dead_letter" && e.TargetID == "evt_nokeys" {
			found = true
		}
	}
	if !found {
		t.Errorf("no plan.dead_letter audit row; got %+v", audit.entries)
	}
}

// TestStripeWebhook_AllUpgradesFailed_DeadLetters pins the second, worse
// outcome: the customer HAS keys, every budget update failed, and pre-fix
// the handler then marked the event processed — wiping the recorded error
// and stamping "complete" on a payment that provisioned nothing.
func TestStripeWebhook_AllUpgradesFailed_DeadLetters(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-unlucky": {
				{KeyID: "kid_a", Identifier: "signup-unlucky", Tier: auth.TierAPIKey, RateLimitPerMin: 1000},
				{KeyID: "kid_b", Identifier: "signup-unlucky", Tier: auth.TierAPIKey, RateLimitPerMin: 1000},
			},
		},
		updateEr: errors.New("key store unreachable"),
	}
	events := newFakeStripeEventStore()
	// A platform bridge too, so the test also pins that the two stores
	// stay independent: a key-store outage must not cost the customer
	// their platform-side tier + dashboard-key upgrade.
	acctID := uuid.New()
	accounts := &fakePlatformAccountsForBridge{
		byStripe: map[string]platform.Account{
			"cus_unlucky": {ID: acctID, Slug: "unlucky-co", StripeCustomerID: "cus_unlucky", Tier: platform.TierFree},
		},
	}
	pgKeys := &fakePlatformAPIKeysForBridge{
		byAcct: map[uuid.UUID][]platform.APIKey{
			acctID: {{ID: "kid_dash", AccountID: acctID, RateLimitPerMin: 1000}},
		},
	}
	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       mgr,
			Events:        events,
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
			Platform:      &v1.StripePlatformBridge{Accounts: accounts, APIKeys: pgKeys},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := `{"id":"evt_allfail","type":"checkout.session.completed","data":{"object":` +
		`{"id":"cs_allfail","client_reference_id":"signup-unlucky","customer":"cus_unlucky",` +
		`"payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	open := events.openDeadLetters()
	got, ok := open["evt_allfail"]
	if !ok {
		t.Fatalf("event was not dead-lettered; open set = %v", open)
	}
	if got != platform.DeadLetterKeyUpgradeFailed {
		t.Errorf("dead-letter reason = %q, want %q", got, platform.DeadLetterKeyUpgradeFailed)
	}
	if pa := events.processedAt("evt_allfail"); !pa.IsZero() {
		t.Errorf("processed_at = %v, want zero — a delivery that applied ZERO budgets must stay "+
			"reprocessable so the next one can complete it", pa)
	}

	pgKeys.mu.Lock()
	defer pgKeys.mu.Unlock()
	if len(pgKeys.updates) != 1 || pgKeys.updates[0].RateLimitPerMin != 10000 {
		t.Errorf("platform key updates = %+v, want the dashboard key lifted to 10000 — the platform "+
			"fan-out must still run when the Redis key store is the thing that failed", pgKeys.updates)
	}
}

// TestStripeWebhook_DeadLetterClosesOnLaterSuccess pins the resolution
// half: a retry that finally provisions clears the OPEN dead-letter, so
// the alert set is "still broken", not "was ever broken".
func TestStripeWebhook_DeadLetterClosesOnLaterSuccess(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-heals": {{KeyID: "kid_h", Identifier: "signup-heals", Tier: auth.TierAPIKey, RateLimitPerMin: 1000}},
		},
		updateEr: errors.New("key store unreachable"),
	}
	events := newFakeStripeEventStore()
	ts := newStripeTestServerWithEvents(t, mgr, events, now)

	body := `{"id":"evt_heals","type":"checkout.session.completed","data":{"object":` +
		`{"id":"cs_heals","client_reference_id":"signup-heals","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	sig := stripeSign(t, body, testStripeSecret, now)

	resp1 := postStripe(t, ts, body, sig)
	resp1.Body.Close()
	if _, ok := events.openDeadLetters()["evt_heals"]; !ok {
		t.Fatal("first delivery should have opened a dead-letter")
	}

	// The store heals; Stripe (or an operator) re-delivers the event. The
	// dedupe row is still reprocessable, so the upgrade runs for real.
	mgr.mu.Lock()
	mgr.updateEr = nil
	mgr.mu.Unlock()

	resp2 := postStripe(t, ts, body, sig)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second delivery status = %d, want 200", resp2.StatusCode)
	}
	mgr.mu.Lock()
	updates := len(mgr.updates)
	applied := 0
	if updates > 0 {
		applied = mgr.updates[0].rateLimit
	}
	mgr.mu.Unlock()
	if updates != 1 {
		t.Fatalf("UpdateRateLimit calls on the retry = %d, want 1 — a dead-lettered event must stay "+
			"reprocessable rather than being dup-acked", updates)
	}
	if applied != 10000 {
		t.Errorf("applied budget = %d, want 10000 (Pro)", applied)
	}
	if open := events.openDeadLetters(); len(open) != 0 {
		t.Errorf("open dead-letters = %v, want empty — a retry that provisioned must close the incident", open)
	}
	if pa := events.processedAt("evt_heals"); pa.IsZero() {
		t.Error("processed_at is still zero after a successful delivery")
	}
}
