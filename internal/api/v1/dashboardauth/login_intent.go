package dashboardauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LoginIntentCookieName is the short-lived cookie that binds a
// magic link to the browser that asked for it.
//
// C3-030 (audit-2026-07-23) — login CSRF. Before this cookie
// existed, `GET /v1/auth/callback` minted a session from a token
// carried purely in the query string, and `sessionSameSite()`
// returns Lax, which *permits* top-level cross-site GET
// navigation. So an attacker could request a magic link for their
// OWN account and mail that link to a victim: the victim's browser
// followed it, got the attacker's session cookie, and every
// subsequent action the victim took (minting an API key, attaching
// a payment method) landed in the attacker's dashboard. That is
// the classic login-CSRF shape and neither the token's
// single-use-ness nor its 15-minute TTL touches it — the attacker
// is happy to spend a fresh token per victim.
//
// The binding: `POST /v1/auth/login` stamps this cookie with a
// random per-browser id and mints a token whose second half is a
// MAC of its first half and that id ([Generator.newBoundToken]);
// `GET /v1/auth/callback` refuses to mint a session unless the MAC
// verifies against the presenting browser's id. The attacker cannot
// set a cookie on the victim's browser for the API's own host (the
// __Host- prefix refuses one planted from a sibling host) and cannot
// compute a tag without the server secret, so their link can no
// longer be completed anywhere but their own browser.
//
// Deliberately distinct from [SessionCookieName] so a browser that
// holds both can't have one surface's credential read as the
// other's, matching that constant's own rationale.
const LoginIntentCookieName = "__Host-stellarindex_login_intent"

// loginIntentBrowserLen is the hex length of a browser id and of a
// full magic-link plaintext.
const loginIntentBrowserLen = MagicLinkPlaintextLen * 2

// loginIntentSeparator joins the fields of the login-device cookie
// value. '.' is a valid RFC 6265 cookie-octet and never appears in
// hex or a decimal expiry.
const loginIntentSeparator = "."

// loginIntentDomain separates this MAC from the other uses of the
// server secret (code derivation, passkey ceremony).
const loginIntentDomain = "stellarindex/login-intent/v3|"

// loginDeviceDomain labels the login-device marker's MAC key and input.
const loginDeviceDomain = "stellarindex/login-device/v1|"

// loginIntentTag is the second half of a bound magic-link plaintext:
// a MAC, under the server secret, of the random first half and the
// requesting browser's id. Only this server can compute it, and it
// binds the link to exactly one browser.
func loginIntentTag(secret []byte, nonceHex, browser string) string {
	mac := hmac.New(sha256.New, mustPurposeKey(secret, loginIntentDomain))
	mac.Write([]byte(loginIntentDomain))
	mac.Write([]byte(nonceHex))
	mac.Write([]byte("|"))
	mac.Write([]byte(browser))
	return hex.EncodeToString(mac.Sum(nil)[:MagicLinkPlaintextLen/2])
}

// isLoginIntentHex reports whether s is exactly loginIntentBrowserLen
// lowercase hex characters. Client-supplied cookie content is echoed back
// in Set-Cookie, so it is validated rather than trusted.
func isLoginIntentHex(s string) bool {
	if len(s) != loginIntentBrowserLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newBoundToken mints a magic-link token whose plaintext is
// `random half || loginIntentTag(random half, browser)`: same length and
// alphabet as an unbound one, with the browser binding carried in the
// link itself so the cookie holds no per-link state.
func (g *Generator) newBoundToken(browser string) (plaintext string, hash []byte, code string, err error) {
	raw, _, _, err := g.NewToken()
	if err != nil {
		return "", nil, "", err
	}
	nonce := raw[:loginIntentBrowserLen/2]
	plaintext = nonce + loginIntentTag(g.Secret, nonce, browser)
	hash = HashMagicLinkPlaintext(plaintext)
	return plaintext, hash, g.CodeForHash(hash), nil
}

// setLoginIntentCookie stamps this browser's login-intent id and returns
// it: the id the request already carries, else a fresh random one. It
// depends on nothing but the request's own cookie, so a throttled login
// and a real send emit the same value and leave the same state behind —
// no sequence of requests can tell them apart — and any number of links
// requested from one browser stay redeemable there.
//
// TTL mirrors MagicLinkTTL, refreshed on every request: the id is
// worthless once the newest link it binds has expired.
func (h *Handlers) setLoginIntentCookie(w http.ResponseWriter, r *http.Request) string {
	var browser string
	if c, err := r.Cookie(LoginIntentCookieName); err == nil && isLoginIntentHex(c.Value) {
		browser = c.Value
	} else {
		id := make([]byte, MagicLinkPlaintextLen)
		_, _ = rand.Read(id) // never errors since Go 1.24; it crashes the program instead
		browser = hex.EncodeToString(id)
	}
	c := credentialCookie(LoginIntentCookieName, browser) //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	c.MaxAge = int(h.cfg.MagicLinkTTL / time.Second)
	http.SetCookie(w, c)
	return browser
}

// clearLoginIntentCookie drops the witness once a link has been
// redeemed. Not load-bearing for security (the token itself is
// single-use), just hygiene: a spent binding shouldn't linger in
// the browser for the rest of the TTL.
func (h *Handlers) clearLoginIntentCookie(w http.ResponseWriter) {
	c := credentialCookie(LoginIntentCookieName, "") //nolint:gosec // G124: credentialCookie sets Secure, HttpOnly and SameSite=Lax
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// LoginDeviceCookieName marks a browser that has completed a sign-in for
// one address. Its only effect: POST /v1/auth/login from this browser, for
// that address, still gets a link when the per-address send cap — which
// anyone who knows the address can fill — is exhausted (see
// [Handlers.admitSignedInBrowser]). It authenticates nothing and is
// scoped by Path to the login route, so no other route ever receives it.
const LoginDeviceCookieName = "stellarindex_login_device"

// loginDeviceTTL is the longest cookie lifetime browsers honour (RFC 6265bis
// caps Max-Age at 400 days); every sign-in re-issues it.
const loginDeviceTTL = 400 * 24 * time.Hour

// loginDeviceMAC binds an address and an expiry under the server secret,
// so the cookie can be neither forged nor moved to another address.
func loginDeviceMAC(secret []byte, email string, expires int64) string {
	m := hmac.New(sha256.New, mustPurposeKey(secret, loginDeviceDomain))
	m.Write([]byte(loginDeviceDomain))
	m.Write([]byte(strconv.FormatInt(expires, 10)))
	m.Write([]byte("|"))
	m.Write([]byte(email))
	return hex.EncodeToString(m.Sum(nil))
}

// setLoginDeviceCookie issues the proof for email, which must be in
// [notify.CanonicalRecipient] form — the form HandleLogin checks against.
func (h *Handlers) setLoginDeviceCookie(w http.ResponseWriter, email string) {
	expires := h.cfg.Now().Add(loginDeviceTTL).Unix()
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly, SameSite=Lax; Secure follows cookie_secure (default true, false only for http dev)
		Name:     LoginDeviceCookieName,
		Value:    strconv.FormatInt(expires, 10) + loginIntentSeparator + loginDeviceMAC(h.cfg.Generator.Secret, email, expires),
		Path:     "/v1/auth/login",
		Domain:   h.cfg.SessionHintDomain,
		MaxAge:   int(loginDeviceTTL / time.Second),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}

// hasLoginDeviceProof reports whether r carries an unexpired proof that
// this browser has signed in to email before.
func (h *Handlers) hasLoginDeviceProof(r *http.Request, email string) bool {
	c, err := r.Cookie(LoginDeviceCookieName)
	if err != nil {
		return false
	}
	exp, mac, ok := strings.Cut(c.Value, loginIntentSeparator)
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expires <= h.cfg.Now().Unix() {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(loginDeviceMAC(h.cfg.Generator.Secret, email, expires)))
}

// hasLoginIntent reports whether this browser is the one that asked
// for the magic link whose plaintext is given.
//
// Constant-time compared: the tag is half of the emailed plaintext, so a
// timing oracle here would leak progress on the token itself.
func (h *Handlers) hasLoginIntent(r *http.Request, plaintext string) bool {
	c, err := r.Cookie(LoginIntentCookieName)
	if err != nil || !isLoginIntentHex(c.Value) || !isLoginIntentHex(plaintext) {
		return false
	}
	half := loginIntentBrowserLen / 2
	want := loginIntentTag(h.cfg.Generator.Secret, plaintext[:half], c.Value)
	return subtle.ConstantTimeCompare([]byte(plaintext[half:]), []byte(want)) == 1
}
