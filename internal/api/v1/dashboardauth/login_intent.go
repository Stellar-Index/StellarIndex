package dashboardauth

import (
	"crypto/hmac"
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
// digest of the token it just minted; `GET /v1/auth/callback`
// refuses to mint a session unless the presented token's digest is
// in the cookie. The attacker cannot set a cookie on the victim's
// browser for the API's own host (the __Host- prefix refuses one
// planted from a sibling host) and cannot compute a valid slot
// without the server secret, so their link can no longer be
// completed anywhere but their own browser.
//
// Deliberately distinct from [SessionCookieName] so a browser that
// holds both can't have one surface's credential read as the
// other's, matching that constant's own rationale.
const LoginIntentCookieName = "__Host-stellarindex_login_intent"

// maxLoginIntents caps how many concurrently-live magic links one
// browser may still complete. A user who taps "email me a link"
// twice (impatient, or the first mail was slow) legitimately holds
// two live tokens and may click either, so a single-slot cookie
// would break the older-link click. Three slots covers that
// without letting the cookie grow unbounded (3 × 64 hex chars +
// separators ≈ 194 bytes).
const maxLoginIntents = 3

// loginIntentDigestLen is the hex length of one HMAC-SHA256 digest.
const loginIntentDigestLen = sha256.Size * 2

// loginIntentSeparator joins the digests inside the cookie value.
// '.' is unambiguously safe in a cookie value (RFC 6265 cookie-
// octet) and cannot appear inside a hex digest, so a split can
// never merge two slots.
const loginIntentSeparator = "."

// loginIntentDomain separates this MAC from the other uses of the
// server secret (code derivation, passkey ceremony).
const loginIntentDomain = "stellarindex/login-intent/v2|"

// loginIntentDigest derives the cookie slot for a magic-link token
// from the token's stored hash, keyed by the server secret so only
// this server can mint a slot [Handlers.hasLoginIntent] accepts. The
// cookie is a binding witness, not a credential.
func loginIntentDigest(secret, tokenHash []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(loginIntentDomain))
	mac.Write(tokenHash)
	return hex.EncodeToString(mac.Sum(nil))
}

// isLoginIntentDigest reports whether s has the exact shape this
// package writes. Client-supplied cookie content is echoed back
// into a Set-Cookie header on the next login, so it is validated
// rather than trusted — net/http would sanitise a malformed value,
// but dropping it outright keeps the cookie's contents provably
// self-generated.
func isLoginIntentDigest(s string) bool {
	if len(s) != loginIntentDigestLen {
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

// setLoginIntentCookie records that THIS browser minted the token
// identified by tokenHash, keeping up to [maxLoginIntents]-1 of the
// browser's still-live prior intents ahead of the eviction edge.
//
// TTL mirrors MagicLinkTTL: the cookie is worthless the moment the
// token it witnesses expires, so it should not outlive it.
func (h *Handlers) setLoginIntentCookie(w http.ResponseWriter, r *http.Request, tokenHash []byte) {
	intents := []string{loginIntentDigest(h.cfg.Generator.Secret, tokenHash)}
	if c, err := r.Cookie(LoginIntentCookieName); err == nil {
		for _, prev := range strings.Split(c.Value, loginIntentSeparator) {
			if len(intents) >= maxLoginIntents {
				break
			}
			if prev == intents[0] || !isLoginIntentDigest(prev) {
				continue
			}
			intents = append(intents, prev)
		}
	}
	c := credentialCookie(LoginIntentCookieName, strings.Join(intents, loginIntentSeparator))
	c.MaxAge = int(h.cfg.MagicLinkTTL / time.Second)
	http.SetCookie(w, c)
}

// clearLoginIntentCookie drops the witness once a link has been
// redeemed. Not load-bearing for security (the token itself is
// single-use), just hygiene: a spent binding shouldn't linger in
// the browser for the rest of the TTL.
func (h *Handlers) clearLoginIntentCookie(w http.ResponseWriter) {
	c := credentialCookie(LoginIntentCookieName, "")
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
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("stellarindex/login-device/v1|"))
	m.Write([]byte(strconv.FormatInt(expires, 10)))
	m.Write([]byte("|"))
	m.Write([]byte(email))
	return hex.EncodeToString(m.Sum(nil))
}

// setLoginDeviceCookie issues the proof for email, which must be in
// [notify.CanonicalRecipient] form — the form HandleLogin checks against.
func (h *Handlers) setLoginDeviceCookie(w http.ResponseWriter, email string) {
	expires := h.cfg.Now().Add(loginDeviceTTL).Unix()
	http.SetCookie(w, &http.Cookie{
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
// for the magic link identified by tokenHash.
//
// Constant-time compared per slot: the digest is derived from the
// token hash, which is derived from the emailed plaintext, so a
// timing oracle here would leak progress on the token itself.
func (h *Handlers) hasLoginIntent(r *http.Request, tokenHash []byte) bool {
	c, err := r.Cookie(LoginIntentCookieName)
	if err != nil {
		return false
	}
	want := []byte(loginIntentDigest(h.cfg.Generator.Secret, tokenHash))
	for _, got := range strings.Split(c.Value, loginIntentSeparator) {
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			return true
		}
	}
	return false
}
