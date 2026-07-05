// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	v1 "github.com/StellarIndex/stellar-index/internal/api/v1"
	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

// fakeStatusNoticeStore implements v1.StatusNoticeStore in memory.
type fakeStatusNoticeStore struct {
	byID        map[uuid.UUID]platform.StatusNotice
	order       []uuid.UUID
	createErr   error
	listErr     error
	resolveErr  error
	createCalls int
}

func newFakeStatusNoticeStore() *fakeStatusNoticeStore {
	return &fakeStatusNoticeStore{byID: map[uuid.UUID]platform.StatusNotice{}}
}

func (f *fakeStatusNoticeStore) Create(_ context.Context, n platform.StatusNotice) (platform.StatusNotice, error) {
	f.createCalls++
	if f.createErr != nil {
		return platform.StatusNotice{}, f.createErr
	}
	n.ID = uuid.New()
	n.Status = platform.NoticeActive
	n.CreatedAt = time.Now().UTC()
	n.UpdatedAt = n.CreatedAt
	f.byID[n.ID] = n
	f.order = append([]uuid.UUID{n.ID}, f.order...)
	return n, nil
}

func (f *fakeStatusNoticeStore) Get(_ context.Context, id uuid.UUID) (platform.StatusNotice, error) {
	n, ok := f.byID[id]
	if !ok {
		return platform.StatusNotice{}, platform.ErrNotFound
	}
	return n, nil
}

func (f *fakeStatusNoticeStore) ListActive(_ context.Context) ([]platform.StatusNotice, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []platform.StatusNotice
	for _, id := range f.order {
		if n := f.byID[id]; n.Status == platform.NoticeActive {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeStatusNoticeStore) List(_ context.Context, _ int) ([]platform.StatusNotice, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []platform.StatusNotice
	for _, id := range f.order {
		out = append(out, f.byID[id])
	}
	return out, nil
}

func (f *fakeStatusNoticeStore) Resolve(_ context.Context, id uuid.UUID) (platform.StatusNotice, error) {
	if f.resolveErr != nil {
		return platform.StatusNotice{}, f.resolveErr
	}
	n, ok := f.byID[id]
	if !ok {
		return platform.StatusNotice{}, platform.ErrNotFound
	}
	n.Status = platform.NoticeResolved
	if n.ResolvedAt.IsZero() {
		n.ResolvedAt = time.Now().UTC()
	}
	f.byID[id] = n
	return n, nil
}

func newStatusNoticeServer(t *testing.T, subject auth.Subject, store v1.StatusNoticeStore, sink v1.AuditSink) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:          fakeAuthMiddleware(subject),
		StatusNotices: store,
		Audit:         sink,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSONWithReason(t *testing.T, url, reason, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if reason != "" {
		req.Header.Set("X-Reason", reason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// decodeNoticeList reads an enveloped StatusNoticesList response body
// ({"data":{"notices":[...],"count":N}}).
func decodeNoticeList(t *testing.T, resp *http.Response) v1.StatusNoticesList {
	t.Helper()
	var env struct {
		Data v1.StatusNoticesList `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode notice list: %v", err)
	}
	return env.Data
}

// TestStatusNoticeCreate_Happy pins create → audit row → the notice
// surfaces on the public active list.
func TestStatusNoticeCreate_Happy(t *testing.T) {
	store := newFakeStatusNoticeStore()
	sink := &recordingAuditSink{}
	ts := newStatusNoticeServer(t, operatorSubject(), store, sink)

	resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "planned maintenance",
		`{"title":"Scheduled maintenance","body":"02:00-03:00 UTC aggregator restart","severity":"maintenance"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var env struct {
		Data v1.StatusNotice `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Severity != "maintenance" || env.Data.Status != "active" || env.Data.ID == "" {
		t.Errorf("created notice = %+v", env.Data)
	}

	if len(sink.entries) != 1 || sink.entries[0].Action != "status_notice.create" ||
		sink.entries[0].TargetKind != "status_notice" {
		t.Errorf("audit entry = %+v", sink.entries)
	}

	// Public list shows it.
	pubResp, err := http.Get(ts.URL + "/v1/status/notices")
	if err != nil {
		t.Fatalf("GET notices: %v", err)
	}
	t.Cleanup(func() { pubResp.Body.Close() })
	list := decodeNoticeList(t, pubResp)
	if list.Count != 1 || len(list.Notices) != 1 {
		t.Fatalf("public list count = %d, want 1", list.Count)
	}
}

// TestStatusNoticeResolve_HidesFromPublic pins the lifecycle: a
// resolved notice drops off the public active list.
func TestStatusNoticeResolve_HidesFromPublic(t *testing.T) {
	store := newFakeStatusNoticeStore()
	sink := &recordingAuditSink{}
	ts := newStatusNoticeServer(t, operatorSubject(), store, sink)

	createResp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "incident",
		`{"title":"Degraded pricing","body":"CEX feed lag","severity":"major"}`)
	var env struct {
		Data v1.StatusNotice `json:"data"`
	}
	_ = json.NewDecoder(createResp.Body).Decode(&env)
	id := env.Data.ID

	resolveResp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices/"+id+"/resolve", "recovered", ``)
	if resolveResp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", resolveResp.StatusCode)
	}

	pubResp, _ := http.Get(ts.URL + "/v1/status/notices")
	t.Cleanup(func() { pubResp.Body.Close() })
	if list := decodeNoticeList(t, pubResp); list.Count != 0 {
		t.Errorf("public list count after resolve = %d, want 0", list.Count)
	}

	// Two audit rows: create + resolve.
	if len(sink.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(sink.entries))
	}
	if sink.entries[1].Action != "status_notice.resolve" {
		t.Errorf("second audit action = %q", sink.entries[1].Action)
	}
}

func TestStatusNoticeCreate_Validation(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"missing title", `{"body":"x","severity":"minor"}`},
		{"missing body", `{"title":"x","severity":"minor"}`},
		{"bad severity", `{"title":"x","body":"y","severity":"catastrophic"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStatusNoticeStore()
			ts := newStatusNoticeServer(t, operatorSubject(), store, nil)
			resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "x", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			if store.createCalls != 0 {
				t.Errorf("Create called on invalid input")
			}
		})
	}
}

func TestStatusNoticeCreate_MissingReason400(t *testing.T) {
	store := newFakeStatusNoticeStore()
	ts := newStatusNoticeServer(t, operatorSubject(), store, nil)
	resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "",
		`{"title":"x","body":"y","severity":"minor"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 without X-Reason", resp.StatusCode)
	}
	if store.createCalls != 0 {
		t.Errorf("Create called without reason")
	}
}

func TestStatusNoticeCreate_NonOperator403(t *testing.T) {
	store := newFakeStatusNoticeStore()
	ts := newStatusNoticeServer(t, auth.Subject{
		Identifier: "acct:customer", Tier: auth.TierAPIKey, KeyID: "kid_cust",
	}, store, nil)
	resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "x",
		`{"title":"x","body":"y","severity":"minor"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestStatusNotices_PublicUnwiredEmpty pins the nil-to-empty contract:
// no store wired → `{"notices":[],"count":0}`, not null, not 503.
func TestStatusNotices_PublicUnwiredEmpty(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/status/notices")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	list := decodeNoticeList(t, resp)
	if list.Notices == nil {
		t.Error("Notices is null; want []")
	}
	if list.Count != 0 {
		t.Errorf("Count = %d, want 0", list.Count)
	}
}

func TestStatusNoticeResolve_NotFound404(t *testing.T) {
	store := newFakeStatusNoticeStore()
	ts := newStatusNoticeServer(t, operatorSubject(), store, nil)
	resp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices/"+uuid.New().String()+"/resolve", "x", ``)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminStatusNoticesList_OperatorSeesResolved(t *testing.T) {
	store := newFakeStatusNoticeStore()
	sink := &recordingAuditSink{}
	ts := newStatusNoticeServer(t, operatorSubject(), store, sink)

	createResp := postJSONWithReason(t, ts.URL+"/v1/admin/status-notices", "i",
		`{"title":"t","body":"b","severity":"minor"}`)
	var env struct {
		Data v1.StatusNotice `json:"data"`
	}
	_ = json.NewDecoder(createResp.Body).Decode(&env)
	_ = postJSONWithReason(t, ts.URL+"/v1/admin/status-notices/"+env.Data.ID+"/resolve", "done", ``)

	// Admin list includes the resolved one; public list is empty.
	adminResp, err := http.Get(ts.URL + "/v1/admin/status-notices")
	if err != nil {
		t.Fatalf("admin GET: %v", err)
	}
	t.Cleanup(func() { adminResp.Body.Close() })
	// A GET with no Subject-attached operator credential would be 401;
	// operatorSubject() flows through fakeAuthMiddleware so this is 200.
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin list status = %d, want 200", adminResp.StatusCode)
	}
	if list := decodeNoticeList(t, adminResp); list.Count != 1 {
		t.Errorf("admin list count = %d, want 1 (incl resolved)", list.Count)
	}
}
