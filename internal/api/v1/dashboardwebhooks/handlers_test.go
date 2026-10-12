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
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
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

// TestHandleCreate_DuplicateURLConflicts pins the UNIQUE (account_id, url): a second registration
// of the same destination is a 409, not a second row fanning every event out to it again.
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

// TestSpecEventTypeEnumsMatchPlatform keeps every OpenAPI enum spanning more than one webhook
// event family (e.g. incident.* and price.*) equal to platform.WebhookEventTypes(). The spec
// feeds the explorer's generated types and customers, so a type missing there is as
// unsubscribable as one missing from validateEvents. Single-family enums (a payload's own
// `event` field) are subsets by design and are not checked.
func TestSpecEventTypeEnumsMatchPlatform(t *testing.T) {
	spec, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	want := make([]string, 0, len(platform.WebhookEventTypes()))
	for _, e := range platform.WebhookEventTypes() {
		want = append(want, string(e))
	}
	slices.Sort(want)

	checked := 0
	for _, enum := range specEnums(strings.Split(string(spec), "\n")) {
		if !spansEventFamilies(enum.values) {
			continue
		}
		checked++
		got := slices.Clone(enum.values)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("openapi line %d: event-type enum %v, want %v", enum.line, got, want)
		}
	}
	if checked == 0 {
		t.Fatal("found no event-type enum in the spec — the parser is not looking at the right shape")
	}
}

type specEnum struct {
	line   int
	values []string
}

// specEnums extracts every `enum:` list, inline (`enum: [a, b]`) or block
// (`enum:` followed by `- a` lines).
func specEnums(lines []string) []specEnum {
	var out []specEnum
	for i, ln := range lines {
		idx := strings.Index(ln, "enum:")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(ln[idx+len("enum:"):])
		if open := strings.Index(rest, "["); open >= 0 {
			if end := strings.Index(rest, "]"); end > open {
				var vals []string
				for _, v := range strings.Split(rest[open+1:end], ",") {
					vals = append(vals, strings.Trim(strings.TrimSpace(v), `"'`))
				}
				out = append(out, specEnum{line: i + 1, values: vals})
			}
			continue
		}
		var vals []string
		for _, next := range lines[i+1:] {
			item := strings.TrimSpace(next)
			if !strings.HasPrefix(item, "- ") {
				break
			}
			vals = append(vals, strings.Trim(strings.TrimSpace(item[2:]), `"'`))
		}
		out = append(out, specEnum{line: i + 1, values: vals})
	}
	return out
}

// spansEventFamilies reports whether values are webhook event types from
// at least two families (the part before the dot).
func spansEventFamilies(values []string) bool {
	families := map[string]bool{}
	for _, v := range values {
		if !platform.IsWebhookEventType(v) {
			continue
		}
		family, _, _ := strings.Cut(v, ".")
		families[family] = true
	}
	return len(families) > 1
}

func rotateViaMount(t *testing.T, mux *http.ServeMux, sc dashboardauth.SessionContext, id uuid.UUID, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+id.String()+"/rotate-secret", nil, sc)
	req.Host = "api.stellarindex.io"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://api.stellarindex.io")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestMount_RotateSecret_KeepsWebhookAndQueue: rotation replaces the key
// on the SAME webhook, keeps the old key for the overlap window, and
// leaves the queued deliveries in place — the delete + recreate it
// replaces lost all three.
func TestMount_RotateSecret_KeepsWebhookAndQueue(t *testing.T) {
	h, store, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	id := uuid.New()
	oldKey := []byte("wsec_old")
	store.webhooks[id] = platform.CustomerWebhook{
		ID: id, AccountID: sc.Account.ID, SigningKey: oldKey,
		URL: "https://ok.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	store.deliveries[id] = []platform.WebhookDelivery{{ID: uuid.New(), WebhookID: id, EventType: "incident.sev1"}}

	w := rotateViaMount(t, mux, sc, id, "rotate-1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp rotateSecretResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WebhookID != id.String() {
		t.Errorf("webhook_id = %q, want %s", resp.WebhookID, id)
	}
	wantExpiry := h.cfg.Now().Add(previousSecretOverlap)
	if !time.Time(resp.PreviousSecretExpiresAt).Equal(wantExpiry) {
		t.Errorf("previous_secret_expires_at = %v, want %v", time.Time(resp.PreviousSecretExpiresAt), wantExpiry)
	}

	got := store.webhooks[id]
	if string(got.SigningKey) != resp.Secret || resp.Secret == string(oldKey) || len(resp.Secret) < 10 {
		t.Errorf("stored key %q, returned %q: want the fresh returned key stored", got.SigningKey, resp.Secret)
	}
	if string(got.PreviousSigningKey) != string(oldKey) || !got.PreviousSecretExpiresAt.Equal(wantExpiry) {
		t.Errorf("previous = (%q, %v), want the old key until %v", got.PreviousSigningKey, got.PreviousSecretExpiresAt, wantExpiry)
	}
	if len(store.deliveries[id]) != 1 {
		t.Errorf("queued deliveries = %d after rotate, want the 1 queued before it", len(store.deliveries[id]))
	}

	// A retried rotate must not rotate again: that would push the key the
	// receiver still holds out of the overlap.
	replay := rotateViaMount(t, mux, sc, id, "rotate-1")
	var again rotateSecretResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if again.Secret != resp.Secret || string(store.webhooks[id].PreviousSigningKey) != string(oldKey) {
		t.Error("idempotent replay rotated the key a second time")
	}
}

func TestHandleRotateSecret_CrossAccountAndRole(t *testing.T) {
	h, store, sc := newTestRig(t)
	stranger := uuid.New()
	strangerKey := []byte("wsec_stranger")
	store.webhooks[stranger] = platform.CustomerWebhook{
		ID: stranger, AccountID: uuid.New(), SigningKey: strangerKey,
		URL: "https://y.example", Events: []string{"incident.sev1"}, Enabled: true,
	}
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+stranger.String()+"/rotate-secret", nil, sc)
	req.SetPathValue("id", stranger.String())
	w := httptest.NewRecorder()
	h.HandleRotateSecret(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("cross-account rotate: status = %d, want 404", w.Code)
	}
	if string(store.webhooks[stranger].SigningKey) != string(strangerKey) {
		t.Error("cross-account rotate changed the other account's key")
	}

	viewer := sc
	viewer.User.Role = platform.RoleViewer
	req = sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks/"+stranger.String()+"/rotate-secret", nil, viewer)
	req.SetPathValue("id", stranger.String())
	w = httptest.NewRecorder()
	h.HandleRotateSecret(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("viewer rotate: status = %d, want 403", w.Code)
	}
}

// TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering: a client retrying POST
// /v1/dashboard/webhooks with the same Idempotency-Key after a timeout gets the ORIGINAL webhook
// and signing secret replayed, not a second webhook with a secret it never saw. Routed through
// h.Mount so the wired middleware is exercised.
func TestMount_CreateIdempotencyKey_ReplaysInsteadOfRegistering(t *testing.T) {
	h, store, sc := newTestRig(t)
	mux := http.NewServeMux()
	h.Mount(mux, middleware.NewPublicRoutes())

	body := createRequest{
		Name:   "ops-slack",
		URL:    "https://hooks.slack.example/services/T/B/X",
		Events: []string{string(platform.WebhookEventIncidentSEV1)},
	}
	send := func() createResponse {
		t.Helper()
		req := sessionReq(t, http.MethodPost, "/v1/dashboard/webhooks", body, sc)
		req.Host = "api.stellarindex.io"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", "https://api.stellarindex.io")
		req.Header.Set("Idempotency-Key", "retry-wh-456")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var resp createResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp
	}

	first, second := send(), send()
	if second.Webhook.ID != first.Webhook.ID {
		t.Errorf("replay registered a DIFFERENT webhook: first id = %s, second id = %s",
			first.Webhook.ID, second.Webhook.ID)
	}
	if second.Secret != first.Secret {
		t.Error("replay returned a different signing secret than the original create")
	}

	store.mu.Lock()
	registered := len(store.webhooks)
	store.mu.Unlock()
	if registered != 1 {
		t.Errorf("store registered %d webhooks for two identical Idempotency-Key requests, want 1", registered)
	}
}

// TestSpecDeclaresParseAndAuthoriseStatuses keeps the published contract
// of every /dashboard/webhooks/{id}* operation in step with
// parseAndAuthorise, which answers 400 for a malformed id and 404 for an
// absent or cross-account one. DELETE once promised "204 on absent" and
// declared neither, so a generated client read a retried delete's 404 as
// an undeclared error.
func TestSpecDeclaresParseAndAuthoriseStatuses(t *testing.T) {
	raw, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	checked := 0
	for path, ops := range spec.Paths {
		if !strings.HasPrefix(path, "/dashboard/webhooks/{id}") {
			continue
		}
		for method, op := range ops {
			if method == "parameters" {
				continue
			}
			checked++
			for _, status := range []string{"400", "404"} {
				if _, ok := op.Responses[status]; !ok {
					t.Errorf("%s %s does not declare %s, which parseAndAuthorise returns", strings.ToUpper(method), path, status)
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("checked %d /dashboard/webhooks/{id} operations, want >= 3 (PATCH, DELETE, deliveries)", checked)
	}
}
