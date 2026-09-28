package dashboardauth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// assertHostOnlyCredential pins the attributes a browser requires
// before it will store a __Host- cookie: no Domain, Path=/, Secure.
func assertHostOnlyCredential(t *testing.T, where string, c *http.Cookie) {
	t.Helper()
	if !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("%s: credential cookie %q lacks the __Host- prefix", where, c.Name)
	}
	if c.Domain != "" {
		t.Errorf("%s: credential cookie %q carries Domain=%q, want host-only", where, c.Name, c.Domain)
	}
	if c.Path != "/" {
		t.Errorf("%s: credential cookie %q Path = %q, want /", where, c.Name, c.Path)
	}
	if !c.Secure {
		t.Errorf("%s: credential cookie %q is not Secure", where, c.Name)
	}
	if !c.HttpOnly {
		t.Errorf("%s: credential cookie %q is not HttpOnly", where, c.Name)
	}
}

// checkCookieScopes asserts every cookie in a response is the
// JS-readable hint (which alone may carry the configured parent domain),
// the login-device marker (Path-scoped, not a credential — see
// [LoginDeviceCookieName]), or a host-only credential cookie, and
// records the names it saw.
func checkCookieScopes(t *testing.T, where string, w *httptest.ResponseRecorder, seen map[string]bool) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		seen[c.Name] = true
		if c.Name == SessionHintCookieName {
			if c.Domain != "stellarindex.io" {
				t.Errorf("%s: hint Domain = %q, want the configured parent domain", where, c.Domain)
			}
			continue
		}
		if c.Name == LoginDeviceCookieName {
			continue
		}
		assertHostOnlyCredential(t, where, c)
	}
}

// Every credential cookie the dashboard emits — session, login intent,
// passkey ceremony, on both set and clear — stays host-only even when a
// parent cookie domain is configured for the presence hint.
func TestCredentialCookies_HostOnlyEvenWithHintDomain(t *testing.T) {
	seen := map[string]bool{}

	r := prodCookieRig(t)
	lw := r.postLogin(t, "scope@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	checkCookieScopes(t, "login", lw, seen)

	plaintext := r.extractTokenFromSentEmail(t)
	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	cw := httptest.NewRecorder()
	r.h.HandleCallback(cw, cb)
	if cw.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", cw.Code)
	}
	checkCookieScopes(t, "callback", cw, seen)

	lo := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	attachCookies(lo, cw)
	low := httptest.NewRecorder()
	r.h.HandleLogout(low, lo)
	checkCookieScopes(t, "logout", low, seen)

	pk := newPasskeyRig(t)
	pw := httptest.NewRecorder()
	pk.h.HandlePasskeyBeginLogin(pw, httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-login", nil))
	if pw.Code != http.StatusOK {
		t.Fatalf("passkey begin: %d (%s)", pw.Code, pw.Body.String())
	}
	checkCookieScopes(t, "passkey begin", pw, seen)
	cw2 := httptest.NewRecorder()
	pk.h.clearPasskeyCeremonyCookie(cw2)
	checkCookieScopes(t, "passkey clear", cw2, seen)

	for _, name := range []string{SessionCookieName, LoginIntentCookieName, PasskeyCeremonyCookieName, SessionHintCookieName} {
		if !seen[name] {
			t.Errorf("flows never emitted %q — the scope check above did not cover it", name)
		}
	}
}

// The login-intent witness must be keyed: a digest anyone can compute
// from a token they hold would let a cookie planted by a third party
// satisfy the browser binding.
func TestHandleCallback_RefusesUnkeyedLoginIntent(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "binding@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	sum := sha256.New()
	sum.Write([]byte("stellarindex/login-intent/v1|"))
	sum.Write(HashMagicLinkPlaintext(plaintext))
	unkeyed := hex.EncodeToString(sum.Sum(nil))

	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	cb.AddCookie(&http.Cookie{Name: LoginIntentCookieName, Value: unkeyed})
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusForbidden {
		t.Fatalf("callback with an unkeyed intent digest: status = %d, want 403", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil && c.Value != "" {
		t.Fatal("session minted from an unkeyed intent digest")
	}

	// Positive control: the cookie the server itself set still binds.
	ok := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	ok.RemoteAddr = "203.0.113.5:55123"
	attachCookies(ok, lw)
	w2 := httptest.NewRecorder()
	r.h.HandleCallback(w2, ok)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("callback with the server-set intent: status = %d, want 303", w2.Code)
	}
}
