package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// prodCookieRig is newTestRig with the cookie attributes production
// actually runs (`/etc/stellarindex.toml`: cookie_secure = true,
// cookie_domain = ".stellarindex.io"). The default rig leaves both at
// their dev zero values, which would let a Domain/Secure regression on
// the hint cookie pass unnoticed.
func prodCookieRig(t *testing.T) *testRig {
	t.Helper()
	r := newTestRig(t)
	r.cfg.SessionHintDomain = ".stellarindex.io"
	r.cfg.CookieSecure = true
	return r
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
