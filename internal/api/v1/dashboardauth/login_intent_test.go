package dashboardauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

// TestLoginIntentTag_KeyedAndBound — the link's binding half must depend
// on the server secret (or anyone could forge a link for a browser whose
// id they learn), on the browser (or it binds nothing), and on the link's
// random half (or one tag would fit every link).
func TestLoginIntentTag_KeyedAndBound(t *testing.T) {
	secret := []byte("test-secret-one")
	nonce := strings.Repeat("0a", MagicLinkPlaintextLen/2)
	browser := strings.Repeat("1b", MagicLinkPlaintextLen)
	tag := loginIntentTag(secret, nonce, browser)
	if !isLoginIntentHex(nonce + tag) {
		t.Fatalf("bound plaintext %q is not the unbound token's shape", nonce+tag)
	}
	if loginIntentTag([]byte("test-secret-two"), nonce, browser) == tag {
		t.Fatal("tag does not depend on the server secret")
	}
	if loginIntentTag(secret, nonce, strings.Repeat("2c", MagicLinkPlaintextLen)) == tag {
		t.Fatal("tag does not depend on the browser")
	}
	if loginIntentTag(secret, strings.Repeat("3d", MagicLinkPlaintextLen/2), browser) == tag {
		t.Fatal("tag does not depend on the link's random half")
	}
}

// loginFrom posts a login from the browser that received prev, replaying
// its cookies; prev == nil is a fresh browser.
func (r *testRig) loginFrom(t *testing.T, email string, prev *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Email: email})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.5:55123"
	if prev != nil {
		attachCookies(req, prev)
	}
	w := httptest.NewRecorder()
	r.h.HandleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d", w.Code)
	}
	return w
}

func intentValue(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	c := cookieNamed(w, LoginIntentCookieName)
	if c == nil {
		t.Fatal("no login-intent cookie set")
	}
	return c.Value
}

// redeems reports whether plaintext signs in from the browser holding
// w's cookies.
func (r *testRig) redeems(t *testing.T, plaintext string, w *httptest.ResponseRecorder) bool {
	t.Helper()
	cb := callbackFor(plaintext)
	attachCookies(cb, w)
	out := httptest.NewRecorder()
	r.h.HandleCallback(out, cb)
	return out.Code == http.StatusSeeOther
}
