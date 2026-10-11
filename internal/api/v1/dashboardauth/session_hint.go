package dashboardauth

import (
	"net/http"
	"time"
)

// SessionHintCookieName is a JS-readable presence flag shadowing
// [SessionCookieName]. It is written and cleared in lockstep with the
// session cookie and carries nothing else.
//
// Why: the explorer is a separate origin from the API, so it could only
// tell a signed-in visitor from an anonymous one by a credentialed `GET
// /v1/account/me`, a guaranteed 401 on every anonymous page load that logs
// a console entry no client code can suppress. The session cookie cannot
// answer: it is HttpOnly (and must stay so, being the bearer credential).
// So the responses that write it also write this one, NOT HttpOnly, and the
// explorer skips the probe when it is absent.
//
// It is a HINT, never an authorization signal:
//   - It carries no identity, token or email (the value is "1"), so
//     forging it buys at most one 401.
//   - Nothing server-side reads it; every authenticated route resolves
//     [SessionCookieName] against the sessions table.
//   - It can outlive the real session; the probe then 401s and the
//     client drops the stale hint.
const SessionHintCookieName = "stellarindex_session_present"

// sessionHintValue is the entire contents of the hint cookie. A
// constant, so no per-user information can reach it by accident.
const sessionHintValue = "1"

// setSessionHintCookie writes the presence flag alongside a freshly
// minted session cookie, with the same lifetime. Unlike the host-only
// session cookie it may carry [Config.SessionHintDomain] so the
// explorer's origin can read it.
func (h *Handlers) setSessionHintCookie(w http.ResponseWriter, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: JS-readable by design (constant flag, no bearer power); SameSite=Lax; Secure follows cookie_secure
		Name:    SessionHintCookieName,
		Value:   sessionHintValue,
		Path:    "/",
		Domain:  h.cfg.SessionHintDomain,
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
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: deletion of the JS-readable hint; same attributes as setSessionHintCookie
		Name:     SessionHintCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.cfg.SessionHintDomain,
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}
