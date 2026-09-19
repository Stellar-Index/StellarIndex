package dashboardauth

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// RLT-321. A deployment with no Resend credential wires a transport that
// declares it cannot deliver (notify.UnconfiguredSender). HandleLogin must
// refuse up front: 503, a counted failed send, and NO side effect — no
// magic-link row, no login-intent cookie — for a link nobody can receive.
//
// Reverting only the handler guard leaves the send-error branch to absorb
// ErrNotConfigured: 200 {"status":"sent"} and a live token row. That is the
// shape this test goes red on.
func TestHandleLogin_MailUnconfigured_Refuses503WithNoSideEffects(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{Reason: "env X is unset/empty"}
	beforeSent := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent)
	beforeFailed := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed)

	w := r.postLogin(t, "alice@example.com")

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("login status = %d, want 503 when the mail transport has no credential", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if strings.Contains(w.Body.String(), `"sent"`) {
		t.Errorf("body claims sent: %s", w.Body.String())
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent) - beforeSent; got != 0 {
		t.Errorf("result=sent delta = %v, want 0", got)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("result=failed delta = %v, want 1 (the failure-ratio alert reads this counter)", got)
	}
	r.tokens.mu.Lock()
	rows := len(r.tokens.tokens)
	r.tokens.mu.Unlock()
	if rows != 0 {
		t.Errorf("magic-link rows = %d, want 0", rows)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == LoginIntentCookieName {
			t.Errorf("login-intent cookie set although no link was minted")
		}
	}
}

// The refusal must be the same for every well-formed request. The throttled
// branch answers a decoy 200 {"status":"sent"}; if the mail guard ran after
// it, a 200 among 503s would tell a caller "a throttle fired for this
// address" — the oracle [LoginThrottle]'s contract forbids — and would claim
// an email on a deployment that cannot send one.
func TestHandleLogin_MailUnconfigured_RefusesBeforeTheThrottle(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{}
	thr := &stubLoginThrottle{allow: false}
	r.cfg.LoginThrottle = thr

	w := r.postLogin(t, "alice@example.com")

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("throttled login status = %d, want the same 503 as an unthrottled one", w.Code)
	}
	if thr.calls != 0 {
		t.Errorf("throttle consulted %d time(s); the mail guard must run first so a refused "+
			"request cannot burn a victim's per-email quota", thr.calls)
	}
}

// A malformed request is still the caller's error: the guard sits behind
// request validation, so it never masks a 400.
func TestHandleLogin_MailUnconfigured_InvalidEmailIsStill400(t *testing.T) {
	r := newTestRig(t)
	r.cfg.Sender = notify.UnconfiguredSender{}
	if w := r.postLogin(t, "not-an-email"); w.Code != http.StatusBadRequest {
		t.Errorf("invalid email status = %d, want 400", w.Code)
	}
}

// The recording test double stays a working transport: the guard keys on a
// transport DECLARING it has no credential, not on "is not Resend", so the
// happy path every other test in this package drives is untouched.
func TestHandleLogin_RecordingSender_StillSends(t *testing.T) {
	r := newTestRig(t)
	if w := r.postLogin(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", w.Code)
	}
	if r.sender.SentCount() != 1 {
		t.Errorf("SentCount = %d, want 1", r.sender.SentCount())
	}
}
