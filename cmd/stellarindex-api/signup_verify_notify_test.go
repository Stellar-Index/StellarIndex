package main

import (
	"context"
	"errors"
	"strings"
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

// recordingNotifySender captures the last message handed to Send.
type recordingNotifySender struct{ last notify.Message }

func (r *recordingNotifySender) Send(_ context.Context, msg notify.Message) error {
	r.last = msg
	return nil
}

// TestSignupVerifyEmailer_EscapesVerifyURLInHTML pins RLT-326: verifyURL
// carries the request Host, so it must be contextually escaped into the HTML
// body rather than concatenated — a quote in it must not break out of href.
func TestSignupVerifyEmailer_EscapesVerifyURLInHTML(t *testing.T) {
	rec := &recordingNotifySender{}
	adapter := &signupVerifyEmailerAdapter{sender: rec, from: "Stellar Index <hello@stellarindex.io>"}

	hostile := `https://evil.example"><script>alert(1)</script>/v1/signup/verify?token=a&b`
	if err := adapter.SendSignupVerification(context.Background(), "alice@example.com", hostile); err != nil {
		t.Fatalf("SendSignupVerification: %v", err)
	}
	const wantAnchor = `<p><a href="https://evil.example%22%3e%3cscript%3ealert%281%29%3c/script%3e/v1/signup/verify?token=a&amp;b">` +
		`https://evil.example&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;/v1/signup/verify?token=a&amp;b</a></p>`
	if !strings.Contains(rec.last.HTML, wantAnchor) {
		t.Fatalf("HTML anchor not escaped:\n got: %s\nwant substring: %s", rec.last.HTML, wantAnchor)
	}
	if strings.Contains(rec.last.HTML, "<script>") {
		t.Fatalf("HTML body carries a raw <script> from verifyURL: %s", rec.last.HTML)
	}
	// The plaintext body is not markup; it keeps the URL verbatim.
	if !strings.Contains(rec.last.Text, "\n"+hostile+"\n") {
		t.Fatalf("text body must carry verifyURL verbatim, got: %s", rec.last.Text)
	}

	// An ordinary URL round-trips byte-identical into both href and link text.
	const ordinary = "https://api.stellarindex.io/v1/signup/verify?token=Zm9vYmFy-_09"
	if err := adapter.SendSignupVerification(context.Background(), "bob@example.com", ordinary); err != nil {
		t.Fatalf("SendSignupVerification: %v", err)
	}
	if want := `<p><a href="` + ordinary + `">` + ordinary + `</a></p>`; !strings.Contains(rec.last.HTML, want) {
		t.Fatalf("ordinary URL mangled:\n got: %s\nwant substring: %s", rec.last.HTML, want)
	}
}
