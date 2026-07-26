package dashboardauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestHandleCallback_MailedLinkDoesNotSignInAForeignBrowser is the
// headline C3-030 regression, written against nothing but the public
// behaviour so it stands on its own: the ONLY thing that changes
// between pre-fix and post-fix is whether a magic link mailed to a
// third party signs that third party into the requester's account.
//
// Attack: the attacker requests a link for their OWN address, then
// mails the link to a victim. The victim's browser has never been
// through /v1/auth/login, so it holds nothing that ties it to the
// link. Pre-fix, the callback minted the attacker's session there
// anyway (`sessionSameSite()` is Lax, which permits top-level
// cross-site GET navigation), and every later action the victim took
// — minting an API key, attaching a payment method — landed in the
// attacker's dashboard.
func TestHandleCallback_MailedLinkDoesNotSignInAForeignBrowser(t *testing.T) {
	r := newTestRig(t)

	if w := r.postLogin(t, "attacker@evil.example"); w.Code != http.StatusOK {
		t.Fatalf("attacker login: %d", w.Code)
	}
	attackerToken := r.extractTokenFromSentEmail(t)

	// A browser that never asked for this link.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/auth/callback?token="+url.QueryEscape(attackerToken), nil)
	req.RemoteAddr = "198.51.100.7:44001"
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			t.Fatalf("login CSRF: a mailed link signed a foreign browser in "+
				"(session cookie %q, status %d)", c.Value, w.Code)
		}
	}
	if w.Code == http.StatusSeeOther {
		t.Fatalf("status = 303: the foreign browser was redirected into the dashboard as the link's owner")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
