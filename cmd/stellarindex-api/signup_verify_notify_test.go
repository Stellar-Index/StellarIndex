package main

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// failingNotifySender is a notify.Sender whose Send always errors — a Resend
// outage from the signup-verify adapter's point of view.
type failingNotifySender struct{ err error }

func (f failingNotifySender) Send(context.Context, notify.Message) error { return f.err }

func signupNotifyCount(t *testing.T, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateSignupVerify, result))
}

// TestSignupVerifyEmailer_RecordsNotifySendMetric pins task #33 / W8 recon 9c
// for the signup-verification path: the second notify.Sender call site (the
// API-signup confirmation email) must bump stellarindex_notify_sends_total{
// template="signup-verify"} — result=sent when Resend accepts, result=failed
// when it errors — so a mail outage that stops signup confirmations is visible.
func TestSignupVerifyEmailer_RecordsNotifySendMetric(t *testing.T) {
	ctx := context.Background()

	// Success path: NoopSender accepts a well-formed message.
	adapter := &signupVerifyEmailerAdapter{
		sender: &notify.NoopSender{},
		from:   "Stellar Index <hello@stellarindex.io>",
	}
	beforeSent := signupNotifyCount(t, obs.NotifySendResultSent)
	if err := adapter.SendSignupVerification(ctx, "alice@example.com", "https://api.stellarindex.io/verify?token=x"); err != nil {
		t.Fatalf("SendSignupVerification (noop) returned error: %v", err)
	}
	if got := signupNotifyCount(t, obs.NotifySendResultSent) - beforeSent; got != 1 {
		t.Errorf("notify_sends_total{template=signup-verify,result=sent} delta = %v, want 1", got)
	}

	// Failure path: the provider errors; the counter is the outage signal.
	sendErr := errors.New("notify: transient provider failure")
	failing := &signupVerifyEmailerAdapter{
		sender: failingNotifySender{err: sendErr},
		from:   "Stellar Index <hello@stellarindex.io>",
	}
	beforeFailed := signupNotifyCount(t, obs.NotifySendResultFailed)
	if err := failing.SendSignupVerification(ctx, "bob@example.com", "https://api.stellarindex.io/verify?token=y"); err == nil {
		t.Fatal("SendSignupVerification (failing sender) returned nil, want the send error propagated")
	}
	if got := signupNotifyCount(t, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("notify_sends_total{template=signup-verify,result=failed} delta = %v, want 1", got)
	}
}
