package dashboardauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

type fakeEraser struct {
	calls []uuid.UUID
	actor platform.ActorKind
	err   error
}

func (f *fakeEraser) Erase(_ context.Context, id uuid.UUID, actor platform.ActorKind) (accounterasure.Report, error) {
	f.calls, f.actor = append(f.calls, id), actor
	if f.err != nil {
		return accounterasure.Report{}, f.err
	}
	return accounterasure.Report{Plan: postgresstore.ErasurePlan{AccountID: id, OwnerEmails: []string{"owner@acme.example"}}}, nil
}

type fakeExporter struct{ requester uuid.UUID }

func (f *fakeExporter) Export(_ context.Context, accountID, requester uuid.UUID, now time.Time) (platform.AccountExport, error) {
	f.requester = requester
	return platform.AccountExport{
		SchemaVersion: platform.AccountExportSchemaVersion, GeneratedAt: now,
		Account:     platform.ExportAccount{ID: accountID.String(), Slug: "acme", BillingEmail: "owner@acme.example"},
		Users:       []platform.ExportUser{{Email: "owner@acme.example", IsRequester: true}},
		PriceAlerts: []platform.ExportPriceAlert{{Threshold: "0.000000000000000001"}},
	}, nil
}

type accountRig struct {
	*testRig
	eraser   *fakeEraser
	exporter *fakeExporter
	sink     *fakeAuditSink
	sc       SessionContext
}

func newAccountRig(t *testing.T) *accountRig {
	t.Helper()
	r := newTestRig(t)
	ar := &accountRig{testRig: r, eraser: &fakeEraser{}, exporter: &fakeExporter{}, sink: &fakeAuditSink{}}
	r.cfg.AccountEraser, r.cfg.AccountExporter, r.cfg.Audit = ar.eraser, ar.exporter, ar.sink
	ar.sc = SessionContext{
		Session: platform.Session{ID: uuid.New(), CreatedAt: r.now().Add(-time.Minute)},
		User:    platform.User{ID: uuid.New(), Role: platform.RoleOwner, Email: "owner@acme.example"},
		Account: platform.Account{ID: uuid.New(), Slug: "acme", Status: platform.AccountActive},
	}
	ar.sc.User.AccountID = ar.sc.Account.ID
	return ar
}

// serve drives the MOUNTED route, with sc attached as the session
// middleware would attach it (nil = no session).
func (ar *accountRig) serve(method, path, body, origin string, sc *SessionContext) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	ar.h.Mount(mux, middleware.NewPublicRoutes())
	req := httptest.NewRequest(method, "http://api.example"+path, strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if sc != nil {
		req = req.WithContext(WithSession(req.Context(), *sc))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func (ar *accountRig) erase(body string, sc *SessionContext) *httptest.ResponseRecorder {
	return ar.serve(http.MethodDelete, accountRoute, body, "http://api.example", sc)
}

func problemType(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var p map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	s, _ := p["type"].(string)
	return s
}

func TestAccountDelete_ErasesAndSignsOut(t *testing.T) {
	ar := newAccountRig(t)
	w := ar.erase(`{"confirm":"acme"}`, &ar.sc)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s, want 204", w.Code, w.Body)
	}
	if len(ar.eraser.calls) != 1 || ar.eraser.calls[0] != ar.sc.Account.ID || ar.eraser.actor != platform.ActorUser {
		t.Errorf("eraser calls = %v actor %q, want the session account as a user action", ar.eraser.calls, ar.eraser.actor)
	}
	cleared := map[string]bool{}
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	if !cleared[SessionCookieName] || !cleared[SessionHintCookieName] {
		t.Errorf("cookies cleared = %v, want the session cookie and its hint", cleared)
	}
	msg, ok := ar.sender.Last()
	if !ok || msg.To[0] != "owner@acme.example" || msg.Tags["template"] != "account-erased" {
		t.Errorf("confirmation mail = %+v (sent=%v), want account-erased to the owner", msg, ok)
	}
	if strings.Contains(msg.Text+msg.HTML, "acme") {
		t.Error("confirmation mail names the erased slug")
	}
}

func TestAccountDelete_Refusals(t *testing.T) {
	ar := newAccountRig(t)
	member := ar.sc
	member.User.Role = platform.RoleAdmin
	stale := ar.sc
	stale.Session.CreatedAt = ar.now().Add(-accountReauthWindow - time.Second)

	for _, tc := range []struct {
		name     string
		body     string
		origin   string
		sc       *SessionContext
		want     int
		wantType string
	}{
		{"no session (API key or none)", `{"confirm":"acme"}`, "http://api.example", nil, http.StatusUnauthorized, ""},
		{"not owner", `{"confirm":"acme"}`, "http://api.example", &member, http.StatusForbidden, ""},
		{
			"stale session", `{"confirm":"acme"}`, "http://api.example", &stale, http.StatusUnauthorized,
			"https://api.stellarindex.io/errors/reauth-required",
		},
		{"wrong slug", `{"confirm":"acme2"}`, "http://api.example", &ar.sc, http.StatusBadRequest, ""},
		{"no body", ``, "http://api.example", &ar.sc, http.StatusBadRequest, ""},
		{
			"cross-site", `{"confirm":"acme"}`, "https://evil.example", &ar.sc, http.StatusForbidden,
			"https://api.stellarindex.io/errors/cross-site-request-blocked",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ar.serve(http.MethodDelete, accountRoute, tc.body, tc.origin, tc.sc)
			if w.Code != tc.want {
				t.Fatalf("status = %d %s, want %d", w.Code, w.Body, tc.want)
			}
			if tc.wantType != "" && problemType(t, w) != tc.wantType {
				t.Errorf("type = %q, want %q", problemType(t, w), tc.wantType)
			}
		})
	}
	if len(ar.eraser.calls) != 0 {
		t.Errorf("a refused request reached the eraser %d time(s)", len(ar.eraser.calls))
	}
}

func TestAccountDelete_BlockedIs409AndFailureIs500(t *testing.T) {
	ar := newAccountRig(t)
	ar.eraser.err = fmt.Errorf("%w: billing", accounterasure.ErrBlocked)
	if w := ar.erase(`{"confirm":"acme"}`, &ar.sc); w.Code != http.StatusConflict {
		t.Errorf("blocked: status = %d, want 409", w.Code)
	}
	ar.eraser.err = errors.New("db down")
	w := ar.erase(`{"confirm":"acme"}`, &ar.sc)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("failure: status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "db down") {
		t.Error("internal error leaked into the response")
	}
	if _, sent := ar.sender.Last(); sent {
		t.Error("a failed erasure sent the confirmation mail")
	}
}

func TestAccountDelete_RateLimited(t *testing.T) {
	ar := newAccountRig(t)
	for i := 0; i < maxAccountErasures; i++ {
		if w := ar.erase(`{"confirm":"wrong"}`, &ar.sc); w.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: status = %d, want 400", i+1, w.Code)
		}
	}
	w := ar.erase(`{"confirm":"acme"}`, &ar.sc)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("status = %d Retry-After=%q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestAccountExport_ServesAnAttachmentAndAuditsCountsOnly(t *testing.T) {
	ar := newAccountRig(t)
	w := ar.serve(http.MethodGet, accountExportRoute, "", "", &ar.sc)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", w.Code, w.Body)
	}
	if ar.exporter.requester != ar.sc.User.ID {
		t.Errorf("export built for %s, want the session user", ar.exporter.requester)
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("headers = %v, want no-store attachment", w.Header())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !strings.Contains(w.Body.String(), `"threshold":"0.000000000000000001"`) {
		t.Error("threshold is not served as a decimal string")
	}
	rows := ar.sink.all()
	if len(rows) != 1 || rows[0].Action != AuditActionAccountExport || rows[0].AccountID != ar.sc.Account.ID {
		t.Fatalf("audit rows = %+v, want one account.export", rows)
	}
	if meta := string(rows[0].Metadata); strings.Contains(meta, "acme") || strings.Contains(meta, "@") {
		t.Errorf("account.export metadata carries PII: %s", meta)
	}
}

func TestAccountExport_Refusals(t *testing.T) {
	ar := newAccountRig(t)
	member := ar.sc
	member.User.Role = platform.RoleMember
	stale := ar.sc
	stale.Session.CreatedAt = ar.now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		sc   *SessionContext
		want int
	}{
		"no session": {nil, http.StatusUnauthorized},
		"not owner":  {&member, http.StatusForbidden},
		"stale":      {&stale, http.StatusUnauthorized},
	} {
		if w := ar.serve(http.MethodGet, accountExportRoute, "", "", tc.sc); w.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", name, w.Code, tc.want)
		}
	}
	for i := 0; i < maxAccountExports; i++ {
		_ = ar.serve(http.MethodGet, accountExportRoute, "", "", &ar.sc)
	}
	if w := ar.serve(http.MethodGet, accountExportRoute, "", "", &ar.sc); w.Code != http.StatusTooManyRequests {
		t.Errorf("export %d: status = %d, want 429", maxAccountExports+1, w.Code)
	}
}

// TestAccountRoutes_UnmountedWithoutBackends — a deployment that wires no
// eraser or exporter serves neither route.
func TestAccountRoutes_UnmountedWithoutBackends(t *testing.T) {
	r := newTestRig(t)
	ar := &accountRig{testRig: r}
	ar.sc = SessionContext{User: platform.User{Role: platform.RoleOwner}, Session: platform.Session{CreatedAt: r.now()}}
	if w := ar.erase(`{"confirm":""}`, &ar.sc); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Errorf("erase without an eraser: status = %d, want unmounted", w.Code)
	}
}
