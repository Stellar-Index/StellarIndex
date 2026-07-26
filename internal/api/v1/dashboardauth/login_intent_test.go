package dashboardauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// cookieNamed pulls one Set-Cookie by name off a recorder, or nil.
func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// callbackFor builds the GET the dashboard's /auth/callback page
// issues for a magic-link plaintext.
func callbackFor(plaintext string) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	req.RemoteAddr = "203.0.113.5:55123"
	return req
}

// TestHandleCallback_LoginCSRF_AttackerLinkInVictimBrowserRejected is
// the C3-030 regression: the attacker requests a magic link for their
// OWN account and mails that link to the victim. Before the
// login-intent binding the victim's browser followed it, took the
// attacker's session cookie, and every later action the victim
// performed landed in the attacker's dashboard.
//
// The victim's browser must not be signed in, and — because the check
// runs before consumption — the attacker's own token must survive so
// this can never be used to burn someone's link.
func TestHandleCallback_LoginCSRF_AttackerLinkInVictimBrowserRejected(t *testing.T) {
	r := newTestRig(t)

	// Attacker's browser requests a link for the attacker's own email.
	attackerLogin := r.postLogin(t, "attacker@evil.example")
	if attackerLogin.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", attackerLogin.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	// Victim's browser follows the mailed link. It never asked for a
	// link, so it holds no login-intent witness.
	victim := callbackFor(attackerToken)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, victim)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (login CSRF: victim signed into the attacker's account)", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil {
		t.Fatalf("session cookie minted for the victim's browser: %q", c.Value)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect into the dashboard", loc)
	}

	// The rejection must not have spent the token: the attacker's own
	// browser can still complete its own login.
	own := callbackFor(attackerToken)
	attachCookies(own, attackerLogin)
	w2 := httptest.NewRecorder()
	r.h.HandleCallback(w2, own)
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("originating browser status = %d, want 303 (binding checked before consumption)", w2.Code)
	}
	if c := cookieNamed(w2, SessionCookieName); c == nil {
		t.Error("originating browser got no session cookie")
	}
}

// TestHandleCallback_LoginIntentIsPerToken proves the binding is
// per-token, not merely "this browser has logged in at some point".
// The victim here is mid-login with a link of their own, so a cookie
// IS present — a naive presence check would pass the attacker's link.
func TestHandleCallback_LoginIntentIsPerToken(t *testing.T) {
	r := newTestRig(t)

	attackerLogin := r.postLogin(t, "attacker@evil.example")
	if attackerLogin.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", attackerLogin.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	victimLogin := r.postLogin(t, "victim@example.com")
	if victimLogin.Code != http.StatusOK {
		t.Fatalf("victim login: %d", victimLogin.Code)
	}
	if cookieNamed(victimLogin, LoginIntentCookieName) == nil {
		t.Fatal("victim browser holds no login-intent cookie; test would be vacuous")
	}

	req := callbackFor(attackerToken)
	attachCookies(req, victimLogin) // victim's own witness, attacker's token
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (witness must bind the specific token)", w.Code)
	}
	if c := cookieNamed(w, SessionCookieName); c != nil {
		t.Fatalf("session cookie minted from another browser's token: %q", c.Value)
	}
}

// TestHandleLogin_SetsLoginIntentCookieForTheMintedToken pins the
// witness's shape: it must be the digest of the token just minted,
// HttpOnly, and expire with the link it witnesses.
func TestHandleLogin_SetsLoginIntentCookieForTheMintedToken(t *testing.T) {
	r := newTestRig(t)
	w := r.postLogin(t, "alice@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d", w.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	c := cookieNamed(w, LoginIntentCookieName)
	if c == nil {
		t.Fatal("no login-intent cookie set by /v1/auth/login")
	}
	want := loginIntentDigest(HashMagicLinkPlaintext(plaintext))
	if c.Value != want {
		t.Errorf("cookie value = %q, want the emailed token's digest %q", c.Value, want)
	}
	if !c.HttpOnly {
		t.Error("login-intent cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if got, wantAge := c.MaxAge, int(r.cfg.MagicLinkTTL/time.Second); got != wantAge {
		t.Errorf("MaxAge = %d, want %d (the link's own TTL)", got, wantAge)
	}
}

// TestHandleLogin_KeepsPriorIntentSoAnEarlierLinkStillWorks — a user
// who taps "email me a link" twice holds two live tokens and may click
// either. A single-slot witness would break the older link.
func TestHandleLogin_KeepsPriorIntentSoAnEarlierLinkStillWorks(t *testing.T) {
	r := newTestRig(t)

	first := r.postLogin(t, "alice@example.com")
	if first.Code != http.StatusOK {
		t.Fatalf("first login: %d", first.Code)
	}
	firstToken := r.extractTokenFromSentEmail(t)

	// Second request from the SAME browser: it replays the cookie it
	// already holds, exactly as a browser would.
	body, _ := json.Marshal(loginRequest{Email: "alice@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.5:55123"
	attachCookies(req, first)
	second := httptest.NewRecorder()
	r.h.HandleLogin(second, req)
	if second.Code != http.StatusOK {
		t.Fatalf("second login: %d", second.Code)
	}
	secondToken := r.extractTokenFromSentEmail(t)
	if secondToken == firstToken {
		t.Fatal("second login re-issued the same token; test would be vacuous")
	}

	// Clicking the OLDER link must still work.
	cb := callbackFor(firstToken)
	attachCookies(cb, second)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("older link status = %d, want 303", w.Code)
	}
}

// TestHandleCallback_ClearsLoginIntentOnSuccess — a spent witness
// shouldn't linger in the browser for the rest of the link's TTL.
func TestHandleCallback_ClearsLoginIntentOnSuccess(t *testing.T) {
	r := newTestRig(t)
	lw := r.postLogin(t, "alice@example.com")
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	plaintext := r.extractTokenFromSentEmail(t)

	cb := callbackFor(plaintext)
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	c := cookieNamed(w, LoginIntentCookieName)
	if c == nil {
		t.Fatal("login-intent cookie not cleared after a successful sign-in")
	}
	if c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("login-intent cookie not expired: value=%q MaxAge=%d", c.Value, c.MaxAge)
	}
}

// TestHandleLogin_ThrottledResponseStillCarriesAnIntentCookie — the
// login-intent cookie must not become a side channel for the
// magic-link throttle. [LoginThrottle]'s contract is that a throttled
// send is byte-indistinguishable from a real one; if only the
// un-throttled path emitted a Set-Cookie, the header's presence would
// answer "does a throttle currently apply to this address?" for free.
func TestHandleLogin_ThrottledResponseStillCarriesAnIntentCookie(t *testing.T) {
	sent := newTestRig(t)
	sentW := sent.postLogin(t, "alice@example.com")
	sentCookie := cookieNamed(sentW, LoginIntentCookieName)
	if sentCookie == nil {
		t.Fatal("un-throttled login set no login-intent cookie; test would be vacuous")
	}

	throttled := newTestRig(t)
	throttled.cfg.LoginThrottle = &stubLoginThrottle{allow: false}
	throttledW := throttled.postLogin(t, "victim@example.com")

	if throttled.sender.SentCount() != 0 {
		t.Fatalf("throttled send count = %d, want 0", throttled.sender.SentCount())
	}
	throttledCookie := cookieNamed(throttledW, LoginIntentCookieName)
	if throttledCookie == nil {
		t.Fatal("throttled login set no login-intent cookie — the header's absence " +
			"tells an attacker a throttle fired for this address")
	}
	if !isLoginIntentDigest(throttledCookie.Value) {
		t.Errorf("throttled cookie value = %q, want a digest of the same shape as the real one (%q)",
			throttledCookie.Value, sentCookie.Value)
	}
	if throttledCookie.MaxAge != sentCookie.MaxAge ||
		throttledCookie.HttpOnly != sentCookie.HttpOnly ||
		throttledCookie.SameSite != sentCookie.SameSite {
		t.Errorf("throttled cookie attributes differ from the real one: %+v vs %+v",
			throttledCookie, sentCookie)
	}
	// And the decoy must be inert: it witnesses a token no store saw.
	if _, err := throttled.tokens.ConsumeMagicLinkToken(t.Context(), []byte("unused")); err == nil {
		t.Error("expected the throttled path to have persisted no token")
	}
}

// TestLoginIntentDigest_DomainSeparated — the witness must not be the
// token hash itself, or a leaked cookie would hand over a credential-
// equivalent value.
func TestLoginIntentDigest_DomainSeparated(t *testing.T) {
	hash := HashMagicLinkPlaintext("abc123")
	d := loginIntentDigest(hash)
	if d == string(hash) || d == hexOf(hash) {
		t.Fatalf("digest equals the token hash: %q", d)
	}
	if !isLoginIntentDigest(d) {
		t.Fatalf("digest %q is not the shape the cookie parser accepts", d)
	}
	if loginIntentDigest(HashMagicLinkPlaintext("abc124")) == d {
		t.Fatal("digest collides across distinct tokens")
	}
}

func hexOf(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
	}
	return string(out)
}
