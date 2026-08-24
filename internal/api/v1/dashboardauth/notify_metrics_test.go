package dashboardauth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stubFailSender is a notify.Sender whose Send always fails — the shape of a
// Resend outage from the login handler's point of view.
type stubFailSender struct{ err error }

func (s stubFailSender) Send(context.Context, notify.Message) error { return s.err }

func notifyCount(t *testing.T, template, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.NotifySendsTotal.WithLabelValues(template, result))
}

// TestHandleLogin_RecordsNotifySendMetric pins task #33 / W8 recon 9c for the
// magic-link path: internal/notify had zero prometheus visibility, and
// HandleLogin swallows the send error (returns 200 either way to avoid an
// enumeration oracle), so a mail outage that silently kills login was invisible.
// The send call site must now bump stellarindex_notify_sends_total{template=
// "magic-link"} with result=sent on success and result=failed on error.
func TestHandleLogin_RecordsNotifySendMetric(t *testing.T) {
	// Success path: the default rig wires a NoopSender (accepts the message).
	r := newTestRig(t)
	beforeSent := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent)

	if w := r.postLogin(t, "alice@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", w.Code)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultSent) - beforeSent; got != 1 {
		t.Errorf("notify_sends_total{template=magic-link,result=sent} delta = %v, want 1", got)
	}

	// Failure path: swap in a sender that always errors (a Resend outage).
	// The response is still 200 (enumeration-safe), so the counter is the
	// ONLY signal the mail never went out.
	r.cfg.Sender = stubFailSender{err: errors.New("resend: 503 service unavailable")}
	beforeFailed := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed)

	if w := r.postLogin(t, "bob@example.com"); w.Code != http.StatusOK {
		t.Fatalf("login status on send failure = %d, want 200 (enumeration-safe)", w.Code)
	}
	if got := notifyCount(t, obs.NotifyTemplateMagicLink, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("notify_sends_total{template=magic-link,result=failed} delta = %v, want 1", got)
	}
}
