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

// The wire name is pinned as a literal: renaming it silently drops every
// browser's proof.
const loginDeviceCookieWireName = "stellarindex_login_device"

// signIn completes a code sign-in for email and returns the cookies the
// browser keeps from it.
func (lr *lockoutRig) signIn(t *testing.T, email string) []*http.Cookie {
	t.Helper()
	w := lr.postVerifyCode(t, email, lr.loginAndCode(t, email))
	if w.Code != http.StatusOK {
		t.Fatalf("sign-in for %s: status = %d, want 200", email, w.Code)
	}
	return w.Result().Cookies()
}

func (lr *lockoutRig) postLoginWithCookies(t *testing.T, email string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Email: email})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.20:40000"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	lr.h.HandleLogin(w, req)
	return w
}

func withCookieValue(cookies []*http.Cookie, name, value string) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		cc := *c
		if cc.Name == name {
			cc.Value = value
		}
		out = append(out, &cc)
	}
	return out
}

func cookieIn(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestLoginThrottle_SignedInBrowserStillGetsALink — the per-address send
// cap is shared by everyone who knows the address, so while it is full the
// owner's own requests were skipped like anyone else's. A browser that has
// signed in to the address before must still get a working link.
func TestLoginThrottle_SignedInBrowserStillGetsALink(t *testing.T) {
	const email = "owner@example.com"
	lr := newLockoutRig(t)
	browser := lr.signIn(t, email)
	lr.cfg.LoginThrottle = &stubLoginThrottle{allow: false}
	sent := lr.sender.SentCount()

	lr.postLoginWithCookies(t, email, nil)
	if got := lr.sender.SentCount(); got != sent {
		t.Fatalf("a caller without the proof got a send past the full cap (%d → %d)", sent, got)
	}

	w := lr.postLoginWithCookies(t, email, browser)
	if got := lr.sender.SentCount(); got != sent+1 {
		t.Fatalf("sent = %d, want %d: the signed-in browser must get a link while the cap is full", got, sent+1)
	}
	plaintext := lr.extractTokenFromSentEmail(t)
	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(plaintext), nil)
	attachCookies(cb, w)
	cbw := httptest.NewRecorder()
	lr.h.HandleCallback(cbw, cb)
	if cbw.Code != http.StatusSeeOther || !sessionCookieSet(cbw) {
		t.Fatalf("callback status = %d, session = %v: the link must complete in the browser that asked",
			cbw.Code, sessionCookieSet(cbw))
	}

	lr.postLoginWithCookies(t, email, browser)
	if got := lr.sender.SentCount(); got != sent+1 {
		t.Fatalf("sent = %d, want %d: one bypass send per link lifetime", got, sent+1)
	}
	lr.advance(lr.cfg.MagicLinkTTL)
	lr.postLoginWithCookies(t, email, browser)
	if got := lr.sender.SentCount(); got != sent+2 {
		t.Fatalf("sent = %d, want %d: the next link lifetime admits one more", got, sent+2)
	}
}

// TestLoginThrottle_SignedInProofIsBoundAndUnforgeable — the proof admits
// only its own address, only unaltered, and only until it expires.
func TestLoginThrottle_SignedInProofIsBoundAndUnforgeable(t *testing.T) {
	const email = "owner2@example.com"
	lr := newLockoutRig(t)
	browser := lr.signIn(t, email)
	other := lr.signIn(t, "someone-else@example.com")
	lr.cfg.LoginThrottle = &stubLoginThrottle{allow: false}

	proof := cookieIn(browser, loginDeviceCookieWireName)
	if proof == nil {
		t.Fatalf("sign-in set no %s cookie", loginDeviceCookieWireName)
	}
	if proof.Path != "/v1/auth" || !proof.HttpOnly {
		t.Errorf("proof cookie Path = %q HttpOnly = %v, want /v1/auth and true", proof.Path, proof.HttpOnly)
	}
	if bytes.Contains([]byte(proof.Value), []byte("owner2")) {
		t.Errorf("proof cookie carries the address in clear: %q", proof.Value)
	}
	tampered := []byte(proof.Value)
	tampered[len(tampered)-1] ^= 1

	sent := lr.sender.SentCount()
	cases := map[string][]*http.Cookie{
		"another address's proof": other,
		"tampered MAC":            withCookieValue(browser, loginDeviceCookieWireName, string(tampered)),
		"garbage":                 withCookieValue(browser, loginDeviceCookieWireName, "not-a-proof"),
	}
	for name, cookies := range cases {
		lr.postLoginWithCookies(t, email, cookies)
		if got := lr.sender.SentCount(); got != sent {
			t.Fatalf("%s: got a send past the full cap", name)
		}
	}

	lr.advance(401 * 24 * time.Hour) // past the 400-day proof lifetime
	lr.postLoginWithCookies(t, email, browser)
	if got := lr.sender.SentCount(); got != sent {
		t.Fatal("an expired proof got a send past the full cap")
	}
}

func (lr *lockoutRig) postVerifyCodeWithCookies(t *testing.T, email, code string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(verifyCodeRequest{Email: email, Code: code})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-code", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.20:40000"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	lr.h.HandleVerifyCode(w, req)
	return w
}

// TestLockout_SignedInBrowserIsNotLockedOut — anyone who knows an address
// can spend its durable code budget, so a stranger's ten wrong codes must
// not shut the owner's own browser out of code sign-in, and that browser's
// typos must not spend the budget strangers are held to.
func TestLockout_SignedInBrowserIsNotLockedOut(t *testing.T) {
	const email = "owner3@example.com"
	lr := newLockoutRig(t)
	browser := lr.signIn(t, email)

	lr.grind(t, email, maxDurableCodeFailures)

	code := lr.loginAndCode(t, email)
	if w := lr.postVerifyCode(t, email, code); w.Code != http.StatusBadRequest {
		t.Fatalf("pre-condition: a caller without the proof got status %d, want 400 while locked", w.Code)
	}
	before := lr.durableFailures(email)
	if w := lr.postVerifyCodeWithCookies(t, email, wrongCode(code), browser); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong code from the signed-in browser: status = %d, want 400", w.Code)
	}
	if got := lr.durableFailures(email); got != before {
		t.Errorf("signed-in browser's typo charged the shared budget: %d → %d", before, got)
	}
	w := lr.postVerifyCodeWithCookies(t, email, code, browser)
	if w.Code != http.StatusOK || !sessionCookieSet(w) {
		t.Fatalf("signed-in browser locked out of its own address: status = %d, body %s", w.Code, w.Body.String())
	}

	other := lr.signIn(t, "stranger@example.com")
	lr.grind(t, email, maxDurableCodeFailures)
	code = lr.loginAndCode(t, email)
	if w := lr.postVerifyCodeWithCookies(t, email, code, other); w.Code != http.StatusBadRequest {
		t.Fatalf("another address's proof passed the lockout: status = %d, want 400", w.Code)
	}
}
