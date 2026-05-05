package v1_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/RatesEngine/rates-engine/internal/api/v1"
	"github.com/RatesEngine/rates-engine/internal/auth"
)

// fakeStripeManager is the test double for [v1.StripeKeyManager].
// Records every UpdateRateLimit call so assertions can confirm the
// handler called the right key with the right budget.
type fakeStripeManager struct {
	mu       sync.Mutex
	keys     map[string][]auth.APIKeyRecord // identifier → keys
	updates  []stripeUpdateCall
	listErr  error
	updateEr error
}

type stripeUpdateCall struct {
	keyID     string
	rateLimit int
}

func (f *fakeStripeManager) ListKeysForIdentifier(_ context.Context, identifier string) ([]auth.APIKeyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.keys[identifier], nil
}

func (f *fakeStripeManager) UpdateRateLimit(_ context.Context, keyID string, rateLimit int) (auth.APIKeyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateEr != nil {
		return auth.APIKeyRecord{}, f.updateEr
	}
	f.updates = append(f.updates, stripeUpdateCall{keyID: keyID, rateLimit: rateLimit})
	return auth.APIKeyRecord{KeyID: keyID, RateLimitPerMin: rateLimit}, nil
}

const testStripeSecret = "whsec_test_signing_secret_value"

// stripeSign produces a valid Stripe-Signature header for the body
// at the given timestamp. Mirrors what Stripe's edge does.
func stripeSign(t *testing.T, body, secret string, ts time.Time) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts.Unix(), body)))
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func newStripeTestServer(t *testing.T, mgr v1.StripeKeyManager, now time.Time) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}), // anonymous
		Stripe: &v1.StripeWebhookConfig{
			SigningSecret: testStripeSecret,
			Manager:       mgr,
			Now:           func() time.Time { return now },
			MaxAge:        5 * time.Minute,
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postStripe(t *testing.T, ts *httptest.Server, body, sigHeader string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/webhooks/stripe", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sigHeader != "" {
		req.Header.Set("Stripe-Signature", sigHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestStripeWebhook_HappyPath_Pro(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-abc": {
				{KeyID: "kid_one", Identifier: "signup-abc", Tier: auth.TierAPIKey, RateLimitPerMin: 1000},
				{KeyID: "kid_two", Identifier: "signup-abc", Tier: auth.TierAPIKey, RateLimitPerMin: 1000},
			},
		},
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_1","client_reference_id":"signup-abc","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	sig := stripeSign(t, body, testStripeSecret, now)
	resp := postStripe(t, ts, body, sig)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(mgr.updates) != 2 {
		t.Errorf("updates = %d, want 2", len(mgr.updates))
	}
	for _, u := range mgr.updates {
		if u.rateLimit != 10000 {
			t.Errorf("upgrade keyID=%s ratelimit=%d, want 10000 (Pro)", u.keyID, u.rateLimit)
		}
	}
}

func TestStripeWebhook_BusinessTier(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-x": {{KeyID: "kid_x", Identifier: "signup-x", Tier: auth.TierAPIKey, RateLimitPerMin: 1000}},
		},
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt_2","type":"checkout.session.completed","data":{"object":{"id":"cs_2","client_reference_id":"signup-x","payment_status":"paid","metadata":{"tier":"business"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(mgr.updates) != 1 || mgr.updates[0].rateLimit != 50000 {
		t.Errorf("expected 1 upgrade to 50000; got %v", mgr.updates)
	}
}

func TestStripeWebhook_OverrideRateLimit(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-ent": {{KeyID: "kid_ent", Identifier: "signup-ent", Tier: auth.TierAPIKey, RateLimitPerMin: 1000}},
		},
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt_3","type":"checkout.session.completed","data":{"object":{"id":"cs_3","client_reference_id":"signup-ent","payment_status":"paid","metadata":{"rate_limit_per_min":"100000"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(mgr.updates) != 1 || mgr.updates[0].rateLimit != 100000 {
		t.Errorf("expected 1 upgrade to 100000; got %v", mgr.updates)
	}
}

func TestStripeWebhook_BadSignature(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"client_reference_id":"x","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, fmt.Sprintf("t=%d,v1=deadbeef", now.Unix()))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if len(mgr.updates) != 0 {
		t.Errorf("must not call manager on bad signature; got %d", len(mgr.updates))
	}
}

func TestStripeWebhook_ReplayProtection(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed"}`
	stale := now.Add(-10 * time.Minute) // > 5 min default MaxAge
	sig := stripeSign(t, body, testStripeSecret, stale)
	resp := postStripe(t, ts, body, sig)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (replay)", resp.StatusCode)
	}
}

func TestStripeWebhook_MissingSignatureHeader(t *testing.T) {
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, time.Now().UTC())
	resp := postStripe(t, ts, `{}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestStripeWebhook_NoConfig(t *testing.T) {
	srv := v1.New(v1.Options{Auth: fakeAuthMiddleware(auth.Subject{})})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := postStripe(t, ts, `{}`, "t=1,v1=x")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestStripeWebhook_IgnoresOtherEventTypes(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"customer.created","data":{"object":{}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (acknowledge so Stripe stops retrying)", resp.StatusCode)
	}
	if len(mgr.updates) != 0 {
		t.Errorf("must not upgrade for non-checkout events; got %d", len(mgr.updates))
	}
}

func TestStripeWebhook_UnpaidIgnored(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"id":"cs","client_reference_id":"x","payment_status":"unpaid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(mgr.updates) != 0 {
		t.Errorf("must not upgrade unpaid sessions; got %d", len(mgr.updates))
	}
}

func TestStripeWebhook_MissingClientReference(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"id":"cs","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestStripeWebhook_BadTierAndNoOverride(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"x": {{KeyID: "kid", Identifier: "x"}},
		},
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"id":"cs","client_reference_id":"x","payment_status":"paid","metadata":{"tier":"hyper"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestStripeWebhook_NoKeysForIdentifier(t *testing.T) {
	now := time.Now().UTC()
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{}, // empty
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"id":"cs","client_reference_id":"signup-unknown","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	// Acknowledge — refusing would just trigger Stripe retries.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (acknowledge so Stripe stops retrying)", resp.StatusCode)
	}
}

func TestStripeWebhook_PartialUpgradeFailure(t *testing.T) {
	// One upgrade fails — the others still go through; webhook
	// returns 200 to prevent Stripe retrying everything.
	now := time.Now().UTC()
	failing := errors.New("redis blip")
	mgr := &fakeStripeManager{
		keys: map[string][]auth.APIKeyRecord{
			"signup-y": {{KeyID: "kid_a"}, {KeyID: "kid_b"}},
		},
		updateEr: failing,
	}
	ts := newStripeTestServer(t, mgr, now)
	body := `{"id":"evt","type":"checkout.session.completed","data":{"object":{"id":"cs","client_reference_id":"signup-y","payment_status":"paid","metadata":{"tier":"pro"}}}}`
	resp := postStripe(t, ts, body, stripeSign(t, body, testStripeSecret, now))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (partial-upgrade is reported, not failed)", resp.StatusCode)
	}
}
