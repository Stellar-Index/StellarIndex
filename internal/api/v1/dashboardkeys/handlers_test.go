package dashboardkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/StellarIndex/stellar-index/internal/api/v1/dashboardauth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// Tests use a session pre-planted on the request context (via
// dashboardauth.WithSession) instead of the full cookie + DB
// resolve path — that's already covered by dashboardauth's own
// tests. Here we only care that the dashboardkeys handlers do
// the right thing GIVEN a session.

func newTestRig(t *testing.T) (*Handlers, *fakeKeyStore, dashboardauth.SessionContext) {
	t.Helper()
	store := newFakeKeyStore()
	h, err := NewHandlers(Config{
		Keys:   store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	sc := dashboardauth.SessionContext{
		Session: platform.Session{ID: uuid.New(), UserID: uuid.New()},
		User: platform.User{
			ID:        uuid.New(),
			AccountID: uuid.New(),
			Email:     "owner@example.com",
			Role:      platform.RoleOwner,
		},
		Account: platform.Account{
			ID:   uuid.New(),
			Slug: "example",
			// Default test account uses Starter tier (1000/min ceiling)
			// so legacy tests that supply RateLimitPerMin: 1000 don't
			// silently get clamped to the free-tier cap. F-1212 tier-
			// clamp regressions get their own test below.
			Tier:   platform.TierStarter,
			Status: platform.AccountActive,
		},
	}
	sc.User.AccountID = sc.Account.ID
	return h, store, sc
}

func sessionRequest(t *testing.T, method, target string, body any, sc dashboardauth.SessionContext) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		bs, _ := json.Marshal(body)
		rdr = bytes.NewReader(bs)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.RemoteAddr = "203.0.113.5:55123"
	req = req.WithContext(dashboardauth.WithSession(req.Context(), sc))
	return req
}

func TestHandleCreate_HappyPath(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:            "production",
		RateLimitPerMin: 1000,
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Plaintext, "sip_") || len(resp.Plaintext) < 20 {
		t.Errorf("plaintext = %q", resp.Plaintext)
	}
	if resp.Key.KeyPrefix != resp.Plaintext[:12] {
		t.Errorf("KeyPrefix mismatch: prefix=%q plaintext=%q", resp.Key.KeyPrefix, resp.Plaintext[:12])
	}
	if resp.Key.Name != "production" {
		t.Errorf("Name = %q", resp.Key.Name)
	}
}

func TestHandleCreate_AnonRejected401(t *testing.T) {
	h, _, _ := newTestRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboard/keys", strings.NewReader(`{"name":"x"}`))
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", w.Code)
	}
}

func TestHandleCreate_ViewerCannotMint(t *testing.T) {
	h, _, sc := newTestRig(t)
	sc.User.Role = platform.RoleViewer
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{Name: "x"}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandleCreate_RejectsMissingName(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{Name: "  "}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d", w.Code)
	}
}

// TestHandleCreate_TierClampsRateLimit pins F-1212 (codex
// audit-2026-05-12): a Free account requesting a 100_000/min
// budget gets clamped to the free-tier ceiling (60/min), and a
// Pro account requesting the same gets clamped to the Pro ceiling
// (10_000/min). Regression-guards against any future change that
// re-introduces direct customer control over the persisted
// budget.
func TestHandleCreate_TierClampsRateLimit(t *testing.T) {
	cases := []struct {
		name      string
		tier      platform.Tier
		requested int
		wantCap   int
	}{
		{"free clamps 100k to 60", platform.TierFree, 100_000, 60},
		{"free clamps 1000 to 60", platform.TierFree, 1000, 60},
		{"starter passes 1000", platform.TierStarter, 1000, 1000},
		{"starter clamps 10000 to 1000", platform.TierStarter, 10_000, 1000},
		{"pro passes 10000", platform.TierPro, 10_000, 10_000},
		{"pro clamps 100k to 10k", platform.TierPro, 100_000, 10_000},
		{"business passes 60k", platform.TierBusiness, 60_000, 60_000},
		{"enterprise passes 100k", platform.TierEnterprise, 100_000, 100_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			sc.Account.Tier = tc.tier
			req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
				Name:            "tier-test",
				RateLimitPerMin: tc.requested,
			}, sc)
			w := httptest.NewRecorder()
			h.HandleCreate(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got int
			for _, k := range store.byID {
				if k.AccountID == sc.Account.ID && k.Name == "tier-test" {
					got = k.RateLimitPerMin
					break
				}
			}
			if got != tc.wantCap {
				t.Errorf("persisted RateLimitPerMin = %d, want %d (requested %d on %s tier)",
					got, tc.wantCap, tc.requested, tc.tier)
			}
		})
	}
}

// TestHandleCreate_ClampsMonthlyQuota pins audit-2026-07 (MEDIUM):
// the customer-supplied monthly_quota is clamped at mint to the
// account's hard ceiling (the operator's account-level override when
// set, else the tier default), so a metered customer can only LOWER
// their cap, never raise it above the plan. Regression-guards the
// revenue-exposure hole where a `monthly_quota: 9_000_000_000` was
// persisted verbatim and then won the auth cascade.
func TestHandleCreate_ClampsMonthlyQuota(t *testing.T) {
	cases := []struct {
		name      string
		tier      platform.Tier
		override  int64 // account-level MonthlyRequestQuotaOverride (0 = unset)
		requested int64
		wantQuota int64
	}{
		{"override is ceiling: huge clamped down", platform.TierStarter, 5_000_000, 9_000_000_000, 5_000_000},
		{"override is ceiling: below honored", platform.TierStarter, 5_000_000, 1_000_000, 1_000_000},
		{"override is ceiling: equal honored", platform.TierStarter, 5_000_000, 5_000_000, 5_000_000},
		{"override set, request 0 stays inherit", platform.TierStarter, 5_000_000, 0, 0},
		{"no override: tier ceiling clamps huge", platform.TierStarter, 0, 9_000_000_000, platform.TierStarter.MaxMonthlyQuota()},
		{"no override: below tier ceiling honored", platform.TierPro, 0, 2_000_000, 2_000_000},
		{"no override, request 0 stays inherit/unlimited", platform.TierFree, 0, 0, 0},
		{"free tier ceiling clamps", platform.TierFree, 0, 500_000, platform.TierFree.MaxMonthlyQuota()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			sc.Account.Tier = tc.tier
			sc.Account.MonthlyRequestQuotaOverride = tc.override
			req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
				Name:         "quota-test",
				MonthlyQuota: tc.requested,
			}, sc)
			w := httptest.NewRecorder()
			h.HandleCreate(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got int64 = -1
			for _, k := range store.byID {
				if k.AccountID == sc.Account.ID && k.Name == "quota-test" {
					got = k.MonthlyQuota
					break
				}
			}
			if got != tc.wantQuota {
				t.Errorf("persisted MonthlyQuota = %d, want %d (requested %d, override %d, tier %s)",
					got, tc.wantQuota, tc.requested, tc.override, tc.tier)
			}
		})
	}
}

func TestHandleCreate_RejectsMalformedExpiresAt(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:      "x",
		ExpiresAt: "not-rfc3339",
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d", w.Code)
	}
}

func TestHandleCreate_RejectsPastExpiresAt(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:      "x",
		ExpiresAt: "2020-01-01T00:00:00Z",
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d", w.Code)
	}
}

func TestHandleCreate_QuotaEnforced(t *testing.T) {
	h, store, sc := newTestRig(t)
	// Rig default account is Starter — seed to that tier's ceiling.
	for i := 0; i < sc.Account.Tier.MaxActiveKeys(); i++ {
		store.byID[uuid.New().String()] = platform.APIKey{
			ID: uuid.New().String(), AccountID: sc.Account.ID,
			Name: "seed", KeyPrefix: "sip_seedseed",
		}
	}
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{Name: "one-too-many"}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// TestHandleCreate_QuotaIsTierAware pins the tier ladder: a Free
// account caps out well below Starter, the same key count passes on
// a higher tier, and Config.KeyQuotas overrides the ladder per tier.
func TestHandleCreate_QuotaIsTierAware(t *testing.T) {
	h, store, sc := newTestRig(t)
	sc.Account.Tier = platform.TierFree
	for i := 0; i < platform.TierFree.MaxActiveKeys(); i++ {
		store.byID[uuid.New().String()] = platform.APIKey{
			ID: uuid.New().String(), AccountID: sc.Account.ID,
			Name: "seed", KeyPrefix: "sip_seedseed",
		}
	}

	// Free at its (lower) cap → 409.
	w := httptest.NewRecorder()
	h.HandleCreate(w, sessionRequest(t, http.MethodPost, "/v1/dashboard/keys",
		createRequest{Name: "over-free"}, sc))
	if w.Code != http.StatusConflict {
		t.Fatalf("free at cap: status = %d, want 409", w.Code)
	}

	// Same count on Business sails through.
	sc.Account.Tier = platform.TierBusiness
	w = httptest.NewRecorder()
	h.HandleCreate(w, sessionRequest(t, http.MethodPost, "/v1/dashboard/keys",
		createRequest{Name: "business-ok"}, sc))
	if w.Code != http.StatusCreated {
		t.Fatalf("business tier: status = %d (body=%s), want 201", w.Code, w.Body.String())
	}

	// Config override: cap Business at 1 — the account is over → 409.
	h2, err := NewHandlers(Config{
		Keys:      store,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		KeyQuotas: map[platform.Tier]int{platform.TierBusiness: 1},
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	w = httptest.NewRecorder()
	h2.HandleCreate(w, sessionRequest(t, http.MethodPost, "/v1/dashboard/keys",
		createRequest{Name: "over-override"}, sc))
	if w.Code != http.StatusConflict {
		t.Errorf("business override cap 1: status = %d, want 409", w.Code)
	}
}

func TestHandleCreate_CIDRAndBareIPParse(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:        "test",
		IPAllowlist: []string{"203.0.113.0/24", "198.51.100.7"},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp createResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Key.IPAllowlist) != 2 {
		t.Fatalf("IPAllowlist = %v", resp.Key.IPAllowlist)
	}
	// Bare IP should have been promoted to /32.
	found := false
	for _, p := range resp.Key.IPAllowlist {
		if p == "198.51.100.7/32" {
			found = true
		}
	}
	if !found {
		t.Errorf("bare IP not promoted to /32: %v", resp.Key.IPAllowlist)
	}
}

func TestHandleList_OnlyOwnAccount(t *testing.T) {
	h, store, sc := newTestRig(t)
	other := uuid.New()
	// One key for our account, one for another.
	store.byID["k-mine"] = platform.APIKey{ID: "k-mine", AccountID: sc.Account.ID, Name: "mine"}
	store.byID["k-other"] = platform.APIKey{ID: "k-other", AccountID: other, Name: "other"}

	req := sessionRequest(t, http.MethodGet, "/v1/dashboard/keys", nil, sc)
	w := httptest.NewRecorder()
	h.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp listResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Keys) != 1 || resp.Keys[0].ID != "k-mine" {
		t.Errorf("unexpected list: %+v", resp.Keys)
	}
}

func TestHandleRevoke_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	store.byID["k-mine"] = platform.APIKey{ID: "k-mine", AccountID: sc.Account.ID, Name: "mine"}
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-mine", nil, sc)
	req.SetPathValue("id", "k-mine")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d", w.Code)
	}
	if store.byID["k-mine"].RevokedAt.IsZero() {
		t.Errorf("RevokedAt not set")
	}
}

func TestHandleRevoke_OtherAccount404(t *testing.T) {
	h, store, sc := newTestRig(t)
	other := uuid.New()
	store.byID["k-other"] = platform.APIKey{ID: "k-other", AccountID: other, Name: "other"}
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-other", nil, sc)
	req.SetPathValue("id", "k-other")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d (revoke leaked across accounts)", w.Code)
	}
	// Confirm the other account's key is untouched.
	if !store.byID["k-other"].RevokedAt.IsZero() {
		t.Errorf("cross-account revoke succeeded")
	}
}

func TestHandleRevoke_AbsentKey404(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-missing", nil, sc)
	req.SetPathValue("id", "k-missing")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d", w.Code)
	}
}

// ─── fake APIKeyStore ─────────────────────────────────────────────

type fakeKeyStore struct {
	mu   sync.Mutex
	byID map[string]platform.APIKey
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{byID: map[string]platform.APIKey{}}
}

func (f *fakeKeyStore) Create(_ context.Context, k platform.APIKey, maxActiveKeysPerAccount int) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maxActiveKeysPerAccount > 0 {
		active := 0
		for _, existing := range f.byID {
			if existing.AccountID == k.AccountID && existing.RevokedAt.IsZero() {
				active++
			}
		}
		if active >= maxActiveKeysPerAccount {
			return platform.APIKey{}, platform.ErrAPIKeyQuotaExceeded
		}
	}
	for _, existing := range f.byID {
		if string(existing.KeyHash) == string(k.KeyHash) {
			return platform.APIKey{}, platform.ErrConflict
		}
	}
	k.CreatedAt = time.Now().UTC()
	f.byID[k.ID] = k
	return k, nil
}

func (f *fakeKeyStore) Get(_ context.Context, id string) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byID[id]
	if !ok {
		return platform.APIKey{}, platform.ErrNotFound
	}
	return k, nil
}

func (f *fakeKeyStore) GetByHash(_ context.Context, hash []byte) (platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.byID {
		if string(k.KeyHash) == string(hash) {
			return k, nil
		}
	}
	return platform.APIKey{}, platform.ErrNotFound
}

func (f *fakeKeyStore) ListForAccount(_ context.Context, accountID uuid.UUID) ([]platform.APIKey, error) {
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

func (f *fakeKeyStore) Update(_ context.Context, k platform.APIKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[k.ID]; !ok {
		return platform.ErrNotFound
	}
	f.byID[k.ID] = k
	return nil
}

func (f *fakeKeyStore) Revoke(_ context.Context, id string, by uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byID[id]
	if !ok {
		return platform.ErrNotFound
	}
	if k.RevokedAt.IsZero() {
		k.RevokedAt = time.Now().UTC()
	}
	k.RevokedByUserID = by
	k.RevokedReason = reason
	f.byID[id] = k
	return nil
}

func (f *fakeKeyStore) TouchUsage(_ context.Context, id string, ip net.IP, ua string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byID[id]
	if !ok {
		return platform.ErrNotFound
	}
	k.LastUsedAt = time.Now().UTC()
	k.LastUsedIP = ip
	k.LastUsedUserAgent = ua
	f.byID[id] = k
	return nil
}

var _ platform.APIKeyStore = (*fakeKeyStore)(nil)

// TestToDTO_OmitsZeroTimes is the regression for the dashboard bugs where a
// fresh key looked "revoked" + "last used ~2025 years ago": a zero time.Time
// with `omitempty` is NOT omitted (it's a non-empty struct → "0001-01-01...").
// Pointer times + nilIfZero must drop them so a never-revoked / never-used /
// never-expiring key omits the fields entirely.
func TestToDTO_OmitsZeroTimes(t *testing.T) {
	dto := toDTO(platform.APIKey{
		ID: "kid_1", Name: "fresh", KeyPrefix: "sip_abc123",
		CreatedAt: time.Now().UTC(),
		// RevokedAt / LastUsedAt / ExpiresAt left zero.
	})
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, banned := range []string{"revoked_at", "last_used_at", "expires_at"} {
		if strings.Contains(s, banned) {
			t.Errorf("fresh-key DTO must omit %q, got: %s", banned, s)
		}
	}
	if !strings.Contains(s, "created_at") {
		t.Errorf("created_at should always be present: %s", s)
	}

	// A revoked key DOES surface revoked_at.
	rev := toDTO(platform.APIKey{ID: "kid_2", CreatedAt: time.Now().UTC(), RevokedAt: time.Now().UTC()})
	if rb, _ := json.Marshal(rev); !strings.Contains(string(rb), "revoked_at") {
		t.Errorf("revoked key must include revoked_at: %s", rb)
	}
}

// TestHandleCreate_Scopes pins the dashboard scope plumbing: valid
// scopes persist on the record (deduped) and echo in the DTO;
// unknown scopes 400 before any mint.
func TestHandleCreate_Scopes(t *testing.T) {
	h, store, sc := newTestRig(t)
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:   "scoped",
		Scopes: []string{"read", "account", "read"},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Key.Scopes) != 2 || resp.Key.Scopes[0] != "read" || resp.Key.Scopes[1] != "account" {
		t.Errorf("DTO scopes = %v, want deduped [read account]", resp.Key.Scopes)
	}
	var persisted []string
	for _, k := range store.byID {
		if k.Name == "scoped" {
			persisted = k.Scopes
		}
	}
	if len(persisted) != 2 {
		t.Errorf("persisted scopes = %v", persisted)
	}

	// Unknown scope → 400, nothing minted.
	before := len(store.byID)
	req = sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{
		Name:   "bad-scope",
		Scopes: []string{"everything"},
	}, sc)
	w = httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown scope", w.Code)
	}
	if len(store.byID) != before {
		t.Errorf("key minted despite invalid scope")
	}
}
