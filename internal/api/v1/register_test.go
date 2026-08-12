// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fakeRegisterAccountStore implements the full [platform.AccountStore]
// so the same instance can back both the register handler (Create)
// and the Postgres API-key validator (Get) in the key-works-after-
// mint test.
type fakeRegisterAccountStore struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]platform.Account
	created []platform.Account
}

func newFakeRegisterAccountStore() *fakeRegisterAccountStore {
	return &fakeRegisterAccountStore{byID: map[uuid.UUID]platform.Account{}}
}

func (f *fakeRegisterAccountStore) Create(_ context.Context, a platform.Account) (platform.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.ID = uuid.New()
	a.CreatedAt = time.Now().UTC()
	f.byID[a.ID] = a
	f.created = append(f.created, a)
	return a, nil
}

func (f *fakeRegisterAccountStore) Get(_ context.Context, id uuid.UUID) (platform.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[id]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

func (f *fakeRegisterAccountStore) GetBySlug(_ context.Context, slug string) (platform.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.byID {
		if a.Slug == slug {
			return a, nil
		}
	}
	return platform.Account{}, platform.ErrNotFound
}

func (f *fakeRegisterAccountStore) Update(_ context.Context, a platform.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[a.ID] = a
	return nil
}

func (f *fakeRegisterAccountStore) Suspend(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
func (f *fakeRegisterAccountStore) Unsuspend(_ context.Context, _ uuid.UUID) error { return nil }

// fakeRegisterKeyStore implements [platform.APIKeyStore] with an
// in-memory map, recording the maxActiveKeysPerAccount the handler
// passed so the tier-ceiling assertion is direct.
type fakeRegisterKeyStore struct {
	mu          sync.Mutex
	byID        map[string]platform.APIKey
	lastMaxKeys int
}

func newFakeRegisterKeyStore() *fakeRegisterKeyStore {
	return &fakeRegisterKeyStore{byID: map[string]platform.APIKey{}}
}

func (f *fakeRegisterKeyStore) Create(_ context.Context, k platform.APIKey, maxActive int) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k.CreatedAt = time.Now().UTC()
	f.byID[k.ID] = k
	f.lastMaxKeys = maxActive
	return k, nil
}

func (f *fakeRegisterKeyStore) Get(_ context.Context, id string) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byID[id]
	if !ok {
		return platform.APIKey{}, platform.ErrNotFound
	}
	return k, nil
}

func (f *fakeRegisterKeyStore) GetByHash(_ context.Context, hash []byte) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.byID {
		if bytes.Equal(k.KeyHash, hash) {
			return k, nil
		}
	}
	return platform.APIKey{}, platform.ErrNotFound
}

func (f *fakeRegisterKeyStore) ListForAccount(_ context.Context, accountID uuid.UUID) ([]platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []platform.APIKey
	for _, k := range f.byID {
		if k.AccountID == accountID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeRegisterKeyStore) Update(_ context.Context, k platform.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[k.ID] = k
	return nil
}

func (f *fakeRegisterKeyStore) Revoke(_ context.Context, _ string, _ uuid.UUID, _ string) error {
	return nil
}

func (f *fakeRegisterKeyStore) TouchUsage(_ context.Context, _ string, _ net.IP, _ string) error {
	return nil
}

// fakeRegisterIPThrottle is the [v1.SignupIPThrottle] double: returns
// the configured error (nil = allow) and records every checked IP.
type fakeRegisterIPThrottle struct {
	mu  sync.Mutex
	err error
	ips []string
}

func (f *fakeRegisterIPThrottle) CheckIP(_ context.Context, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips = append(f.ips, ip)
	return f.err
}

func newRegisterTestServer(
	t *testing.T,
	accounts v1.RegisterAccountCreator,
	keys platform.APIKeyStore,
	throttle v1.SignupIPThrottle,
	mirrors ...v1.KeyMirror,
) *httptest.Server {
	t.Helper()
	budgets := v1.APIKeyBudgetStores{Platform: keys}
	if len(mirrors) > 0 {
		budgets.RedisMirror = mirrors[0]
	}
	srv := v1.New(v1.Options{
		Auth:             fakeAuthMiddleware(auth.Subject{}), // anonymous
		RegisterAccounts: accounts,
		APIKeyBudgets:    budgets,
		SignupIPThrottle: throttle,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postRegister(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/v1/register", rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/register: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeRegisterResult(t *testing.T, resp *http.Response) v1.RegisterResult {
	t.Helper()
	var env struct {
		Data v1.RegisterResult `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode register envelope: %v", err)
	}
	return env.Data
}

// TestRegister_HappyPath — one unauthenticated POST creates a
// free-tier account + first key and returns the free-tier limits.
func TestRegister_HappyPath(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	throttle := &fakeRegisterIPThrottle{}
	ts := newRegisterTestServer(t, accounts, keys, throttle)

	resp := postRegister(t, ts.URL, "application/json",
		`{"name":"claude-agent","email":"Agent@Example.com"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	got := decodeRegisterResult(t, resp)

	// Wire shape.
	if _, err := uuid.Parse(got.AccountID); err != nil {
		t.Errorf("account_id %q is not a UUID: %v", got.AccountID, err)
	}
	if !strings.HasPrefix(got.APIKey, "sip_") || len(got.APIKey) != len("sip_")+64 {
		t.Errorf("api_key = %q, want sip_<64hex>", got.APIKey)
	}
	if got.Tier != "free" {
		t.Errorf("tier = %q, want free", got.Tier)
	}
	wantLimits := v1.RegisterLimits{
		RateLimitPerMin: platform.TierFree.MaxRateLimitPerMin(),
		MonthlyQuota:    platform.TierFree.MaxMonthlyQuota(),
		MaxActiveKeys:   platform.TierFree.MaxActiveKeys(),
		MaxWebhooks:     platform.TierFree.MaxWebhooks(),
		MaxPriceAlerts:  platform.TierFree.MaxPriceAlerts(),
	}
	if got.Limits != wantLimits {
		t.Errorf("limits = %+v, want %+v", got.Limits, wantLimits)
	}

	// Persisted account.
	if len(accounts.created) != 1 {
		t.Fatalf("accounts created = %d, want 1", len(accounts.created))
	}
	acct := accounts.created[0]
	if acct.Tier != platform.TierFree || acct.Status != platform.AccountActive {
		t.Errorf("account tier/status = %s/%s, want free/active", acct.Tier, acct.Status)
	}
	if acct.Name != "claude-agent" {
		t.Errorf("account name = %q", acct.Name)
	}
	if acct.BillingEmail != "agent@example.com" {
		t.Errorf("billing email = %q, want lowercased agent@example.com", acct.BillingEmail)
	}
	if !strings.HasPrefix(acct.Slug, "reg-") {
		t.Errorf("slug = %q, want reg-<hex>", acct.Slug)
	}

	// Persisted key: free-tier ceilings applied at mint, and the
	// active-key cap passed through to the store's atomic check.
	key, err := keys.Get(context.Background(), got.KeyID)
	if err != nil {
		t.Fatalf("minted key %q not found in store: %v", got.KeyID, err)
	}
	if key.RateLimitPerMin != platform.TierFree.MaxRateLimitPerMin() {
		t.Errorf("key rate_limit_per_min = %d, want %d", key.RateLimitPerMin, platform.TierFree.MaxRateLimitPerMin())
	}
	if key.MonthlyQuota != platform.TierFree.MaxMonthlyQuota() {
		t.Errorf("key monthly_quota = %d, want %d (metered at the tier ceiling from birth)",
			key.MonthlyQuota, platform.TierFree.MaxMonthlyQuota())
	}
	if keys.lastMaxKeys != platform.TierFree.MaxActiveKeys() {
		t.Errorf("maxActiveKeysPerAccount passed to Create = %d, want %d",
			keys.lastMaxKeys, platform.TierFree.MaxActiveKeys())
	}
	if throttle.ips == nil {
		t.Error("signup IP throttle was never consulted")
	}
}

// TestRegister_EmptyBody — the canonical bare-curl onboarding call:
// no body, no Content-Type.
func TestRegister_EmptyBody(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	ts := newRegisterTestServer(t, accounts, keys, nil)

	resp := postRegister(t, ts.URL, "", "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	got := decodeRegisterResult(t, resp)
	if got.APIKey == "" || got.AccountID == "" {
		t.Errorf("bare POST should still mint: %+v", got)
	}
	if accounts.created[0].Name != "api-registered account" {
		t.Errorf("default account name = %q", accounts.created[0].Name)
	}
	if accounts.created[0].BillingEmail != "" {
		t.Errorf("billing email should be empty, got %q", accounts.created[0].BillingEmail)
	}
}

// TestRegister_ThrottleReturns429 — the F-1232 per-IP signup throttle
// gates /v1/register with the SAME budget as /v1/signup.
func TestRegister_ThrottleReturns429(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	throttle := &fakeRegisterIPThrottle{err: auth.ErrSignupRateLimited}
	ts := newRegisterTestServer(t, accounts, keys, throttle)

	resp := postRegister(t, ts.URL, "application/json", `{}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 429; body=%s", resp.StatusCode, body)
	}
	if len(accounts.created) != 0 {
		t.Errorf("throttled request must not create an account; created=%d", len(accounts.created))
	}
	if len(keys.byID) != 0 {
		t.Errorf("throttled request must not mint a key; keys=%d", len(keys.byID))
	}
}

// TestRegister_KeyWorksAfterMint — the minted plaintext authenticates
// through the SAME validator production uses for Postgres-backed keys
// (auth.PostgresAPIKeyValidator over the account + key stores), and
// carries the free-tier budget onto the Subject.
func TestRegister_KeyWorksAfterMint(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	ts := newRegisterTestServer(t, accounts, keys, nil)

	resp := postRegister(t, ts.URL, "application/json", `{"name":"agent"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decodeRegisterResult(t, resp)

	validator, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})
	if err != nil {
		t.Fatalf("NewPostgresAPIKeyValidator: %v", err)
	}
	sub, err := validator.Lookup(context.Background(), got.APIKey)
	if err != nil {
		t.Fatalf("Lookup(minted key) failed: %v", err)
	}
	if sub.Tier != auth.TierAPIKey {
		t.Errorf("subject tier = %q, want apikey", sub.Tier)
	}
	if sub.RateLimitPerMin != platform.TierFree.MaxRateLimitPerMin() {
		t.Errorf("subject rate limit = %d, want %d", sub.RateLimitPerMin, platform.TierFree.MaxRateLimitPerMin())
	}
	if sub.MonthlyQuota != platform.TierFree.MaxMonthlyQuota() {
		t.Errorf("subject monthly quota = %d, want %d", sub.MonthlyQuota, platform.TierFree.MaxMonthlyQuota())
	}
	if !strings.HasPrefix(sub.Identifier, auth.AccountIdentifierPrefix) {
		t.Errorf("subject identifier = %q, want %s<slug>", sub.Identifier, auth.AccountIdentifierPrefix)
	}
	if sub.KeyID != got.KeyID {
		t.Errorf("subject key id = %q, want %q", sub.KeyID, got.KeyID)
	}

	// A wrong key still fails.
	if _, err := validator.Lookup(context.Background(), "sip_"+strings.Repeat("0", 64)); err == nil {
		t.Error("Lookup(wrong key) succeeded, want error")
	}
}

// TestRegister_ValidationAndGates covers the reject paths: bad email,
// oversized name, wrong content type, authenticated caller, and the
// store-less deployment.
func TestRegister_ValidationAndGates(t *testing.T) {
	t.Run("invalid email 400", func(t *testing.T) {
		ts := newRegisterTestServer(t, newFakeRegisterAccountStore(), newFakeRegisterKeyStore(), nil)
		resp := postRegister(t, ts.URL, "application/json", `{"email":"not-an-email"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("name too long 400", func(t *testing.T) {
		ts := newRegisterTestServer(t, newFakeRegisterAccountStore(), newFakeRegisterKeyStore(), nil)
		resp := postRegister(t, ts.URL, "application/json",
			`{"name":"`+strings.Repeat("x", 129)+`"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("non-JSON content type 415", func(t *testing.T) {
		ts := newRegisterTestServer(t, newFakeRegisterAccountStore(), newFakeRegisterKeyStore(), nil)
		resp := postRegister(t, ts.URL, "text/plain", `{}`)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("status = %d, want 415", resp.StatusCode)
		}
	})
	t.Run("authenticated caller 400", func(t *testing.T) {
		srv := v1.New(v1.Options{
			Auth:             fakeAuthMiddleware(auth.Subject{Identifier: "acct:x", Tier: auth.TierAPIKey}),
			RegisterAccounts: newFakeRegisterAccountStore(),
			APIKeyBudgets:    v1.APIKeyBudgetStores{Platform: newFakeRegisterKeyStore()},
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp := postRegister(t, ts.URL, "application/json", `{}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("no stores 503", func(t *testing.T) {
		srv := v1.New(v1.Options{Auth: fakeAuthMiddleware(auth.Subject{})})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp := postRegister(t, ts.URL, "application/json", `{}`)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", resp.StatusCode)
		}
	})
}

// recordingMirror captures what the register mint writes into the
// validator's own key store.
type recordingMirror struct {
	got  auth.MirroredKey
	err  error
	call int
}

func (m *recordingMirror) CreateWithSecret(_ context.Context, k auth.MirroredKey) error {
	m.call++
	m.got = k
	return m.err
}

// TestRegister_MirrorsKeyIntoValidatorStore is the v0.32.0
// post-deploy regression: /v1/register wrote only the Postgres
// MANAGEMENT row while r1's auth middleware validates against REDIS,
// so the 200 response carried a key that 401'd on first use. The mint
// must mirror the SAME plaintext into the validator store when one is
// wired.
func TestRegister_MirrorsKeyIntoValidatorStore(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	mirror := &recordingMirror{}
	ts := newRegisterTestServer(t, accounts, keys, &fakeRegisterIPThrottle{}, mirror)

	resp := postRegister(t, ts.URL, "application/json", `{"name":"agent"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			APIKey string `json:"api_key"`
			KeyID  string `json:"key_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mirror.call != 1 {
		t.Fatalf("mirror called %d times, want 1 — a management-store-only key cannot authenticate", mirror.call)
	}
	if mirror.got.Plaintext != env.Data.APIKey {
		t.Errorf("mirrored plaintext does not match the key handed to the caller — one secret must validate on either backend")
	}
	if mirror.got.KeyID != env.Data.KeyID {
		t.Errorf("mirrored KeyID = %q, want %q (revocation must line up across stores)", mirror.got.KeyID, env.Data.KeyID)
	}
	if mirror.got.Identifier == "" {
		t.Error("mirrored Identifier is empty — the validator keys records by account identifier")
	}
}

// A mirror failure must fail the request: 200 with a key that cannot
// authenticate is the exact defect being fixed.
func TestRegister_MirrorFailureIsNotA200(t *testing.T) {
	accounts := newFakeRegisterAccountStore()
	keys := newFakeRegisterKeyStore()
	mirror := &recordingMirror{err: context.DeadlineExceeded}
	ts := newRegisterTestServer(t, accounts, keys, &fakeRegisterIPThrottle{}, mirror)

	resp := postRegister(t, ts.URL, "application/json", "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("register returned 200 though the credential never reached the validator store")
	}
}
