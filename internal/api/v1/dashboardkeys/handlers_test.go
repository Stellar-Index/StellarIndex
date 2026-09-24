package dashboardkeys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
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
// audit-2026-05-12): a free account requesting a 100_000/min
// budget gets clamped to the free-tier ceiling (1000/min under the
// free-platform model), while a partner account keeps the 100_000
// ceiling. Legacy tier strings must clamp at their CANONICAL rung
// (starter≡free, pro/business/enterprise≡partner).
// Regression-guards against any future change that re-introduces
// direct customer control over the persisted budget.
func TestHandleCreate_TierClampsRateLimit(t *testing.T) {
	cases := []struct {
		name      string
		tier      platform.Tier
		requested int
		wantCap   int
	}{
		{"free clamps 100k to 1000", platform.TierFree, 100_000, 1000},
		{"free clamps 10000 to 1000", platform.TierFree, 10_000, 1000},
		{"free passes 1000", platform.TierFree, 1000, 1000},
		{"partner passes 100k", platform.TierPartner, 100_000, 100_000},
		{"legacy starter maps to free: clamps 10000 to 1000", platform.TierStarter, 10_000, 1000},
		{"legacy pro maps to partner: passes 100k", platform.TierPro, 100_000, 100_000},
		{"legacy business maps to partner: passes 60k", platform.TierBusiness, 60_000, 60_000},
		{"legacy enterprise maps to partner: passes 100k", platform.TierEnterprise, 100_000, 100_000},
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
		{"no override, request 0 falls back to the tier ceiling", platform.TierFree, 0, 0, platform.TierFree.MaxMonthlyQuota()},
		{"override present, request 0 inherits it at auth time", platform.TierFree, 250_000, 0, 0},
		{"free tier ceiling clamps", platform.TierFree, 0, 5_000_000, platform.TierFree.MaxMonthlyQuota()},
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

// TestDefaultMintedKey_MatchesAccountEffectiveLimits locks the
// account-level "effective" limits served on /v1/account/me and the
// staff views (GH-1074) to what auth enforces on a key minted here with
// the request's limits unset: mint through the real handler, resolve
// the persisted key through the same platform cascade auth's Validate
// calls, and require both to equal the account view and the expected
// value. A partner comped to 5,000/min must read 5,000, not the
// 100,000 tier ceiling.
func TestDefaultMintedKey_MatchesAccountEffectiveLimits(t *testing.T) {
	cases := []struct {
		name      string
		tier      platform.Tier
		rate      int
		quota     int64
		wantRate  int
		wantQuota int64
	}{
		{"partner comped to 5000/min", platform.TierPartner, 5000, 200_000, 5000, 200_000},
		{"partner, no overrides", platform.TierPartner, 0, 0, 1000, platform.TierPartner.MaxMonthlyQuota()},
		{"free, no overrides", platform.TierFree, 0, 0, 1000, platform.TierFree.MaxMonthlyQuota()},
		{"free raised above its tier", platform.TierFree, 50_000, 5_000_000, 50_000, 5_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			sc.Account.Tier = tc.tier
			sc.Account.RateLimitPerMinOverride = tc.rate
			sc.Account.MonthlyRequestQuotaOverride = tc.quota
			req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{Name: "default-key"}, sc)
			w := httptest.NewRecorder()
			h.HandleCreate(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var key platform.APIKey
			for _, k := range store.byID {
				if k.AccountID == sc.Account.ID && k.Name == "default-key" {
					key = k
				}
			}
			enforcedRate := sc.Account.ResolveKeyRateLimitPerMin(key.RateLimitPerMin)
			enforcedQuota := sc.Account.ResolveKeyMonthlyQuota(key.MonthlyQuota)
			if enforcedRate != tc.wantRate || sc.Account.EffectiveRateLimitPerMin() != tc.wantRate {
				t.Errorf("rate: enforced %d, account view %d, want %d",
					enforcedRate, sc.Account.EffectiveRateLimitPerMin(), tc.wantRate)
			}
			if enforcedQuota != tc.wantQuota || sc.Account.EffectiveMonthlyQuota() != tc.wantQuota {
				t.Errorf("quota: enforced %d, account view %d, want %d",
					enforcedQuota, sc.Account.EffectiveMonthlyQuota(), tc.wantQuota)
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

// TestHandleList_BoundsRevokedHistory pins GH-766: a create/revoke loop grows
// revoked rows without bound, so the list returns every active key but only
// the listRevokedLimit most recent revoked ones, and says it truncated.
func TestHandleList_BoundsRevokedHistory(t *testing.T) {
	h, store, sc := newTestRig(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// The active key is the OLDEST row: it must survive any truncation.
	store.byID["k-active"] = platform.APIKey{ID: "k-active", AccountID: sc.Account.ID, CreatedAt: base}
	const revokedRows = listRevokedLimit + 50
	for i := 1; i <= revokedRows; i++ {
		id := fmt.Sprintf("k-rev-%03d", i)
		at := base.Add(time.Duration(i) * time.Minute)
		store.byID[id] = platform.APIKey{ID: id, AccountID: sc.Account.ID, CreatedAt: at, RevokedAt: at}
	}

	w := httptest.NewRecorder()
	h.HandleList(w, sessionRequest(t, http.MethodGet, "/v1/dashboard/keys", nil, sc))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := len(resp.Keys), 1+listRevokedLimit; got != want {
		t.Fatalf("len(keys) = %d, want %d (every active key + the %d newest revoked)", got, want, listRevokedLimit)
	}
	if !resp.RevokedTruncated {
		t.Error("revoked_truncated = false with 50 older revoked keys omitted")
	}
	if resp.Keys[0].ID != "k-active" {
		t.Errorf("keys[0] = %q, want the oldest active key k-active", resp.Keys[0].ID)
	}
	if first, want := resp.Keys[1].ID, fmt.Sprintf("k-rev-%03d", revokedRows-listRevokedLimit+1); first != want {
		t.Errorf("oldest revoked key returned = %q, want %q (the newest revoked are kept)", first, want)
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

func TestHandleRevoke_MemberCannotRevokeAnotherUsersKey(t *testing.T) {
	h, store, sc := newTestRig(t)
	sc.User.Role = platform.RoleMember
	otherUser := uuid.New()
	store.byID["k-owner"] = platform.APIKey{ID: "k-owner", AccountID: sc.Account.ID, CreatedByUserID: otherUser, Name: "owner's key"}
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-owner", nil, sc)
	req.SetPathValue("id", "k-owner")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (member revoked a key it didn't create)", w.Code)
	}
	if !store.byID["k-owner"].RevokedAt.IsZero() {
		t.Errorf("member revoked another user's key")
	}
}

func TestHandleRevoke_MemberCanRevokeOwnKey(t *testing.T) {
	h, store, sc := newTestRig(t)
	sc.User.Role = platform.RoleMember
	store.byID["k-mine"] = platform.APIKey{ID: "k-mine", AccountID: sc.Account.ID, CreatedByUserID: sc.User.ID, Name: "mine"}
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-mine", nil, sc)
	req.SetPathValue("id", "k-mine")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (member revoking its own key)", w.Code)
	}
	if store.byID["k-mine"].RevokedAt.IsZero() {
		t.Errorf("RevokedAt not set")
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

func (f *fakeKeyStore) CountActiveForAccount(ctx context.Context, accountID uuid.UUID) (int, error) {
	active, err := f.ListActiveForAccount(ctx, accountID)
	return len(active), err
}

func (f *fakeKeyStore) ListActiveForAccount(_ context.Context, accountID uuid.UUID) ([]platform.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []platform.APIKey
	for _, k := range f.byID {
		if k.AccountID == accountID && k.RevokedAt.IsZero() {
			out = append(out, k)
		}
	}
	return out, nil
}

// ListForAccount mirrors the store contract: every active key plus the
// revokedLimit most recently created revoked keys, oldest first.
func (f *fakeKeyStore) ListForAccount(ctx context.Context, accountID uuid.UUID, revokedLimit int) ([]platform.APIKey, bool, error) {
	out, _ := f.ListActiveForAccount(ctx, accountID)
	f.mu.Lock()
	var revoked []platform.APIKey
	for _, k := range f.byID {
		if k.AccountID == accountID && !k.RevokedAt.IsZero() {
			revoked = append(revoked, k)
		}
	}
	f.mu.Unlock()
	slices.SortFunc(revoked, func(a, b platform.APIKey) int { return b.CreatedAt.Compare(a.CreatedAt) })
	more := len(revoked) > revokedLimit
	if more {
		revoked = revoked[:revokedLimit]
	}
	out = append(out, revoked...)
	slices.SortFunc(out, func(a, b platform.APIKey) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, more, nil
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

// TestParseCreateRequest_RejectsCheckConstrainedValues pins that the four
// fields Postgres CHECK-constrains are rejected as 400s naming the field,
// not passed through to surface as an opaque 500 from a constraint
// violation deep in the store.
//
// A cold audit (2026-08-04) proved all four reached Postgres. The
// operator cost is real: a 500 here is indistinguishable from the
// genuine 500 that api_keys migration drift produces, so the first
// conclusion it prompts is "key creation is broken" rather than "the
// request was malformed".
func TestParseCreateRequest_RejectsCheckConstrainedValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"description over 2000", `{"name":"k","description":"` + strings.Repeat("x", 2001) + `"}`, "description"},
		{"negative monthly_quota", `{"name":"k","monthly_quota":-1}`, "monthly_quota"},
		{"threshold over 100", `{"name":"k","usage_alert_threshold_pct":999}`, "usage_alert_threshold_pct"},
		{"threshold under 1", `{"name":"k","usage_alert_threshold_pct":-5}`, "usage_alert_threshold_pct"},
		{"cidr with host bits", `{"name":"k","ip_allowlist":["203.0.113.5/24"]}`, "host bits"},
		{"malformed cidr", `{"name":"k","ip_allowlist":["not-an-ip/24"]}`, "not valid CIDR"},
		{"malformed ip", `{"name":"k","ip_allowlist":["999.1.1.1"]}`, "not a valid IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/dashboard/keys", strings.NewReader(tc.body))
			_, status, problem := parseCreateRequest(r)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (problem %q)", status, problem)
			}
			if !strings.Contains(problem, tc.want) {
				t.Errorf("problem = %q, want it to name %q", problem, tc.want)
			}
		})
	}
}

// TestParseCreateRequest_AcceptsValidAllowlistForms guards against the
// validation above over-rejecting: a bare IP and a correctly-aligned
// prefix are both legitimate.
func TestParseCreateRequest_AcceptsValidAllowlistForms(t *testing.T) {
	body := `{"name":"k","ip_allowlist":["203.0.113.5","203.0.113.0/24","2001:db8::/32"],` +
		`"usage_alert_threshold_pct":80,"monthly_quota":500}`
	r := httptest.NewRequest(http.MethodPost, "/v1/dashboard/keys", strings.NewReader(body))
	_, status, problem := parseCreateRequest(r)
	if problem != "" || status != 0 {
		t.Fatalf("valid body rejected: status=%d problem=%q", status, problem)
	}
}

type recordingAuditSink struct {
	mu      sync.Mutex
	entries []platform.AuditEntry
}

func (s *recordingAuditSink) Append(_ context.Context, e platform.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *recordingAuditSink) AppendBatch(ctx context.Context, entries []platform.AuditEntry) error {
	for _, e := range entries {
		if err := s.Append(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (s *recordingAuditSink) List(context.Context, platform.AuditQuery) ([]platform.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]platform.AuditEntry(nil), s.entries...), nil
}

func (s *recordingAuditSink) only(t *testing.T) platform.AuditEntry {
	t.Helper()
	all, _ := s.List(context.Background(), platform.AuditQuery{})
	if len(all) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (%+v)", len(all), all)
	}
	return all[0]
}

// TestHandleCreate_WritesKeyMintAuditRow — a dashboard mint lands the same
// key.mint row /v1/account/keys and /v1/admin/keys write.
func TestHandleCreate_WritesKeyMintAuditRow(t *testing.T) {
	h, _, sc := newTestRig(t)
	sink := &recordingAuditSink{}
	h.cfg.Audit = sink
	req := sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", createRequest{Name: "production"}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	e := sink.only(t)
	if e.Action != "key.mint" || e.ActorKind != platform.ActorUser || e.ActorUserID != sc.User.ID ||
		e.AccountID != sc.Account.ID || e.TargetKind != "api_key" || e.TargetID != resp.Key.ID {
		t.Fatalf("audit row = %+v, want key.mint by the session user on %s", e, resp.Key.ID)
	}
	if e.IP.String() != "203.0.113.5" {
		t.Fatalf("audit ip = %v, want the caller's", e.IP)
	}
	if strings.Contains(string(e.Metadata), resp.Plaintext) {
		t.Fatal("key.mint audit metadata carries the plaintext key")
	}
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta["session_id"] != sc.Session.ID.String() || meta["name"] != "production" || meta["route"] != "/v1/dashboard/keys" {
		t.Fatalf("audit metadata = %v", meta)
	}
}

// TestHandleRevoke_WritesKeyRevokeAuditRow — and a refused cross-account
// revoke writes nothing.
func TestHandleRevoke_WritesKeyRevokeAuditRow(t *testing.T) {
	h, store, sc := newTestRig(t)
	sink := &recordingAuditSink{}
	h.cfg.Audit = sink
	store.byID["k-theirs"] = platform.APIKey{ID: "k-theirs", AccountID: uuid.New(), Name: "theirs"}
	store.byID["k-mine"] = platform.APIKey{ID: "k-mine", AccountID: sc.Account.ID, Name: "mine"}

	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-theirs", nil, sc)
	req.SetPathValue("id", "k-theirs")
	w := httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNotFound || len(sink.entries) != 0 {
		t.Fatalf("cross-account revoke: status %d, audit rows %d — want 404 and none", w.Code, len(sink.entries))
	}

	req = sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-mine", nil, sc)
	req.SetPathValue("id", "k-mine")
	w = httptest.NewRecorder()
	h.HandleRevoke(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	e := sink.only(t)
	if e.Action != "key.revoke" || e.ActorUserID != sc.User.ID || e.AccountID != sc.Account.ID || e.TargetID != "k-mine" {
		t.Fatalf("audit row = %+v, want key.revoke by the session user on k-mine", e)
	}
}

// TestParseCreateRequest_LengthLimitsCountCodePoints: the spec's
// maxLength and Postgres length() both count characters, so a CJK name
// at the documented limit is valid even though it is 3× that in bytes.
func TestParseCreateRequest_LengthLimitsCountCodePoints(t *testing.T) {
	for _, tc := range []struct {
		field string
		limit int
		body  func(v string) string
	}{
		{"name", 200, func(v string) string { return `{"name":"` + v + `"}` }},
		{"description", 2000, func(v string) string { return `{"name":"k","description":"` + v + `"}` }},
	} {
		for _, n := range []int{tc.limit, tc.limit + 1} {
			body := tc.body(strings.Repeat("名", n))
			r := httptest.NewRequest(http.MethodPost, "/v1/dashboard/keys", strings.NewReader(body))
			_, status, problem := parseCreateRequest(r)
			if accepted := problem == ""; accepted != (n <= tc.limit) {
				t.Errorf("%s of %d code points: status=%d problem=%q", tc.field, n, status, problem)
			}
		}
	}
}

// TestHandleList_ServesTheEnforcedKeyCeiling pins GH-1073: the dashboard
// could only learn the key cap from the 409, and that 409 called expired
// keys "active". The list now carries the cap create enforces (the
// KeyQuotas override, not the default ladder), and the 409 names what
// the cap counts: every unrevoked key, expired ones included.
func TestHandleList_ServesTheEnforcedKeyCeiling(t *testing.T) {
	h, store, sc := newTestRig(t)
	h.cfg.KeyQuotas = map[platform.Tier]int{sc.Account.Tier: 2}
	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"k-exp-1", "k-exp-2"} {
		store.byID[id] = platform.APIKey{ID: id, AccountID: sc.Account.ID, Name: id, ExpiresAt: expired}
	}

	w := httptest.NewRecorder()
	h.HandleList(w, sessionRequest(t, http.MethodGet, "/v1/dashboard/keys", nil, sc))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["max_active_keys"]); got != "2" {
		t.Errorf("max_active_keys = %q, want 2 (the KeyQuotas override create enforces)", got)
	}

	w = httptest.NewRecorder()
	h.HandleCreate(w, sessionRequest(t, http.MethodPost, "/v1/dashboard/keys",
		createRequest{Name: "over-cap"}, sc))
	if w.Code != http.StatusConflict {
		t.Fatalf("create with two expired, unrevoked keys at cap 2: status = %d, want 409", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "2 unrevoked keys") || !strings.Contains(body, "expired keys hold their slot") {
		t.Errorf("409 body = %s, want it to count unrevoked keys and say expired ones hold a slot", body)
	}
}
