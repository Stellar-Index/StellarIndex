package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// prodCookieRig is newTestRig with the cookie attributes production
// actually runs (`/etc/stellarindex.toml`: cookie_secure = true,
// cookie_domain = ".stellarindex.io"). The default rig leaves both at
// their dev zero values, which would let a Domain/Secure regression on
// the hint cookie pass unnoticed.
func prodCookieRig(t *testing.T) *testRig {
	t.Helper()
	r := newTestRig(t)
	r.cfg.CookieDomain = ".stellarindex.io"
	r.cfg.CookieSecure = true
	return r
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

	// Same scope and same lifetime as the session cookie, or the pair
	// can disagree about whether a session exists.
	if hint.Domain != session.Domain {
		t.Errorf("hint Domain = %q, session Domain = %q", hint.Domain, session.Domain)
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

// The hint must be a bare presence flag. Anything derived from the
// user would be a non-HttpOnly leak of identity, and anything derived
// from the session token would be a second bearer credential readable
// by any script on the page.
func TestSessionHint_CarriesNoIdentityOrCredential(t *testing.T) {
	r := prodCookieRig(t)
	lw := r.postLogin(t, "secret-person@example.com")
	plaintext := r.extractTokenFromSentEmail(t)

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)

	hint := cookieNamed(w, SessionHintCookieName)
	if hint == nil {
		t.Fatal("session hint cookie not set")
	}
	if hint.Value != "1" {
		t.Errorf("hint value = %q, want the constant \"1\"", hint.Value)
	}

	session := cookieNamed(w, SessionCookieName)
	if session == nil {
		t.Fatal("session cookie not set")
	}
	user, err := r.users.GetUserByEmail(context.Background(), "secret-person@example.com")
	if err != nil {
		t.Fatalf("user not created: %v", err)
	}
	for _, leak := range []string{
		"secret-person@example.com",
		"secret-person",
		user.ID.String(),
		session.Value,
	} {
		if leak == "" {
			continue
		}
		if strings.Contains(hint.Value, leak) {
			t.Errorf("hint value %q leaks %q", hint.Value, leak)
		}
	}
}

// Logout clears both cookies in the same response. A hint that
// survived logout would send the explorer back for one 401 per page
// load until it expired on its own — the exact request this change
// exists to remove.
func TestHandleLogout_ClearsSessionHint(t *testing.T) {
	r := prodCookieRig(t)
	acct, err := r.accounts.Create(context.Background(), platform.Account{
		Name: "x", Slug: "x", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := r.users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID, Email: "owner@example.com", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, token := mintTestSession(t, r.users, platform.Session{
		UserID: user.ID, ExpiresAt: r.now().Add(24 * time.Hour),
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)

	hint := cookieNamed(w, SessionHintCookieName)
	if hint == nil {
		t.Fatal("logout did not emit a hint-clearing cookie")
	}
	if hint.MaxAge >= 0 {
		t.Errorf("logout did not expire the hint: MaxAge = %d", hint.MaxAge)
	}
	if hint.Value != "" {
		t.Errorf("cleared hint still carries a value: %q", hint.Value)
	}
	// Deleting a cookie requires the SAME Domain and Path it was set
	// with; a mismatch leaves the original in the browser. Compared
	// against the session-clearing cookie in this same response rather
	// than against the configured value, because the Set-Cookie parser
	// normalises the leading dot away on read-back.
	cleared := cookieNamed(w, SessionCookieName)
	if cleared == nil {
		t.Fatal("logout did not clear the session cookie")
	}
	if hint.Domain != cleared.Domain {
		t.Errorf("clear Domain = %q, session clear Domain = %q", hint.Domain, cleared.Domain)
	}
	if hint.Path != cleared.Path {
		t.Errorf("clear Path = %q, session clear Path = %q", hint.Path, cleared.Path)
	}
}

// Logout without any cookie is already idempotent for the session
// cookie; the hint must be cleared on that path too, because the two
// can be out of step (that is the whole stale-hint case).
func TestHandleLogout_ClearsSessionHintWithoutCookie(t *testing.T) {
	r := prodCookieRig(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	w := httptest.NewRecorder()
	r.h.HandleLogout(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	hint := cookieNamed(w, SessionHintCookieName)
	if hint == nil || hint.MaxAge >= 0 {
		t.Errorf("logout without a session did not expire the hint: %+v", hint)
	}
}
