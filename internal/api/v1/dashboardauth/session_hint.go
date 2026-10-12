package dashboardauth

import (
	"net/http"
	"time"
)

// SessionHintCookieName is a JS-readable presence flag shadowing [SessionCookieName]; a HINT only, never an authorization signal (no identity, nothing server-side reads it).
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
		// Readable from JavaScript by design; safe because the value is a constant.
		HttpOnly: false,
		Secure:   h.cfg.CookieSecure,
		SameSite: sessionSameSite(),
	})
}

// clearSessionHintCookie drops the presence flag wherever the session cookie is cleared, so a
// browser is never left holding a hint for a session the server already dropped.
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
