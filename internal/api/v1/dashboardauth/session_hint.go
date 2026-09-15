package dashboardauth

import (
	"net/http"
	"time"
)

// SessionHintCookieName is a JS-readable presence flag that shadows
// [SessionCookieName]. It is written and cleared in lockstep with the
// session cookie and carries nothing else.
//
// Why it exists: the explorer is a separate origin from the API, so the
// only way it could tell a signed-in visitor from an anonymous one was
// to make a credentialed `GET /v1/account/me` and read the status. For
// an anonymous visitor that is a guaranteed 401 on every page load
// (plus a refetch every five minutes), and a browser emits its own
// console entry for any non-2xx response — an entry no amount of
// client-side error handling can suppress, because it comes from the
// network layer rather than from the promise.
//
// The session cookie itself cannot answer the question: it is HttpOnly
// (and must stay that way — it is the bearer credential). So the same
// responses that write the session cookie also write this one, which is
// NOT HttpOnly. `document.cookie` sees it, and the explorer skips the
// probe entirely when it is absent.
//
// It is a HINT, never an authorization signal:
//
//   - It carries no identity, no token and no email — the value is the
//     single byte "1". Anything an attacker could forge, they could
//     equally forge by setting this cookie themselves, and the worst
//     that buys them is one 401.
//   - Nothing server-side reads it. Every authenticated route still
//     resolves [SessionCookieName] against the sessions table.
//   - It can outlive the real session (server-side expiry, revocation,
//     one cookie cleared without the other). That is expected: the
//     probe then runs and 401s exactly as it does today, and the client
//     drops the stale hint.
const SessionHintCookieName = "stellarindex_session_present"

// sessionHintValue is the entire contents of the hint cookie. A
// constant, so no per-user information can reach it by accident.
const sessionHintValue = "1"

// setSessionHintCookie writes the presence flag alongside a freshly
// minted session cookie. Every attribute except HttpOnly mirrors the
// session cookie so the pair shares one scope and one lifetime.
func (h *Handlers) setSessionHintCookie(w http.ResponseWriter, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:    SessionHintCookieName,
		Value:   sessionHintValue,
		Path:    "/",
		Domain:  h.cfg.CookieDomain,
		Expires: expires,
		// Deliberately readable from JavaScript — that is the whole
		// point of this cookie. It is safe because the value is a
		// constant with no bearer power (see [SessionHintCookieName]).
		HttpOnly: false,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}

// clearSessionHintCookie drops the presence flag. Called from the same
// places that clear the session cookie, so a browser can never be left
// holding a hint for a session the server has already dropped on its
// own initiative.
func (h *Handlers) clearSessionHintCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionHintCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}
