package dashboardwebhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fakeStore is an in-memory platform.WebhookStore. Each test gets
// a fresh instance so they can't interfere with each other.
type fakeStore struct {
	mu         sync.Mutex
	webhooks   map[uuid.UUID]platform.CustomerWebhook
	deliveries map[uuid.UUID][]platform.WebhookDelivery
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		webhooks:   map[uuid.UUID]platform.CustomerWebhook{},
		deliveries: map[uuid.UUID][]platform.WebhookDelivery{},
	}
}

func (s *fakeStore) CreateWebhook(_ context.Context, w platform.CustomerWebhook, maxPerAccount int) (platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxPerAccount > 0 {
		count := 0
		for _, existing := range s.webhooks {
			if existing.AccountID == w.AccountID {
				count++
			}
		}
		if count >= maxPerAccount {
			return platform.CustomerWebhook{}, platform.ErrWebhookQuotaExceeded
		}
	}
	if s.urlTaken(w) {
		return platform.CustomerWebhook{}, platform.ErrConflict
	}
	w.ID = uuid.New()
	w.CreatedAt = time.Now().UTC()
	w.UpdatedAt = w.CreatedAt
	s.webhooks[w.ID] = w
	return w, nil
}

// urlTaken mirrors migration 0180's UNIQUE (account_id, url). Caller holds s.mu.
func (s *fakeStore) urlTaken(w platform.CustomerWebhook) bool {
	for id, existing := range s.webhooks {
		if id != w.ID && existing.AccountID == w.AccountID && existing.URL == w.URL {
			return true
		}
	}
	return false
}

func (s *fakeStore) GetWebhook(_ context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.webhooks[id]; ok {
		return w, nil
	}
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (s *fakeStore) ListWebhooksForAccount(_ context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []platform.CustomerWebhook
	for _, w := range s.webhooks {
		if w.AccountID == accountID {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s *fakeStore) ListWebhooksSubscribedTo(_ context.Context, eventType platform.WebhookEventType) ([]platform.CustomerWebhook, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []platform.CustomerWebhook
	for _, w := range s.webhooks {
		if !w.Enabled {
			continue
		}
		for _, ev := range w.Events {
			if ev == string(eventType) {
				out = append(out, w)
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) UpdateWebhook(_ context.Context, w platform.CustomerWebhook) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.webhooks[w.ID]; !ok {
		return platform.ErrNotFound
	}
	if s.urlTaken(w) {
		return platform.ErrConflict
	}
	w.UpdatedAt = time.Now().UTC()
	s.webhooks[w.ID] = w
	return nil
}

func (s *fakeStore) RotateWebhookSecret(_ context.Context, _ uuid.UUID) (string, error) {
	return "", errors.New("not implemented")
}

func (s *fakeStore) DeleteWebhook(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.webhooks, id)
	delete(s.deliveries, id)
	return nil
}

func (s *fakeStore) AppendDelivery(_ context.Context, d platform.WebhookDelivery) (platform.WebhookDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.ID = uuid.New()
	d.CreatedAt = time.Now().UTC()
	s.deliveries[d.WebhookID] = append(s.deliveries[d.WebhookID], d)
	return d, nil
}

func (s *fakeStore) UpdateDelivery(_ context.Context, _ platform.WebhookDelivery) error {
	return nil
}

func (s *fakeStore) ListDeliveries(_ context.Context, webhookID uuid.UUID, limit int) ([]platform.WebhookDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.deliveries[webhookID]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *fakeStore) EnqueueDelivery(_ context.Context, _ platform.WebhookDelivery) error {
	return nil
}

func (s *fakeStore) ListPendingDeliveries(_ context.Context, _ int) ([]platform.WebhookDelivery, error) {
	return nil, nil
}

func (s *fakeStore) MarkDelivered(_ context.Context, _ uuid.UUID, _ int) error {
	return nil
}

func (s *fakeStore) MarkAttemptFailed(_ context.Context, _ uuid.UUID, _ string, _ int, _ time.Time) error {
	return nil
}

func newTestRig(t *testing.T) (*Handlers, *fakeStore, dashboardauth.SessionContext) {
	t.Helper()
	store := newFakeStore()
	h, err := NewHandlers(Config{
		Webhooks: store,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC) },
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
			ID:     uuid.New(),
			Slug:   "example",
			Tier:   platform.TierFree,
			Status: platform.AccountActive,
		},
	}
	sc.User.AccountID = sc.Account.ID
	return h, store, sc
}

func sessionReq(t *testing.T, method, target string, body any, sc dashboardauth.SessionContext) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		bs, _ := json.Marshal(body)
		rdr = bytes.NewReader(bs)
	}
	req := httptest.NewRequest(method, target, rdr)
	req = req.WithContext(dashboardauth.WithSession(req.Context(), sc))
	return req
}

func TestHandleCreate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
		Name:   "ops-slack",
		URL:    "https://hooks.slack.example/services/T/B/X",
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp createResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Webhook.ID == "" {
		t.Errorf("ID not populated")
	}
	if resp.Secret == "" || len(resp.Secret) < 10 {
		t.Errorf("secret looks too short: %q", resp.Secret)
	}
	if len(store.webhooks) != 1 {
		t.Errorf("store should contain 1 webhook, got %d", len(store.webhooks))
	}
}

func TestHandleCreate_AnonRejected401(t *testing.T) {
	h, _, _ := newTestRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboard/webhooks", nil)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleCreate_ViewerCannotManage(t *testing.T) {
	h, _, sc := newTestRig(t)
	sc.User.Role = platform.RoleViewer
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
		Name:   "ops",
		URL:    "https://example.com/hook",
		Events: []string{string(platform.WebhookEventAnomalyFreeze)},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandleCreate_RejectsHTTPURL(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
		Name:   "ops",
		URL:    "http://example.com/hook", // plain HTTP, not HTTPS
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleCreate_RejectsSSRFTargets pins F-1245 (codex
// audit-2026-05-12): webhook URLs that point at internal /
// loopback / link-local / private / CGN / cloud-metadata
// destinations must be rejected at registration. Userinfo-
// embedded URLs are also rejected so an attacker can't disguise
// credentials in the URL itself.
func TestHandleCreate_RejectsSSRFTargets(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"loopback IPv4 literal", "https://127.0.0.1/hook"},
		{"loopback IPv6 literal", "https://[::1]/hook"},
		{"RFC1918 10/8", "https://10.0.0.1/hook"},
		{"RFC1918 192.168", "https://192.168.1.1/hook"},
		{"RFC1918 172.16", "https://172.16.0.1/hook"},
		{"link-local", "https://169.254.169.254/latest/meta-data/"},
		{"unspecified", "https://0.0.0.0/hook"},
		{"CGN 100.64", "https://100.64.0.1/hook"},
		{"userinfo embedded", "https://user:pass@example.com/hook"},
		{"empty hostname", "https:///hook"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, sc := newTestRig(t)
			req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
				Name:   "ssrf-probe",
				URL:    tc.url,
				Events: []string{string(platform.WebhookEventIncidentSEV1)},
			}, sc)
			w := httptest.NewRecorder()
			h.HandleCreate(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d (body=%s), want 400 for SSRF target %q",
					w.Code, w.Body.String(), tc.url)
			}
		})
	}
}

// TestBlockedResolvedAddrError_DoesNotLeakResolvedIP pins RSEC-Y1: the
// error validateWebhookURL returns for a DNS-resolved internal address must
// name the customer-supplied hostname, never the resolved IP — that error
// text is written verbatim into the 400 response body.
func TestBlockedResolvedAddrError_DoesNotLeakResolvedIP(t *testing.T) {
	host := "rebind.example.net"
	err := blockedResolvedAddrError(host, []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}})
	if err == nil {
		t.Fatal("blockedResolvedAddrError = nil, want a rejection for a cloud-metadata address")
	}
	if !strings.Contains(err.Error(), host) {
		t.Errorf("error %q does not name the rejected host %q", err.Error(), host)
	}
	if strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("error %q leaks the resolved internal address", err.Error())
	}
}

func TestHandleCreate_RejectsUnknownEventType(t *testing.T) {
	h, _, sc := newTestRig(t)
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
		Name:   "ops",
		URL:    "https://example.com/hook",
		Events: []string{"made.up.event"},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreate_QuotaEnforced(t *testing.T) {
	h, store, sc := newTestRig(t)
	// Pre-populate the tier's quota (the rig's account is Free).
	for i := 0; i < sc.Account.Tier.MaxWebhooks(); i++ {
		store.webhooks[uuid.New()] = platform.CustomerWebhook{
			ID:        uuid.New(),
			AccountID: sc.Account.ID,
		}
	}
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
		Name:   "one-too-many",
		URL:    "https://example.com/hook",
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}, sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// TestHandleCreate_QuotaIsTierAware pins the tier ladder: the same
// webhook count that 409s a Free account passes on a Pro account,
// and Config.WebhookQuotas overrides the ladder per tier.
func TestHandleCreate_QuotaIsTierAware(t *testing.T) {
	h, store, sc := newTestRig(t)
	for i := 0; i < platform.TierFree.MaxWebhooks(); i++ {
		store.webhooks[uuid.New()] = platform.CustomerWebhook{
			ID:        uuid.New(),
			AccountID: sc.Account.ID,
		}
	}
	hookReq := func(name string) *http.Request {
		return sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", createRequest{
			Name:   name,
			URL:    "https://example.com/hook",
			Events: []string{string(platform.WebhookEventIncidentSEV1)},
		}, sc)
	}

	// Free (rig default) is at cap → 409.
	w := httptest.NewRecorder()
	h.HandleCreate(w, hookReq("over-free"))
	if w.Code != http.StatusConflict {
		t.Fatalf("free at cap: status = %d, want 409", w.Code)
	}

	// Same account on Pro sails through.
	sc.Account.Tier = platform.TierPro
	w = httptest.NewRecorder()
	h.HandleCreate(w, hookReq("pro-ok"))
	if w.Code != http.StatusCreated {
		t.Fatalf("pro tier: status = %d (body=%s), want 201", w.Code, w.Body.String())
	}

	// Config override: cap Pro at 1 — the account (now over it) 409s.
	h2, err := NewHandlers(Config{
		Webhooks:      store,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		WebhookQuotas: map[platform.Tier]int{platform.TierPro: 1},
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	w = httptest.NewRecorder()
	h2.HandleCreate(w, hookReq("over-override"))
	if w.Code != http.StatusConflict {
		t.Errorf("pro override cap 1: status = %d, want 409", w.Code)
	}
}

func TestHandleList_ScopesToAccount(t *testing.T) {
	h, store, sc := newTestRig(t)
	// Mine
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID, Name: "mine",
		URL: "https://x.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	// Someone else's — must NOT appear in the response
	stranger := uuid.New()
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(), Name: "stranger",
		URL: "https://y.example", Events: []string{"incident.sev1"}, Enabled: true,
	}

	req := sessionReq(t, http.MethodGet, "/v1/dashboard/webhooks", nil, sc)
	w := httptest.NewRecorder()
	h.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp listResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Webhooks) != 1 {
		t.Fatalf("expected 1 webhook in response, got %d", len(resp.Webhooks))
	}
	if resp.Webhooks[0].Name != "mine" {
		t.Errorf("returned wrong webhook: %v", resp.Webhooks[0])
	}
}

func TestHandleDelete_RejectsCrossAccount(t *testing.T) {
	h, store, sc := newTestRig(t)
	stranger := uuid.New()
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(),
		URL: "https://y.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	req := sessionReq(t, http.MethodDelete, "/v1/dashboard/webhooks/"+stranger.String(), nil, sc)
	req.SetPathValue("id", stranger.String())
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	// Cross-account must look like not-found, not 403 — don't
	// leak existence.
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no existence leak)", w.Code)
	}
	// Webhook must still exist after the rejected delete.
	if _, ok := store.webhooks[stranger]; !ok {
		t.Error("cross-account delete should not have removed the row")
	}
}

// TestHandleDelete_HappyPath — owner deletes their own webhook;
// row gone, 204.
func TestHandleDelete_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	req := sessionReq(t, http.MethodDelete, "/v1/dashboard/webhooks/"+mine.String(), nil, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := store.webhooks[mine]; ok {
		t.Error("row should be deleted")
	}
}

// TestHandleUpdate_HappyPath — owner patches name + enabled; the
// resulting row carries the new values, secret + account id stay
// immutable.
func TestHandleUpdate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	originalSecret := []byte("original-secret")
	store.webhooks[mine] = platform.CustomerWebhook{
		ID:         mine,
		AccountID:  sc.Account.ID,
		Name:       "before",
		URL:        "https://before.example/hook",
		SecretHash: originalSecret,
		Events:     []string{"incident.sev1"},
		Enabled:    true,
	}

	falseB := false
	patch := updateRequest{
		Name:    strPtr("after"),
		Enabled: &falseB,
	}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+mine.String(), patch, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := store.webhooks[mine]
	if got.Name != "after" {
		t.Errorf("Name = %q, want after", got.Name)
	}
	if got.Enabled {
		t.Errorf("Enabled should be false after update")
	}
	if string(got.SecretHash) != string(originalSecret) {
		t.Errorf("SecretHash mutated: got %q, want %q", got.SecretHash, originalSecret)
	}
	if got.AccountID != sc.Account.ID {
		t.Errorf("AccountID mutated: got %v, want %v", got.AccountID, sc.Account.ID)
	}
}

// TestHandleUpdate_RejectsCrossAccount — same existence-leak
// posture as Delete.
func TestHandleUpdate_RejectsCrossAccount(t *testing.T) {
	h, store, sc := newTestRig(t)
	stranger := uuid.New()
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(),
		Name: "stranger", URL: "https://x.example",
		Events: []string{"incident.sev1"}, Enabled: true,
	}
	patch := updateRequest{Name: strPtr("renamed")}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+stranger.String(), patch, sc)
	req.SetPathValue("id", stranger.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if store.webhooks[stranger].Name != "stranger" {
		t.Error("cross-account update should not have mutated the row")
	}
}

// TestHandleUpdate_RejectsBadURL — PATCHing an http:// URL must
// 400 (HTTPS-only contract).
func TestHandleUpdate_RejectsBadURL(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	patch := updateRequest{URL: strPtr("http://insecure.example/hook")}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+mine.String(), patch, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if store.webhooks[mine].URL != "https://ok.example" {
		t.Error("rejected update should have preserved the original URL")
	}
}

// TestHandleUpdate_RejectsEmptyName pins T154: PATCHing an empty (or
// whitespace-only) name must 400, matching the create path's
// validation, instead of silently writing an empty name.
func TestHandleUpdate_RejectsEmptyName(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		Name: "before", URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	patch := updateRequest{Name: strPtr("   ")}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+mine.String(), patch, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if store.webhooks[mine].Name != "before" {
		t.Errorf("rejected update should have preserved the original name, got %q", store.webhooks[mine].Name)
	}
}

// failGetStore wraps fakeStore and makes GetWebhook fail starting on
// its THIRD call, simulating a transient read error on the
// post-update reload while leaving the handler's two earlier lookups
// (parseAndAuthorise, then the pre-patch "current" fetch) unaffected.
type failGetStore struct {
	*fakeStore
	calls int
}

func (s *failGetStore) GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	s.calls++
	if s.calls > 2 {
		return platform.CustomerWebhook{}, errors.New("reload failed")
	}
	return s.fakeStore.GetWebhook(ctx, id)
}

// TestHandleUpdate_ReloadFailureReturns500 pins T155: when the
// post-update GetWebhook reload fails, the handler must surface a
// 500, not silently write 200 with a zero-value DTO.
func TestHandleUpdate_ReloadFailureReturns500(t *testing.T) {
	store := newFakeStore()
	sc := dashboardauth.SessionContext{
		Session: platform.Session{ID: uuid.New(), UserID: uuid.New()},
		User:    platform.User{ID: uuid.New(), Email: "owner@example.com", Role: platform.RoleOwner},
		Account: platform.Account{ID: uuid.New(), Slug: "example", Tier: platform.TierFree, Status: platform.AccountActive},
	}
	sc.User.AccountID = sc.Account.ID
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}

	h, err := NewHandlers(Config{
		Webhooks: &failGetStore{fakeStore: store},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}

	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+mine.String(), updateRequest{Name: strPtr("renamed")}, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the post-update reload fails", w.Code)
	}
}

// TestHandleListDeliveries_ZeroTimestampsOmitted pins T153: a
// delivery row with no scheduled retry and no delivery yet must
// genuinely omit next_attempt_at/delivered_at, not serialize the
// year-1 zero timestamp (omitempty is a no-op on a struct-typed
// time.Time).
func TestHandleListDeliveries_ZeroTimestampsOmitted(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	store.deliveries[mine] = []platform.WebhookDelivery{
		{ID: uuid.New(), WebhookID: mine, EventType: "incident.sev1"},
	}

	req := sessionReq(t, http.MethodGet, "/v1/dashboard/webhooks/"+mine.String()+"/deliveries", nil, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleListDeliveries(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var raw struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Deliveries) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(raw.Deliveries))
	}
	d := raw.Deliveries[0]
	if v, present := d["next_attempt_at"]; present {
		t.Errorf("next_attempt_at should be omitted when zero, got %v", v)
	}
	if v, present := d["delivered_at"]; present {
		t.Errorf("delivered_at should be omitted when zero, got %v", v)
	}
}

// TestHandleListDeliveries_HappyPath — returns the delivery log
// for the caller's own webhook.
func TestHandleListDeliveries_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := uuid.New()
	store.webhooks[mine] = platform.CustomerWebhook{
		ID: mine, AccountID: sc.Account.ID,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	// Seed two delivery rows.
	store.deliveries[mine] = []platform.WebhookDelivery{
		{ID: uuid.New(), WebhookID: mine, EventType: "incident.sev1", AttemptCount: 1, LastResponseStatus: 200},
		{ID: uuid.New(), WebhookID: mine, EventType: "anomaly.freeze", AttemptCount: 3, LastResponseStatus: 503},
	}

	req := sessionReq(t, http.MethodGet, "/v1/dashboard/webhooks/"+mine.String()+"/deliveries", nil, sc)
	req.SetPathValue("id", mine.String())
	w := httptest.NewRecorder()
	h.HandleListDeliveries(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp deliveriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Deliveries) != 2 {
		t.Errorf("got %d deliveries, want 2", len(resp.Deliveries))
	}
}

// TestHandleListDeliveries_CrossAccount404 — listing another
// account's deliveries returns 404 (existence-leak protection).
func TestHandleListDeliveries_CrossAccount404(t *testing.T) {
	h, store, sc := newTestRig(t)
	stranger := uuid.New()
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(),
		URL: "https://x.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	store.deliveries[stranger] = []platform.WebhookDelivery{
		{ID: uuid.New(), WebhookID: stranger, EventType: "incident.sev1"},
	}
	req := sessionReq(t, http.MethodGet, "/v1/dashboard/webhooks/"+stranger.String()+"/deliveries", nil, sc)
	req.SetPathValue("id", stranger.String())
	w := httptest.NewRecorder()
	h.HandleListDeliveries(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// strPtr is a tiny test helper — Go has no literal *string syntax
// and inline helpers like `&s` need a temporary variable.
func strPtr(s string) *string { return &s }

// TestValidateWebhookName_CountsCodePoints: maxLength 200 is code points
// (spec) and characters (Postgres CHECK), not bytes.
func TestValidateWebhookName_CountsCodePoints(t *testing.T) {
	if err := validateWebhookName(strings.Repeat("名", 200)); err != nil {
		t.Errorf("200 code points (600 bytes) rejected: %v", err)
	}
	if err := validateWebhookName(strings.Repeat("名", 201)); err == nil {
		t.Error("201 code points accepted")
	}
}

// TestValidateEvents_AcceptsTheCanonicalSet pins subscription validation
// to platform.WebhookEventTypes(): every member is subscribable and the
// rejection names every member, so a new type cannot be refused with a
// message that predates it (GH-1348).
func TestValidateEvents_AcceptsTheCanonicalSet(t *testing.T) {
	for _, e := range platform.WebhookEventTypes() {
		if err := validateEvents([]string{string(e)}); err != nil {
			t.Errorf("validateEvents(%q) = %v, want nil", e, err)
		}
	}
	err := validateEvents([]string{"not.a.type"})
	if err == nil {
		t.Fatal("validateEvents accepted an unknown type")
	}
	for _, e := range platform.WebhookEventTypes() {
		if !strings.Contains(err.Error(), string(e)) {
			t.Errorf("rejection %q does not name supported type %q", err.Error(), e)
		}
	}
}

// TestValidateWebhookURL_PortAndLength pins GH-828: a webhook URL is
// confined to the default https port and a 2048-byte ceiling, so a
// registration cannot aim signed POSTs at an arbitrary TCP service or
// store a multi-KiB URL that every failed attempt copies into last_error.
func TestValidateWebhookURL_PortAndLength(t *testing.T) {
	base := "https://hooks.example/"
	for _, tc := range []struct {
		name, url string
		ok        bool
	}{
		{"default port", base + "hook", true},
		{"explicit 443", "https://hooks.example:443/hook", true},
		{"smtp port", "https://hooks.example:25/hook", false},
		{"alt https port", "https://hooks.example:8443/hook", false},
		{"at the cap", base + strings.Repeat("a", 2048-len(base)), true},
		{"one past the cap", base + strings.Repeat("a", 2048-len(base)+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWebhookURL(context.Background(), tc.url)
			if tc.ok && err != nil {
				t.Fatalf("validateWebhookURL(%d bytes) = %v, want nil", len(tc.url), err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validateWebhookURL(%.40q, %d bytes) accepted, want rejection", tc.url, len(tc.url))
			}
		})
	}
}

// TestHandleCreate_DuplicateURLConflicts pins GH-828's UNIQUE
// (account_id, url): a second registration of the same destination is a
// 409, not a second row fanning every event out to it again.
func TestHandleCreate_DuplicateURLConflicts(t *testing.T) {
	h, store, sc := newTestRig(t)
	body := createRequest{
		Name:   "ops",
		URL:    "https://hooks.example/dup",
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}
	for i, want := range []int{http.StatusCreated, http.StatusConflict} {
		w := httptest.NewRecorder()
		h.HandleCreate(w, sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", body, sc))
		if w.Code != want {
			t.Fatalf("create #%d status = %d, want %d; body=%s", i+1, w.Code, want, w.Body.String())
		}
	}
	if len(store.webhooks) != 1 {
		t.Errorf("store holds %d webhooks, want 1", len(store.webhooks))
	}
}

// TestHandleUpdate_DuplicateURLConflicts is the PATCH half of the same
// uniqueness: moving a webhook onto a sibling's url is a 409.
func TestHandleUpdate_DuplicateURLConflicts(t *testing.T) {
	h, store, sc := newTestRig(t)
	a, b := uuid.New(), uuid.New()
	for id, u := range map[uuid.UUID]string{a: "https://a.example/hook", b: "https://b.example/hook"} {
		store.webhooks[id] = platform.CustomerWebhook{
			ID: id, AccountID: sc.Account.ID, Name: "n", URL: u,
			Events: []string{"incident.sev1"}, Enabled: true,
		}
	}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/webhooks/"+b.String(),
		updateRequest{URL: strPtr("https://a.example/hook")}, sc)
	req.SetPathValue("id", b.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if got := store.webhooks[b].URL; got != "https://b.example/hook" {
		t.Errorf("rejected update rewrote url to %q", got)
	}
}
