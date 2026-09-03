package dashboardauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// C3-056 (audit-2026-07-23).
//
// `POST /v1/account/admin/lookup` hands a staff member another customer's
// billing email, tier, status and EVERY user's email + last-login. Pre-fix
// the only record that the access happened was one `Logger.Info` line,
// while every sibling admin surface (account override, key mint/revoke,
// status notice) wrote a durable platform.AuditEntry.
//
// A log line is not a privacy audit trail: it is rotated on journal
// retention, is not queryable per-account, and never reaches the audit
// view. These tests pin the durable row — its action, its target, and the
// actor identity — plus the failure counter that makes a LOST row visible.

// fakeAuditSink is a recording platform.AuditStore. `err` (when set) makes
// Append fail, which is the audit-store-blip path.
type fakeAuditSink struct {
	mu      sync.Mutex
	err     error
	entries []platform.AuditEntry
}

func (f *fakeAuditSink) Append(_ context.Context, e platform.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAuditSink) AppendBatch(ctx context.Context, entries []platform.AuditEntry) error {
	for _, e := range entries {
		if err := f.Append(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeAuditSink) List(_ context.Context, _ platform.AuditQuery) ([]platform.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platform.AuditEntry(nil), f.entries...), nil
}

func (f *fakeAuditSink) all() []platform.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]platform.AuditEntry(nil), f.entries...)
}

// adminLookupFixture seeds one customer account with two users plus a
// separate staff user, and returns a rig whose Audit sink is `sink`.
type adminLookupFixture struct {
	t        *testing.T
	rig      *testRig
	sink     *fakeAuditSink
	customer platform.Account
	staff    platform.User
	staffSC  SessionContext
}

func newAdminLookupFixture(t *testing.T, sink *fakeAuditSink) *adminLookupFixture {
	t.Helper()
	rig := newTestRig(t)
	rig.cfg.Audit = sink

	customer, err := rig.accounts.Create(context.Background(), platform.Account{
		Name:         "Acme Corp",
		Slug:         "acme",
		BillingEmail: "billing@acme.example",
		Tier:         platform.TierPro,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create customer account: %v", err)
	}
	for _, email := range []string{"ceo@acme.example", "dev@acme.example"} {
		if _, err := rig.users.CreateUser(context.Background(), platform.User{
			AccountID: customer.ID,
			Email:     email,
			Role:      platform.RoleOwner,
		}); err != nil {
			t.Fatalf("create customer user %s: %v", email, err)
		}
	}

	staffAcct, err := rig.accounts.Create(context.Background(), platform.Account{
		Name: "Stellar Index", Slug: "stellarindex",
		Tier: platform.TierEnterprise, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create staff account: %v", err)
	}
	staff, err := rig.users.CreateUser(context.Background(), platform.User{
		AccountID: staffAcct.ID,
		Email:     "ops@stellarindex.io",
		Role:      platform.RoleOwner,
		IsStaff:   true,
	})
	if err != nil {
		t.Fatalf("create staff user: %v", err)
	}
	sess, err := rig.users.CreateSession(context.Background(), platform.Session{UserID: staff.ID})
	if err != nil {
		t.Fatalf("create staff session: %v", err)
	}
	return &adminLookupFixture{
		t: t, rig: rig, sink: sink, customer: customer, staff: staff,
		staffSC: SessionContext{Session: sess, User: staff, Account: staffAcct},
	}
}

// lookup drives the handler as `sc` looking up the given dimension. `term` is
// written in the familiar "email=…" / "slug=…" shorthand, but it is marshalled
// into the request BODY and the URL is left bare — the look-up term is
// customer PII and must never appear in a URL (PRV F2, #346). Nothing here
// may put it back in the query string.
func (f *adminLookupFixture) lookup(sc SessionContext, term string) *httptest.ResponseRecorder {
	f.t.Helper()
	key, value, _ := strings.Cut(term, "=")
	body, err := json.Marshal(adminLookupRequest{
		Email: valueIf(key == "email", value),
		Slug:  valueIf(key == "slug", value),
	})
	if err != nil {
		f.t.Fatalf("marshal lookup body: %v", err)
	}
	return f.post(sc, "/v1/account/admin/lookup", body)
}

// post drives HandleAdminLookup with an explicit URI and body, so a test can
// assert on what the URI does (and does not) carry.
func (f *adminLookupFixture) post(sc SessionContext, uri string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, uri, bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.7:44321"
	req.Header.Set("User-Agent", "staff-console/1.0")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithSession(req.Context(), sc))
	rec := httptest.NewRecorder()
	f.rig.h.HandleAdminLookup(rec, req)
	return rec
}

func valueIf(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}

func auditWriteFailures(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(auditSurfaceStaffLookup))
}

// TestAdminLookup_WritesDurableAuditRow — the fix's core assertion: a
// successful staff PII read appends exactly one platform.AuditEntry naming
// the actor, the account they read, and how they found it.
func TestAdminLookup_WritesDurableAuditRow(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	rec := f.lookup(f.staffSC, "email=ceo@acme.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("audit rows appended = %d, want exactly 1 (the staff PII read must leave a durable trail)", len(entries))
	}
	got := entries[0]
	if got.Action != "staff.customer.lookup" {
		t.Errorf("Action = %q, want %q", got.Action, "staff.customer.lookup")
	}
	if got.ActorKind != platform.ActorStaff {
		t.Errorf("ActorKind = %q, want %q", got.ActorKind, platform.ActorStaff)
	}
	if got.ActorUserID != f.staff.ID {
		t.Errorf("ActorUserID = %s, want the staff user %s", got.ActorUserID, f.staff.ID)
	}
	if got.AccountID != f.customer.ID {
		t.Errorf("AccountID = %s, want the LOOKED-UP account %s", got.AccountID, f.customer.ID)
	}
	if got.TargetKind != "account" || got.TargetID != f.customer.ID.String() {
		t.Errorf("Target = %s/%s, want account/%s", got.TargetKind, got.TargetID, f.customer.ID)
	}
	if got.IP == nil || got.IP.String() != "198.51.100.7" {
		t.Errorf("IP = %v, want 198.51.100.7", got.IP)
	}
	if got.UserAgent != "staff-console/1.0" {
		t.Errorf("UserAgent = %q, want %q", got.UserAgent, "staff-console/1.0")
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp is zero — the row cannot be placed in time")
	}

	var meta map[string]any
	if err := json.Unmarshal(got.Metadata, &meta); err != nil {
		t.Fatalf("metadata is not JSON: %v (%s)", err, got.Metadata)
	}
	if meta["actor_email"] != "ops@stellarindex.io" {
		t.Errorf("metadata.actor_email = %v, want ops@stellarindex.io", meta["actor_email"])
	}
	if meta["query_kind"] != "email" {
		t.Errorf("metadata.query_kind = %v, want email", meta["query_kind"])
	}
	if meta["account_slug"] != "acme" {
		t.Errorf("metadata.account_slug = %v, want acme", meta["account_slug"])
	}
	// The count of user records disclosed — the size of the PII read.
	if got, want := meta["users_returned"], float64(2); got != want {
		t.Errorf("metadata.users_returned = %v, want %v", got, want)
	}
}

// TestAdminLookup_MasksStaffEmailInLogSink pins W6-prv-3: the staff
// actor's email must be masked before it reaches the app logger (which
// ships to Loki), consistent with the package's maskEmail policy — while
// the durable audit_log row keeps the FULL actor_email as the intended
// tamper-evident trail.
func TestAdminLookup_MasksStaffEmailInLogSink(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	var buf bytes.Buffer
	f.rig.cfg.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	rec := f.lookup(f.staffSC, "email=ceo@acme.example")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	logged := buf.String()
	// The log SINK must never carry the full staff email.
	if strings.Contains(logged, "ops@stellarindex.io") {
		t.Errorf("log sink leaked the full staff actor email (masking not applied):\n%s", logged)
	}
	// ...but it must still identify the actor in masked form.
	masked := maskEmail("ops@stellarindex.io")
	if !strings.Contains(logged, masked) {
		t.Errorf("expected masked staff email %q in the log sink; got:\n%s", masked, logged)
	}

	// The durable audit row keeps the FULL actor email — the trail is not
	// masked, only the log sink is.
	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(entries))
	}
	var meta map[string]any
	if err := json.Unmarshal(entries[0].Metadata, &meta); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if meta["actor_email"] != "ops@stellarindex.io" {
		t.Errorf("audit row actor_email = %v, want the FULL email (tamper-evident trail)", meta["actor_email"])
	}
}

// TestAdminLookup_AuditRecordsSlugQueryKind — a slug probe and an email
// probe are different investigative acts; the row must say which.
func TestAdminLookup_AuditRecordsSlugQueryKind(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	if rec := f.lookup(f.staffSC, "slug=acme"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	entries := sink.all()
	if len(entries) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(entries))
	}
	var meta map[string]any
	if err := json.Unmarshal(entries[0].Metadata, &meta); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if meta["query_kind"] != "slug" {
		t.Errorf("metadata.query_kind = %v, want slug", meta["query_kind"])
	}
}

// TestAdminLookup_NoAuditRowWhenAccessDenied — a 403'd non-staff caller saw
// nothing, so it must not manufacture a "staff read this customer" row.
func TestAdminLookup_NoAuditRowWhenAccessDenied(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	nonStaff, err := f.rig.users.CreateUser(context.Background(), platform.User{
		AccountID: f.customer.ID, Email: "outsider@example.com", Role: platform.RoleMember,
	})
	if err != nil {
		t.Fatalf("create non-staff user: %v", err)
	}
	sess, err := f.rig.users.CreateSession(context.Background(), platform.Session{UserID: nonStaff.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	rec := f.lookup(SessionContext{Session: sess, User: nonStaff, Account: f.customer}, "slug=acme")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("audit rows = %d, want 0 for a denied lookup", n)
	}
}

// TestAdminLookup_NoAuditRowWhenNotFound — a miss disclosed no PII.
func TestAdminLookup_NoAuditRowWhenNotFound(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	rec := f.lookup(f.staffSC, "slug=does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("audit rows = %d, want 0 when nothing matched", n)
	}
}

// TestAdminLookup_AuditFailureIsCountedNotSwallowed — the sink is down. The
// read already happened and cannot be un-done, so the response stays 200 —
// but the lost row must show up on
// stellarindex_admin_audit_write_failures_total{surface="staff_customer_lookup"},
// or an unrecorded PII access is indistinguishable from no access at all.
func TestAdminLookup_AuditFailureIsCountedNotSwallowed(t *testing.T) {
	sink := &fakeAuditSink{err: errors.New("simulated audit-db blip")}
	f := newAdminLookupFixture(t, sink)

	before := auditWriteFailures(t)
	rec := f.lookup(f.staffSC, "slug=acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an audit-sink blip must not break the staff tool)", rec.Code)
	}
	if got, want := auditWriteFailures(t), before+1; got != want {
		t.Errorf("admin_audit_write_failures_total{surface=%q} = %v, want %v",
			auditSurfaceStaffLookup, got, want)
	}
}

// TestAdminLookup_AuditFailureNotCountedOnSuccess — the counter must be
// silent on the healthy path or the alert built on it is permanently firing.
func TestAdminLookup_AuditFailureNotCountedOnSuccess(t *testing.T) {
	sink := &fakeAuditSink{}
	f := newAdminLookupFixture(t, sink)

	before := auditWriteFailures(t)
	if rec := f.lookup(f.staffSC, "slug=acme"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := auditWriteFailures(t); got != before {
		t.Errorf("counter moved on the healthy path: %v → %v", before, got)
	}
}

// TestAdminLookup_NilAuditSinkStillServes — Postgres-less deployments leave
// Config.Audit nil; the handler must degrade, not panic.
func TestAdminLookup_NilAuditSinkStillServes(t *testing.T) {
	f := newAdminLookupFixture(t, &fakeAuditSink{})
	f.rig.cfg.Audit = nil

	rec := f.lookup(f.staffSC, "slug=acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a nil audit sink", rec.Code)
	}
	var resp AdminLookupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.Account.ID != f.customer.ID.String() {
		t.Errorf("account id = %q, want %q", resp.Account.ID, f.customer.ID)
	}
}

// PRV F2 (#346) — the look-up term must not be reachable via the URL.
//
// The pre-fix handler read `r.URL.Query()`, so a staff member's browser
// history, the edge proxy, every intermediate CDN and the Referer header on
// the next navigation each recorded a real customer's email address verbatim.
// The 7843f129 Caddy filter masks that in OUR access log only; it cannot
// reach a browser's history or a third party's log. The address therefore has
// to stop travelling in the URL at all.
//
// This pins the corrected behaviour from both sides, which is what makes it
// non-vacuous: the query string must NOT resolve an account (it is no longer
// an input channel), and the body MUST resolve the same one.
func TestAdminLookup_QueryStringIsNotAnInputChannel(t *testing.T) {
	f := newAdminLookupFixture(t, &fakeAuditSink{})

	// The email in the URL and nowhere else. Pre-fix this returned 200 plus
	// the customer's account; it must now be refused outright.
	rec := f.post(f.staffSC, "/v1/account/admin/lookup?email=ceo@acme.example", []byte(`{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("query-string email: status = %d, want 400 — the query string must not resolve an account; body = %s",
			rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, f.customer.ID.String()) ||
		strings.Contains(body, "billing@acme.example") {
		t.Errorf("query-string email leaked customer data into the response: %s", body)
	}

	// The same term in the BODY resolves the very same account, so this
	// cannot pass merely by the handler being broken.
	rec = f.post(f.staffSC, "/v1/account/admin/lookup", []byte(`{"email":"ceo@acme.example"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("body email: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp AdminLookupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Account.Slug != "acme" {
		t.Errorf("Account.Slug = %q, want %q — the body must be the input channel", resp.Account.Slug, "acme")
	}
	if len(resp.Users) != 2 {
		t.Errorf("Users = %d, want 2", len(resp.Users))
	}
}

// The route must be mounted as a POST. A GET mounting is what puts the
// address in the URL in the first place, so the verb is part of the fix and a
// future edit that restores `GET` has to fail here.
func TestAdminLookup_MountedAsPostOnly(t *testing.T) {
	rig := newTestRig(t)
	mux := http.NewServeMux()
	rig.h.Mount(mux)

	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost,
		"/v1/account/admin/lookup", nil)); pattern != "POST /v1/account/admin/lookup" {
		t.Errorf("POST pattern = %q, want %q", pattern, "POST /v1/account/admin/lookup")
	}

	// GET must reach Go's 405 handler, not a registered one.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/account/admin/lookup", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405 — a GET mounting would put the email back in the URL", rec.Code)
	}
}
