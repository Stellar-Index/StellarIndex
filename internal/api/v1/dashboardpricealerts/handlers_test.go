package dashboardpricealerts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/StellarIndex/stellar-index/internal/api/v1/dashboardauth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// fakeStore is an in-memory platform.PriceAlertStore. A fresh instance
// per test so they can't interfere.
type fakeStore struct {
	mu     sync.Mutex
	alerts map[uuid.UUID]platform.PriceAlert
}

func newFakeStore() *fakeStore {
	return &fakeStore{alerts: map[uuid.UUID]platform.PriceAlert{}}
}

func (s *fakeStore) CreatePriceAlert(_ context.Context, a platform.PriceAlert, maxPerAccount int) (platform.PriceAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxPerAccount > 0 {
		count := 0
		for _, e := range s.alerts {
			if e.AccountID == a.AccountID {
				count++
			}
		}
		if count >= maxPerAccount {
			return platform.PriceAlert{}, platform.ErrPriceAlertQuotaExceeded
		}
	}
	a.ID = uuid.New()
	a.CreatedAt = time.Now().UTC()
	a.UpdatedAt = a.CreatedAt
	s.alerts[a.ID] = a
	return a, nil
}

func (s *fakeStore) GetPriceAlert(_ context.Context, id uuid.UUID) (platform.PriceAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.alerts[id]; ok {
		return a, nil
	}
	return platform.PriceAlert{}, platform.ErrNotFound
}

func (s *fakeStore) ListPriceAlertsForAccount(_ context.Context, accountID uuid.UUID) ([]platform.PriceAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []platform.PriceAlert
	for _, a := range s.alerts {
		if a.AccountID == accountID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) ListEnabledPriceAlerts(_ context.Context) ([]platform.PriceAlert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []platform.PriceAlert
	for _, a := range s.alerts {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) UpdatePriceAlert(_ context.Context, a platform.PriceAlert) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.alerts[a.ID]; !ok {
		return platform.ErrNotFound
	}
	a.UpdatedAt = time.Now().UTC()
	s.alerts[a.ID] = a
	return nil
}

func (s *fakeStore) DeletePriceAlert(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.alerts, id)
	return nil
}

func (s *fakeStore) MarkPriceAlertFired(_ context.Context, id uuid.UUID, firedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.alerts[id]; ok {
		a.LastFiredAt = firedAt
		s.alerts[id] = a
	}
	return nil
}

func newTestRig(t *testing.T, quotas map[platform.Tier]int) (*Handlers, *fakeStore, dashboardauth.SessionContext) {
	t.Helper()
	store := newFakeStore()
	h, err := NewHandlers(Config{
		Alerts:      store,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
		AlertQuotas: quotas,
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	sc := dashboardauth.SessionContext{
		Session: platform.Session{ID: uuid.New(), UserID: uuid.New()},
		User: platform.User{
			ID:    uuid.New(),
			Email: "owner@example.com",
			Role:  platform.RoleOwner,
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

func validCreate() createRequest {
	return createRequest{
		BaseAsset:       "native",
		QuoteAsset:      "fiat:USD",
		Condition:       "above",
		Threshold:       "0.15",
		CooldownSeconds: 300,
	}
}

func TestHandleCreate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", validCreate(), sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var dto priceAlertDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.ID == "" {
		t.Errorf("ID not populated")
	}
	if dto.BaseAsset != "native" || dto.QuoteAsset != "fiat:USD" || dto.Condition != "above" || dto.Threshold != "0.15" {
		t.Errorf("unexpected DTO: %+v", dto)
	}
	if !dto.Enabled {
		t.Errorf("enabled should default true")
	}
	if len(store.alerts) != 1 {
		t.Errorf("store should contain 1 alert, got %d", len(store.alerts))
	}
}

func TestHandleCreate_AnonRejected401(t *testing.T) {
	h, _, _ := newTestRig(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboard/price-alerts", nil)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandleCreate_ViewerCannotManage(t *testing.T) {
	h, _, sc := newTestRig(t, nil)
	sc.User.Role = platform.RoleViewer
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", validCreate(), sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandleCreate_Validation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*createRequest)
	}{
		{"missing base", func(c *createRequest) { c.BaseAsset = "" }},
		{"bad asset", func(c *createRequest) { c.QuoteAsset = "not a valid asset!!" }},
		{"bad condition", func(c *createRequest) { c.Condition = "sideways" }},
		{"negative threshold", func(c *createRequest) { c.Threshold = "-1" }},
		{"zero threshold", func(c *createRequest) { c.Threshold = "0" }},
		{"fraction threshold", func(c *createRequest) { c.Threshold = "3/2" }},
		{"scientific threshold", func(c *createRequest) { c.Threshold = "1e3" }},
		{"negative cooldown", func(c *createRequest) { c.CooldownSeconds = -5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, sc := newTestRig(t, nil)
			body := validCreate()
			tc.mut(&body)
			req := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", body, sc)
			w := httptest.NewRecorder()
			h.HandleCreate(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleCreate_QuotaExceeded409(t *testing.T) {
	h, _, sc := newTestRig(t, map[platform.Tier]int{platform.TierFree: 1})
	// First create succeeds.
	req := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", validCreate(), sc)
	h.HandleCreate(httptest.NewRecorder(), req)
	// Second hits the cap.
	req2 := sessionReq(t, http.MethodPost, "/v1/dashboard/price-alerts", validCreate(), sc)
	w := httptest.NewRecorder()
	h.HandleCreate(w, req2)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestHandleList(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	seed := platform.PriceAlert{AccountID: sc.Account.ID, BaseAsset: "native", QuoteAsset: "fiat:USD", Condition: platform.AlertAbove, Threshold: "0.2", Enabled: true}
	if _, err := store.CreatePriceAlert(context.Background(), seed, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// An alert for a different account must not appear.
	other := seed
	other.AccountID = uuid.New()
	_, _ = store.CreatePriceAlert(context.Background(), other, 0)

	req := sessionReq(t, http.MethodGet, "/v1/dashboard/price-alerts", nil, sc)
	w := httptest.NewRecorder()
	h.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Alerts) != 1 {
		t.Errorf("want 1 alert for this account, got %d", len(resp.Alerts))
	}
}

func TestHandleUpdate_HappyPath(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	seed := platform.PriceAlert{AccountID: sc.Account.ID, BaseAsset: "native", QuoteAsset: "fiat:USD", Condition: platform.AlertAbove, Threshold: "0.2", Enabled: true}
	created, _ := store.CreatePriceAlert(context.Background(), seed, 0)

	dis := false
	body := updateRequest{Threshold: ptr("0.99"), Enabled: &dis}
	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/price-alerts/"+created.ID.String(), body, sc)
	req.SetPathValue("id", created.ID.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	got := store.alerts[created.ID]
	if got.Threshold != "0.99" || got.Enabled {
		t.Errorf("update not applied: %+v", got)
	}
}

func TestHandleUpdate_CrossAccount404(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	// Alert owned by a DIFFERENT account.
	seed := platform.PriceAlert{AccountID: uuid.New(), BaseAsset: "native", QuoteAsset: "fiat:USD", Condition: platform.AlertAbove, Threshold: "0.2", Enabled: true}
	created, _ := store.CreatePriceAlert(context.Background(), seed, 0)

	req := sessionReq(t, http.MethodPatch, "/v1/dashboard/price-alerts/"+created.ID.String(), updateRequest{Threshold: ptr("0.5")}, sc)
	req.SetPathValue("id", created.ID.String())
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no cross-account leak)", w.Code)
	}
}

func TestHandleDelete(t *testing.T) {
	h, store, sc := newTestRig(t, nil)
	seed := platform.PriceAlert{AccountID: sc.Account.ID, BaseAsset: "native", QuoteAsset: "fiat:USD", Condition: platform.AlertBelow, Threshold: "0.1", Enabled: true}
	created, _ := store.CreatePriceAlert(context.Background(), seed, 0)

	req := sessionReq(t, http.MethodDelete, "/v1/dashboard/price-alerts/"+created.ID.String(), nil, sc)
	req.SetPathValue("id", created.ID.String())
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	if _, ok := store.alerts[created.ID]; ok {
		t.Errorf("alert not deleted")
	}
}

func ptr[T any](v T) *T { return &v }
