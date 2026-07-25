package v1_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-014 (audit-2026-07-23) — billing-downgrade enforcement.
//
// `customer.subscription.deleted` used to write `accounts.tier = free`
// and stop. Because the enforced per-minute budget is read straight off
// the key record, an ex-Pro customer kept 10_000/min forever. These
// tests pin the three clamps the downgrade now applies, all against
// `platform.TierFree.MaxRateLimitPerMin()` = 60.

// subscriptionDeletedBody builds the wire event for a terminated
// subscription belonging to `stripeCustomer`.
func subscriptionDeletedBody(eventID, subID, stripeCustomer string) string {
	return `{"id":"` + eventID + `","type":"customer.subscription.deleted","data":{"object":{` +
		`"id":"` + subID + `","customer":"` + stripeCustomer + `","status":"canceled"}}}`
}

// TestStripeWebhook_SubscriptionDeleted_LowersPostgresKeyBudgets is the
// core regression: every active Postgres-backed dashboard key budgeted
// above the Free ceiling is lowered to it, each eviction from the auth
// read-through cache included, while revoked and already-compliant keys
// are untouched.
func TestStripeWebhook_SubscriptionDeleted_LowersPostgresKeyBudgets(t *testing.T) {
	now := time.Now().UTC()
	acctID := uuid.New()
	accounts := &fakePlatformAccountsForBridge{
		byStripe: map[string]platform.Account{
			"cus_expired": {
				ID: acctID, Slug: "expired-co", StripeCustomerID: "cus_expired",
				Tier: platform.TierPro,
			},
		},
	}
	keys := &fakePlatformAPIKeysForBridge{
		byAcct: map[uuid.UUID][]platform.APIKey{
			acctID: {
				{ID: "kid_paid_a", AccountID: acctID, RateLimitPerMin: 10000, KeyHash: []byte{0xaa, 0xbb}},
				{ID: "kid_paid_b", AccountID: acctID, RateLimitPerMin: 50000, KeyHash: []byte{0xcc, 0xdd}},
				// Already at/below the Free ceiling — must not be rewritten.
				{ID: "kid_free", AccountID: acctID, RateLimitPerMin: 60},
				// Revoked — must not be rewritten.
				{ID: "kid_revoked", AccountID: acctID, RateLimitPerMin: 10000, RevokedAt: now.Add(-time.Hour)},
			},
		},
	}
	inv := &fakeKeyCacheInvalidator{}
	mgr := &fakeStripeManager{keys: map[string][]auth.APIKeyRecord{}}

	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       mgr,
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
			Platform: &v1.StripePlatformBridge{
				Accounts:            accounts,
				APIKeys:             keys,
				KeyCacheInvalidator: inv,
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := subscriptionDeletedBody("evt_sub_del_1", "sub_1", "cus_expired")
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	keys.mu.Lock()
	updates := make([]platform.APIKey, len(keys.updates))
	copy(updates, keys.updates)
	keys.mu.Unlock()

	if len(updates) != 2 {
		t.Fatalf("Postgres key Update calls = %d, want 2 (only the two above-ceiling active keys); got %+v",
			len(updates), updates)
	}
	lowered := map[string]int{}
	for _, k := range updates {
		lowered[k.ID] = k.RateLimitPerMin
	}
	for _, id := range []string{"kid_paid_a", "kid_paid_b"} {
		got, ok := lowered[id]
		if !ok {
			t.Errorf("key %q was not lowered", id)
			continue
		}
		if got != 60 {
			t.Errorf("key %q RateLimitPerMin = %d, want 60 (free-tier ceiling)", id, got)
		}
	}
	if _, touched := lowered["kid_free"]; touched {
		t.Error("key kid_free was already at the ceiling and must not be rewritten")
	}
	if _, touched := lowered["kid_revoked"]; touched {
		t.Error("revoked key kid_revoked must not be rewritten")
	}

	// Every lowered key with a stored hash must be evicted from the auth
	// read-through cache, else auth_backend=postgres keeps serving the
	// paid budget for the whole validator TTL.
	if got, want := len(inv.seen()), 2; got != want {
		t.Errorf("cache invalidations = %d (%v), want %d", got, inv.seen(), want)
	}
}

// TestStripeWebhook_SubscriptionDeleted_LowersRedisSelfServiceKeys pins
// the second population: keys minted through POST /v1/account/keys land
// in Redis under `acct:<slug>` (the identifier the Postgres validator
// stamps on the Subject) and inherit the caller's then-paid budget, so
// the downgrade must reach them by that identifier.
func TestStripeWebhook_SubscriptionDeleted_LowersRedisSelfServiceKeys(t *testing.T) {
	now := time.Now().UTC()
	acctID := uuid.New()
	accounts := &fakePlatformAccountsForBridge{
		byStripe: map[string]platform.Account{
			"cus_selfserve": {
				ID: acctID, Slug: "selfserve-co", StripeCustomerID: "cus_selfserve",
				Tier: platform.TierBusiness,
			},
		},
	}
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			auth.AccountIdentifier("selfserve-co"): {
				{KeyID: "kid_rotated", Identifier: auth.AccountIdentifier("selfserve-co"), Tier: auth.TierAPIKey, RateLimitPerMin: 50000},
				{KeyID: "kid_small", Identifier: auth.AccountIdentifier("selfserve-co"), Tier: auth.TierAPIKey, RateLimitPerMin: 30},
			},
		},
	}

	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       mgr,
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
			Platform:      &v1.StripePlatformBridge{Accounts: accounts},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := subscriptionDeletedBody("evt_sub_del_2", "sub_2", "cus_selfserve")
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	mgr.mu.Lock()
	updates := make([]stripeUpdateCall, len(mgr.updates))
	copy(updates, mgr.updates)
	mgr.mu.Unlock()

	if len(updates) != 1 {
		t.Fatalf("Redis UpdateRateLimit calls = %v, want exactly 1 (only the above-ceiling key)", updates)
	}
	if updates[0].keyID != "kid_rotated" {
		t.Errorf("lowered key = %q, want kid_rotated", updates[0].keyID)
	}
	if updates[0].rateLimit != 60 {
		t.Errorf("lowered to %d, want 60 (free-tier ceiling)", updates[0].rateLimit)
	}
}

// TestStripeWebhook_SubscriptionDeleted_ClampsAccountOverride pins the
// third clamp: the account-level rate_limit_per_min_override is an
// account-wide FLOOR at enforcement time, so a comp granted under the
// paid plan would keep flooring every key at the old budget after the
// plan ended. It is clamped to the new tier's ceiling in the same
// Update that writes the tier.
func TestStripeWebhook_SubscriptionDeleted_ClampsAccountOverride(t *testing.T) {
	now := time.Now().UTC()
	acctID := uuid.New()
	accounts := &fakePlatformAccountsForBridge{
		byStripe: map[string]platform.Account{
			"cus_comped": {
				ID: acctID, Slug: "comped-co", StripeCustomerID: "cus_comped",
				Tier: platform.TierPro, RateLimitPerMinOverride: 25000,
			},
		},
	}
	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       &fakeStripeManager{},
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
			Platform:      &v1.StripePlatformBridge{Accounts: accounts},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := subscriptionDeletedBody("evt_sub_del_3", "sub_3", "cus_comped")
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	accounts.mu.Lock()
	updates := make([]platform.Account, len(accounts.updates))
	copy(updates, accounts.updates)
	accounts.mu.Unlock()

	if len(updates) != 1 {
		t.Fatalf("Account Update calls = %d, want 1", len(updates))
	}
	if updates[0].Tier != platform.TierFree {
		t.Errorf("Tier = %q, want free", updates[0].Tier)
	}
	if updates[0].RateLimitPerMinOverride != 60 {
		t.Errorf("RateLimitPerMinOverride = %d, want 60 (clamped to the free-tier ceiling; "+
			"an above-ceiling override survives the downgrade as an account-wide floor otherwise)",
			updates[0].RateLimitPerMinOverride)
	}
}

// TestStripeWebhook_SubscriptionDeleted_BelowCeilingOverrideSurvives
// pins the boundary: an override already at or under the new tier's
// ceiling is a deliberate operator value with no billing consequence and
// must be left alone (and the tier write must still happen).
func TestStripeWebhook_SubscriptionDeleted_BelowCeilingOverrideSurvives(t *testing.T) {
	now := time.Now().UTC()
	acctID := uuid.New()
	accounts := &fakePlatformAccountsForBridge{
		byStripe: map[string]platform.Account{
			"cus_small": {
				ID: acctID, Slug: "small-co", StripeCustomerID: "cus_small",
				Tier: platform.TierPro, RateLimitPerMinOverride: 30,
			},
		},
	}
	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       &fakeStripeManager{},
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
			Platform:      &v1.StripePlatformBridge{Accounts: accounts},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body := subscriptionDeletedBody("evt_sub_del_4", "sub_4", "cus_small")
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	accounts.mu.Lock()
	defer accounts.mu.Unlock()
	if len(accounts.updates) != 1 {
		t.Fatalf("Account Update calls = %d, want 1", len(accounts.updates))
	}
	if got := accounts.updates[0].RateLimitPerMinOverride; got != 30 {
		t.Errorf("RateLimitPerMinOverride = %d, want 30 (below the ceiling — untouched)", got)
	}
}
