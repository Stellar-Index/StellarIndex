package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestMintSession_RevokesTheSessionItReplaces pins that logging in again
// is the victim's instinctive remedy for a stolen cookie, so the session the
// browser presents at login must be revoked, not left alive beside the new
// one. Both login paths (magic link / code and passkey) go through
// mintSession.
func TestMintSession_RevokesTheSessionItReplaces(t *testing.T) {
	rig := newPasskeyRig(t)
	const oldToken = "old-session-cookie-value"
	old, err := rig.users.CreateSession(context.Background(), platform.Session{
		UserID:    rig.user.ID,
		TokenHash: HashSessionToken(oldToken),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-code", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: oldToken})
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}

	var minted string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			minted = c.Value
		}
	}
	if minted == "" || minted == oldToken {
		t.Fatalf("new session cookie = %q, want a fresh token", minted)
	}

	rig.users.mu.Lock()
	revokedAt := rig.users.sessions[old.ID].RevokedAt
	rig.users.mu.Unlock()
	if revokedAt.IsZero() {
		t.Error("the session presented at login is still live after a fresh login; " +
			"a stolen copy of that cookie survives the victim logging in again")
	}
}

// TestMintSession_CapsLiveSessionsPerUser pins that a login beyond the
// per-user cap revokes the oldest live session, so the row count stays
// bounded however often a door mints.
func TestMintSession_CapsLiveSessionsPerUser(t *testing.T) {
	rig := newPasskeyRig(t)
	base := time.Now().Add(-time.Hour)
	var oldest platform.Session
	for i := range maxLiveSessionsPerUser {
		s, err := rig.users.CreateSession(context.Background(), platform.Session{
			UserID:    rig.user.ID,
			TokenHash: HashSessionToken("prior-" + strconv.Itoa(i)),
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		rig.users.mu.Lock()
		s.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		rig.users.sessions[s.ID] = s
		rig.users.mu.Unlock()
		if i == 0 {
			oldest = s
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
	req.RemoteAddr = "203.0.113.10:44100"
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}
	var minted string
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			minted = c.Value
		}
	}
	if _, err := rig.users.GetSessionByTokenHash(context.Background(), HashSessionToken(minted)); err != nil {
		t.Fatalf("freshly minted session must be live: %v", err)
	}

	rig.users.mu.Lock()
	defer rig.users.mu.Unlock()
	live := 0
	for _, s := range rig.users.sessions {
		if s.UserID == rig.user.ID && s.RevokedAt.IsZero() {
			live++
		}
	}
	if live != maxLiveSessionsPerUser {
		t.Errorf("live sessions = %d, want %d", live, maxLiveSessionsPerUser)
	}
	if rig.users.sessions[oldest.ID].RevokedAt.IsZero() {
		t.Error("oldest session still live after a login beyond the cap")
	}
}

// A session issued through the magic-link door must set the JS-readable
// presence flag beside the HttpOnly session cookie, scoped identically
// so the explorer origin can see it and so the two expire together.
func TestMintSession_SetsSessionHintBesideSessionCookie(t *testing.T) {
	r := prodCookieRig(t)
	lw := r.postLogin(t, "hint@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", w.Code)
	}

	session := cookieNamed(w, SessionCookieName)
	if session == nil {
		t.Fatal("session cookie not set")
	}
	hint := cookieNamed(w, SessionHintCookieName)
	if hint == nil {
		t.Fatal("session hint cookie not set beside the session cookie")
	}

	// The session cookie is the bearer credential and stays HttpOnly.
	if !session.HttpOnly {
		t.Error("session cookie lost HttpOnly")
	}
	// The hint exists only to be read by document.cookie.
	if hint.HttpOnly {
		t.Error("hint cookie is HttpOnly — the explorer cannot read it, so the probe can never be skipped")
	}

	// The hint carries the parent domain so the explorer's origin can
	// read it; the credential it shadows stays host-only.
	if hint.Domain != "stellarindex.io" {
		t.Errorf("hint Domain = %q, want the configured parent domain", hint.Domain)
	}
	if session.Domain != "" {
		t.Errorf("session Domain = %q, want host-only", session.Domain)
	}
	if hint.Path != session.Path {
		t.Errorf("hint Path = %q, session Path = %q", hint.Path, session.Path)
	}
	if hint.Secure != session.Secure {
		t.Errorf("hint Secure = %v, session Secure = %v", hint.Secure, session.Secure)
	}
	if hint.SameSite != session.SameSite {
		t.Errorf("hint SameSite = %v, session SameSite = %v", hint.SameSite, session.SameSite)
	}
	if !hint.Expires.Equal(session.Expires) {
		t.Errorf("hint Expires = %v, session Expires = %v", hint.Expires, session.Expires)
	}
}

// TestMintSession_SetsSameCookieAsEmailFlow pins that the passkey
// login's terminal step issues the identical credential the email
// flows do: a DB session row + the stellarindex_session cookie with
// the same attributes.
func TestMintSession_SetsSameCookieAsEmailFlow(t *testing.T) {
	rig := newPasskeyRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/finish-login", nil)
	req.RemoteAddr = "203.0.113.10:44100"
	w := httptest.NewRecorder()
	if err := rig.h.mintSession(w, req, rig.user); err != nil {
		t.Fatalf("mintSession: %v", err)
	}

	var got *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName {
			got = c
		}
	}
	if got == nil || got.Value == "" {
		t.Fatal("no session cookie set")
	}
	if !got.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	// W1-auth-passkey-2: the cookie carries a random token, NOT the
	// session PK, and the row is found by sha256(token). Looking the
	// session up by the cookie value hashed is how resolveSession does
	// it; a read of the row yields only the hash, never a replayable id.
	sess, err := rig.users.GetSessionByTokenHash(context.Background(), HashSessionToken(got.Value))
	if err != nil {
		t.Fatalf("session row not created / not resolvable by token hash: %v", err)
	}
	// The cookie value must NOT itself be the session id — that was the
	// unhashed-bearer defect. Even if it happened to parse as a UUID, it
	// must not equal the stored PK.
	if got.Value == sess.ID.String() {
		t.Fatal("cookie value equals the session PK — the raw-id bearer defect (W1-auth-passkey-2) is back")
	}
	if sess.UserID != rig.user.ID {
		t.Fatalf("session user = %s, want %s", sess.UserID, rig.user.ID)
	}
	wantExpiry := rig.now().Add(rig.h.cfg.SessionTTL)
	if !sess.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("session expiry = %v, want %v", sess.ExpiresAt, wantExpiry)
	}
}
