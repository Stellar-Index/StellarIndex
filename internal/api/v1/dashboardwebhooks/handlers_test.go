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
	"reflect"
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

func (s *fakeStore) RotateWebhookSecret(_ context.Context, id uuid.UUID, newSecret []byte, previousExpiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.webhooks[id]
	if !ok {
		return platform.ErrNotFound
	}
	w.PreviousSigningKey, w.PreviousSecretExpiresAt = w.SigningKey, previousExpiresAt
	w.SigningKey = newSecret
	s.webhooks[id] = w
	return nil
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
	return newRigWith(t, func(s *fakeStore) platform.WebhookStore { return s })
}

// newRigWith builds the rig over a store derived from the fake, for tests
// that need a failing or degraded store.
func newRigWith(t *testing.T, wrap func(*fakeStore) platform.WebhookStore) (*Handlers, *fakeStore, dashboardauth.SessionContext) {
	t.Helper()
	store := newFakeStore()
	h, err := NewHandlers(Config{
		Webhooks: wrap(store),
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

const hooksPath = "/v1/dashboard/webhooks"

// call invokes a handler as the session; a non-nil id is set as the {id}
// path value and appended to the target, with suffix after it.
func call(t *testing.T, fn http.HandlerFunc, method, suffix string, id uuid.UUID, body any, sc dashboardauth.SessionContext) *httptest.ResponseRecorder {
	t.Helper()
	target := hooksPath
	if id != uuid.Nil {
		target += "/" + id.String()
	}
	req := sessionReq(t, method, target+suffix, body, sc)
	if id != uuid.Nil {
		req.SetPathValue("id", id.String())
	}
	w := httptest.NewRecorder()
	fn(w, req)
	return w
}

// seedHook stores a webhook for accountID (pass uuid.New() for a stranger's)
// and returns its id.
func seedHook(store *fakeStore, accountID uuid.UUID, mut func(*platform.CustomerWebhook)) uuid.UUID {
	id := uuid.New()
	w := platform.CustomerWebhook{
		ID: id, AccountID: accountID, Name: "n",
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	if mut != nil {
		mut(&w)
	}
	store.webhooks[id] = w
	return id
}

func createBody(name, url string, events ...platform.WebhookEventType) createRequest {
	evs := make([]string, len(events))
	for i, e := range events {
		evs[i] = string(e)
	}
	return createRequest{Name: name, URL: url, Events: evs}
}

func TestHandleCreate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	w := call(t, h.HandleCreate, http.MethodPost, "", uuid.Nil,
		createBody("ops-slack", "https://hooks.slack.example/services/T/B/X", platform.WebhookEventIncidentSEV1), sc)

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
	w := httptest.NewRecorder()
	h.HandleCreate(w, httptest.NewRequest(http.MethodPost, hooksPath, nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// TestHandleCreate_Rejections pins the create gates. The SSRF rows pin the
// registration guard: internal, loopback, link-local, private, CGN and
// cloud-metadata destinations are refused, as are userinfo-embedded URLs.
func TestHandleCreate_Rejections(t *testing.T) {
	type createCase struct {
		name   string
		role   platform.Role
		url    string
		events []platform.WebhookEventType
		want   int
	}
	cases := []createCase{
		{
			name: "viewer cannot manage", role: platform.RoleViewer, url: "https://example.com/hook",
			events: []platform.WebhookEventType{platform.WebhookEventAnomalyFreeze}, want: http.StatusForbidden,
		},
		{
			name: "plain http", url: "http://example.com/hook",
			events: []platform.WebhookEventType{platform.WebhookEventIncidentSEV1}, want: http.StatusBadRequest,
		},
		{
			name: "unknown event type", url: "https://example.com/hook",
			events: []platform.WebhookEventType{"made.up.event"}, want: http.StatusBadRequest,
		},
	}
	for name, u := range map[string]string{
		"ssrf loopback IPv4 literal": "https://127.0.0.1/hook",
		"ssrf loopback IPv6 literal": "https://[::1]/hook",
		"ssrf RFC1918 10/8":          "https://10.0.0.1/hook",
		"ssrf RFC1918 192.168":       "https://192.168.1.1/hook",
		"ssrf RFC1918 172.16":        "https://172.16.0.1/hook",
		"ssrf link-local":            "https://169.254.169.254/latest/meta-data/",
		"ssrf unspecified":           "https://0.0.0.0/hook",
		"ssrf CGN 100.64":            "https://100.64.0.1/hook",
		"ssrf userinfo embedded":     "https://user:pass@example.com/hook",
		"ssrf empty hostname":        "https:///hook",
	} {
		cases = append(cases, createCase{name, "", u, []platform.WebhookEventType{platform.WebhookEventIncidentSEV1}, http.StatusBadRequest})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			if tc.role != "" {
				sc.User.Role = tc.role
			}
			w := call(t, h.HandleCreate, http.MethodPost, "", uuid.Nil, createBody("ops", tc.url, tc.events...), sc)
			if w.Code != tc.want {
				t.Errorf("status = %d (body=%s), want %d for %q", w.Code, w.Body.String(), tc.want, tc.url)
			}
			if len(store.webhooks) != 0 {
				t.Errorf("rejected create stored %d webhooks", len(store.webhooks))
			}
		})
	}
}

// TestBlockedResolvedAddrError_DoesNotLeakResolvedIP: the error
// validateWebhookURL returns for a DNS-resolved internal address must
// name the customer-supplied hostname, never the resolved IP, because
// that text is written verbatim into the 400 response body.
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

// TestHandleCreate_QuotaIsTierAware pins the tier ladder: the same
// webhook count that 409s a Free account passes on a Pro account,
// and Config.WebhookQuotas overrides the ladder per tier.
func TestHandleCreate_QuotaIsTierAware(t *testing.T) {
	h, store, sc := newTestRig(t)
	for i := 0; i < platform.TierFree.MaxWebhooks(); i++ {
		seedHook(store, sc.Account.ID, nil)
	}
	create := func(h *Handlers, name string) *httptest.ResponseRecorder {
		return call(t, h.HandleCreate, http.MethodPost, "", uuid.Nil,
			createBody(name, "https://example.com/hook", platform.WebhookEventIncidentSEV1), sc)
	}

	if w := create(h, "over-free"); w.Code != http.StatusConflict {
		t.Fatalf("free at cap: status = %d, want 409", w.Code)
	}

	sc.Account.Tier = platform.TierPro
	if w := create(h, "pro-ok"); w.Code != http.StatusCreated {
		t.Fatalf("pro tier: status = %d (body=%s), want 201", w.Code, w.Body.String())
	}

	h2, err := NewHandlers(Config{
		Webhooks:      store,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		WebhookQuotas: map[platform.Tier]int{platform.TierPro: 1},
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	if w := create(h2, "over-override"); w.Code != http.StatusConflict {
		t.Errorf("pro override cap 1: status = %d, want 409", w.Code)
	}
}

func TestHandleList_ScopesToAccount(t *testing.T) {
	h, store, sc := newTestRig(t)
	seedHook(store, sc.Account.ID, func(w *platform.CustomerWebhook) { w.Name = "mine" })
	seedHook(store, uuid.New(), func(w *platform.CustomerWebhook) { w.Name = "stranger" })

	w := call(t, h.HandleList, http.MethodGet, "", uuid.Nil, nil, sc)
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

// TestReads_RequireManageRole: endpoint URLs commonly embed the receiver's
// own credential and last_error echoes endpoint behaviour, so the reads
// carry the same owner/admin/member gate as the three mutations.
func TestReads_RequireManageRole(t *testing.T) {
	for _, role := range []platform.Role{platform.RoleViewer, platform.RoleBilling, platform.RoleMember} {
		t.Run(string(role), func(t *testing.T) {
			h, store, sc := newTestRig(t)
			sc.User.Role = role
			id := seedHook(store, sc.Account.ID, func(w *platform.CustomerWebhook) { w.URL = "https://x.example/hook?token=t" })
			want := http.StatusForbidden
			if role == platform.RoleMember {
				want = http.StatusOK
			}

			w := call(t, h.HandleList, http.MethodGet, "", uuid.Nil, nil, sc)
			if w.Code != want {
				t.Errorf("list: status = %d, want %d (body %s)", w.Code, want, w.Body.String())
			}
			w = call(t, h.HandleListDeliveries, http.MethodGet, "/deliveries", id, nil, sc)
			if w.Code != want {
				t.Errorf("deliveries: status = %d, want %d (body %s)", w.Code, want, w.Body.String())
			}
		})
	}
}

// TestHandleDelete: a cross-account delete must look like not-found, not
// 403, so existence does not leak, and must leave the row in place.
func TestHandleDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		own     bool
		want    int
		wantRow bool
	}{
		{"own webhook", true, http.StatusNoContent, false},
		{"cross-account", false, http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			acct := uuid.New()
			if tc.own {
				acct = sc.Account.ID
			}
			id := seedHook(store, acct, nil)
			w := call(t, h.HandleDelete, http.MethodDelete, "", id, nil, sc)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if _, ok := store.webhooks[id]; ok != tc.wantRow {
				t.Errorf("row present = %v, want %v", ok, tc.wantRow)
			}
		})
	}
}

// TestHandleUpdate_HappyPath: patching name + enabled changes those; the
// secret and account id stay immutable.
func TestHandleUpdate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t)
	originalSecret := []byte("original-secret")
	mine := seedHook(store, sc.Account.ID, func(w *platform.CustomerWebhook) {
		w.Name = "before"
		w.SigningKey = originalSecret
	})

	falseB := false
	w := call(t, h.HandleUpdate, http.MethodPatch, "", mine, updateRequest{Name: strPtr("after"), Enabled: &falseB}, sc)
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
	if string(got.SigningKey) != string(originalSecret) {
		t.Errorf("SigningKey mutated: got %q, want %q", got.SigningKey, originalSecret)
	}
	if got.AccountID != sc.Account.ID {
		t.Errorf("AccountID mutated: got %v, want %v", got.AccountID, sc.Account.ID)
	}
}

// TestHandleUpdate_Rejections: every rejected PATCH leaves the stored row
// untouched. Cross-account is a 404 (same existence-leak posture as
// delete); a duplicate url is the PATCH half of UNIQUE (account_id, url).
func TestHandleUpdate_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stranger bool
		patch    updateRequest
		want     int
	}{
		{"cross-account", true, updateRequest{Name: strPtr("renamed")}, http.StatusNotFound},
		{"http url", false, updateRequest{URL: strPtr("http://insecure.example/hook")}, http.StatusBadRequest},
		{"whitespace-only name", false, updateRequest{Name: strPtr("   ")}, http.StatusBadRequest},
		{"duplicate url", false, updateRequest{URL: strPtr("https://sibling.example/hook")}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			acct := sc.Account.ID
			if tc.stranger {
				acct = uuid.New()
			}
			id := seedHook(store, acct, func(w *platform.CustomerWebhook) { w.Name = "before" })
			seedHook(store, sc.Account.ID, func(w *platform.CustomerWebhook) { w.URL = "https://sibling.example/hook" })
			before := store.webhooks[id]

			w := call(t, h.HandleUpdate, http.MethodPatch, "", id, tc.patch, sc)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if !reflect.DeepEqual(store.webhooks[id], before) {
				t.Errorf("rejected update mutated the row: %+v -> %+v", before, store.webhooks[id])
			}
		})
	}
}

// failGetStore makes GetWebhook fail from its THIRD call, simulating a
// transient read error on the post-update reload while leaving the
// handler's two earlier lookups (authorise, pre-patch fetch) unaffected.
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

// A failed post-update reload must surface a 500, not a 200 with a
// zero-value DTO.
func TestHandleUpdate_ReloadFailureReturns500(t *testing.T) {
	h, store, sc := newRigWith(t, func(s *fakeStore) platform.WebhookStore { return &failGetStore{fakeStore: s} })
	mine := seedHook(store, sc.Account.ID, nil)

	w := call(t, h.HandleUpdate, http.MethodPatch, "", mine, updateRequest{Name: strPtr("renamed")}, sc)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the post-update reload fails", w.Code)
	}
}

// TestHandleListDeliveries: the caller's own log is returned, a row with no
// scheduled retry and no delivery omits next_attempt_at/delivered_at (not
// the year-1 zero time; omitempty is a no-op on time.Time), and another
// account's webhook is a 404.
func TestHandleListDeliveries(t *testing.T) {
	h, store, sc := newTestRig(t)
	mine := seedHook(store, sc.Account.ID, nil)
	store.deliveries[mine] = []platform.WebhookDelivery{
		{ID: uuid.New(), WebhookID: mine, EventType: "incident.sev1", AttemptCount: 1, LastResponseStatus: 200},
		{ID: uuid.New(), WebhookID: mine, EventType: "anomaly.freeze", AttemptCount: 3, LastResponseStatus: 503},
	}
	stranger := seedHook(store, uuid.New(), nil)
	store.deliveries[stranger] = []platform.WebhookDelivery{
		{ID: uuid.New(), WebhookID: stranger, EventType: "incident.sev1"},
	}

	w := call(t, h.HandleListDeliveries, http.MethodGet, "/deliveries", mine, nil, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var raw struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Deliveries) != 2 {
		t.Fatalf("got %d deliveries, want 2", len(raw.Deliveries))
	}
	for _, d := range raw.Deliveries {
		for _, field := range []string{"next_attempt_at", "delivered_at"} {
			if v, present := d[field]; present {
				t.Errorf("%s should be omitted when zero, got %v", field, v)
			}
		}
	}

	w = call(t, h.HandleListDeliveries, http.MethodGet, "/deliveries", stranger, nil, sc)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-account: status = %d, want 404", w.Code)
	}
}

// strPtr: Go has no literal *string syntax.
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
// message that predates it.
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

// TestValidateWebhookURL_PortAndLength pins that a webhook URL is
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

// TestHandleCreate_DuplicateURLConflicts pins the UNIQUE
// (account_id, url): a second registration of the same destination is a
// 409, not a second row fanning every event out to it again.
// TestHandleCreate_DuplicateURLConflicts pins the UNIQUE
// (account_id, url): a second registration of the same destination is a
// 409, not a second row fanning every event out to it again.
func TestHandleCreate_DuplicateURLConflicts(t *testing.T) {
	h, store, sc := newTestRig(t)
	body := createBody("ops", "https://hooks.example/dup", platform.WebhookEventIncidentSEV1)
	for i, want := range []int{http.StatusCreated, http.StatusConflict} {
		w := call(t, h.HandleCreate, http.MethodPost, "", uuid.Nil, body, sc)
		if w.Code != want {
			t.Fatalf("create #%d status = %d, want %d; body=%s", i+1, w.Code, want, w.Body.String())
		}
	}
	if len(store.webhooks) != 1 {
		t.Errorf("store holds %d webhooks, want 1", len(store.webhooks))
	}
}

// TestDTOs_TimestampsRenderUTC pins the wire rendering of every webhook
// and delivery timestamp: Postgres hands timestamptz back in the process's
// local zone, and a raw time.Time field would emit that offset instead of Z.
func TestDTOs_TimestampsRenderUTC(t *testing.T) {
	at := time.Date(2026, 6, 1, 2, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	hook, err := json.Marshal(toDTO(platform.CustomerWebhook{CreatedAt: at, UpdatedAt: at}))
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	delivery, err := json.Marshal(toDeliveryDTO(platform.WebhookDelivery{
		NextAttemptAt: at, DeliveredAt: at, CreatedAt: at,
	}))
	if err != nil {
		t.Fatalf("marshal delivery: %v", err)
	}
	for body, fields := range map[string][]string{
		string(hook):     {"created_at", "updated_at"},
		string(delivery): {"next_attempt_at", "delivered_at", "created_at"},
	} {
		for _, field := range fields {
			if want := `"` + field + `":"2026-06-01T00:00:00Z"`; !strings.Contains(body, want) {
				t.Errorf("DTO %s: want %s, got %s", field, want, body)
			}
		}
	}
}

// unsealableStore is fakeStore whose GetWebhook behaves like the Postgres
// store with a missing or wrong seal key: the row comes back without its
// signing key, beside an ErrWebhookKeyUnsealable.
type unsealableStore struct{ *fakeStore }

func (s unsealableStore) GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	w, err := s.fakeStore.GetWebhook(ctx, id)
	if err != nil {
		return w, err
	}
	w.SigningKey = nil
	return w, platform.ErrWebhookSealKeyMissing
}

// An unopenable signing key must not block the owner from editing or
// deleting the webhook: delete + recreate is the recovery from a lost
// seal key.
func TestHandlers_EditAndDeleteDoNotNeedSigningKey(t *testing.T) {
	h, store, sc := newRigWith(t, func(s *fakeStore) platform.WebhookStore { return unsealableStore{s} })
	mine := seedHook(store, sc.Account.ID, func(w *platform.CustomerWebhook) { w.Name = "before" })

	w := call(t, h.HandleUpdate, http.MethodPatch, "", mine, updateRequest{Name: strPtr("renamed")}, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := store.webhooks[mine].Name; got != "renamed" {
		t.Errorf("PATCH stored name %q, want renamed", got)
	}

	w = call(t, h.HandleDelete, http.MethodDelete, "", mine, nil, sc)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if _, ok := store.webhooks[mine]; ok {
		t.Error("DELETE left the webhook in place")
	}
}

// TestHandleDelete_AbsentID404 pins the behaviour the spec now documents:
// deleting an id this account does not have is 404, never a 204.
func TestHandleDelete_AbsentID404(t *testing.T) {
	h, _, sc := newTestRig(t)
	absent := uuid.New().String()
	req := sessionReq(t, http.MethodDelete, "/v1/dashboard/webhooks/"+absent, nil, sc)
	req.SetPathValue("id", absent)
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
