package dashboardauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSignInAndLogout_WriteAuditRows: the privacy notice says sign-ins are kept in the audit log with address and
// user agent; the session row itself is deleted 90 days after expiry.
func TestSignInAndLogout_WriteAuditRows(t *testing.T) {
	rig := newPasskeyRig(t)
	sink := withAuditSink(rig)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-code", nil)
	req.Header.Set("User-Agent", "audit-test/1")
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user, SignInMethodEmailCode); err != nil {
		t.Fatalf("mintSession: %v", err)
	}
	e := onlyAuditEntry(t, sink)
	var meta map[string]any
	if err := json.Unmarshal(e.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if e.Action != AuditActionSessionSignIn || e.ActorUserID != rig.user.ID || e.AccountID != rig.account.ID ||
		e.TargetKind != "user" || e.TargetID != rig.user.ID.String() || meta["session_id"] == nil ||
		meta["method"] != SignInMethodEmailCode || e.UserAgent != "audit-test/1" {
		t.Fatalf("sign-in row = %+v meta=%v", e, meta)
	}

	signInSession := meta["session_id"]
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			cookie = c
		}
	}
	out := httptest.NewRequest(http.MethodPost, "/v1/auth/logout?all=true", nil)
	out.AddCookie(cookie)
	rec := httptest.NewRecorder()
	rig.h.HandleLogout(rec, out)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout status = %d", rec.Code)
	}
	rows := sink.all()
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want sign-in and logout", len(rows))
	}
	last := rows[1]
	meta = nil
	if err := json.Unmarshal(last.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if last.Action != AuditActionSessionRevoke || last.TargetID != rig.user.ID.String() ||
		meta["session_id"] != signInSession || meta["all_sessions"] != true {
		t.Fatalf("logout row = %+v meta=%v", last, meta)
	}

	// A logout with no live session revokes nothing and records nothing.
	rec = httptest.NewRecorder()
	rig.h.HandleLogout(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil))
	if rec.Code != http.StatusOK || len(sink.all()) != 2 {
		t.Fatalf("cookieless logout: status %d rows %d", rec.Code, len(sink.all()))
	}
}
