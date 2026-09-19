package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// rlt321MailEnv is a test-private env var name, so the test never reads (or
// clobbers) the real STELLARINDEX_RESEND_API_KEY of whoever runs it.
const rlt321MailEnv = "STELLARINDEX_TEST_RLT321_MAIL_TRANSPORT"

// countingTokenStore records CreateMagicLinkToken calls. Every other
// TokenStore method is the embedded nil interface: HandleLogin must not reach
// them, and a nil-deref panic is the right answer if it ever does.
type countingTokenStore struct {
	platform.TokenStore
	mu      sync.Mutex
	created int
}

func (s *countingTokenStore) CreateMagicLinkToken(context.Context, platform.MagicLinkToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	return nil
}

func (s *countingTokenStore) createdCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created
}

type (
	unusedAccountStore struct{ platform.AccountStore }
	unusedUserStore    struct{ platform.UserStore }
)

func magicLinkNotifyCount(t *testing.T, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateMagicLink, result))
}

// loginThroughProductionWiring builds the sender exactly as the API binary
// does (buildDashboardSender, reading the env), hands it to the production
// dashboardauth handler, and POSTs one /v1/auth/login.
func loginThroughProductionWiring(t *testing.T, logs *bytes.Buffer) (*httptest.ResponseRecorder, *countingTokenStore, notify.Sender) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	from := "Stellar Index <hello@stellarindex.io>"
	sender, err := buildDashboardSender(config.DashboardConfig{
		ResendAPIKeyEnv: rlt321MailEnv,
		EmailFrom:       from,
	}, logger)
	if err != nil {
		t.Fatalf("buildDashboardSender: %v", err)
	}
	tokens := &countingTokenStore{}
	h, err := dashboardauth.NewHandlers(dashboardauth.Config{
		Accounts:         unusedAccountStore{},
		Users:            unusedUserStore{},
		Tokens:           tokens,
		Sender:           sender,
		Logger:           logger,
		DashboardBaseURL: "https://stellarindex.io",
		EmailFrom:        from,
		MagicLinkTTL:     15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login",
		strings.NewReader(`{"email":"alice@example.com"}`))
	req.RemoteAddr = "203.0.113.5:55123"
	w := httptest.NewRecorder()
	h.HandleLogin(w, req)
	return w, tokens, sender
}

// TestLogin_EmptyResendKey_IsACountedFailureNeverSent pins RLT-321 through the
// production wiring + the production login handler.
//
// Before the fix an empty/unset Resend key wired a NoopSender whose Send
// returns nil: the handler answered 200 {"status":"sent"}, bumped
// notify_sends_total{result="sent"}, minted a live magic-link row nobody could
// ever receive, and the failure-ratio alert (failed/total > 0.5) read 0 while
// no sign-in email could be delivered at all.
func TestLogin_EmptyResendKey_IsACountedFailureNeverSent(t *testing.T) {
	t.Setenv(rlt321MailEnv, "")
	beforeSent := magicLinkNotifyCount(t, obs.NotifySendResultSent)
	beforeFailed := magicLinkNotifyCount(t, obs.NotifySendResultFailed)

	var logs bytes.Buffer
	w, tokens, sender := loginThroughProductionWiring(t, &logs)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("login status with no mail credential = %d, want 503 — the API must not "+
			"claim a sign-in email it cannot deliver", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `"sent"`) {
		t.Errorf("login body reports sent with no mail credential: %s", body)
	}
	if got := magicLinkNotifyCount(t, obs.NotifySendResultSent) - beforeSent; got != 0 {
		t.Errorf("notify_sends_total{magic-link,sent} delta = %v, want 0 — an undeliverable "+
			"mail counted as sent holds the failure-ratio alert at 0", got)
	}
	if got := magicLinkNotifyCount(t, obs.NotifySendResultFailed) - beforeFailed; got != 1 {
		t.Errorf("notify_sends_total{magic-link,failed} delta = %v, want 1", got)
	}
	if got := tokens.createdCount(); got != 0 {
		t.Errorf("magic-link rows minted = %d, want 0 — a link nobody can receive must not go live", got)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == dashboardauth.LoginIntentCookieName {
			t.Errorf("login-intent cookie set for a link that was never minted")
		}
	}
	if !strings.Contains(logs.String(), rlt321MailEnv) {
		t.Errorf("boot log does not name the env var the operator must set: %s", logs.String())
	}
	// The signup sibling must keep treating this transport as "no mail":
	// email_verification_sent:false, never a doomed send.
	if e := signupVerifyEmailerOrNil(sender, "Stellar Index <hello@stellarindex.io>"); e != nil {
		t.Errorf("signupVerifyEmailerOrNil = %T, want nil for a transport with no credential", e)
	}
}

// TestBuildDashboardSender_WhitespaceOnlyCredential_IsNotATransport: a
// whitespace-only value is no credential. It must not be handed to Resend as
// a blank bearer. Asserted at the wiring only — no Send — so this test stays
// offline on unfixed code too.
func TestBuildDashboardSender_WhitespaceOnlyCredential_IsNotATransport(t *testing.T) {
	t.Setenv(rlt321MailEnv, "  \t")
	var logs bytes.Buffer
	sender, err := buildDashboardSender(config.DashboardConfig{
		ResendAPIKeyEnv: rlt321MailEnv,
		EmailFrom:       "Stellar Index <hello@stellarindex.io>",
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("buildDashboardSender: %v", err)
	}
	if _, isResend := sender.(*notify.ResendSender); isResend {
		t.Errorf("a whitespace-only credential wired a live Resend transport")
	}
	if e := signupVerifyEmailerOrNil(sender, "Stellar Index <hello@stellarindex.io>"); e != nil {
		t.Errorf("signupVerifyEmailerOrNil = %T, want nil for a whitespace-only credential", e)
	}
}

// TestBuildDashboardSender_WithCredential_WiresResendAndNeverLogsIt is the
// other half of the wiring: a present credential still selects the real
// Resend transport, and neither the value nor any part of it reaches the log.
func TestBuildDashboardSender_WithCredential_WiresResendAndNeverLogsIt(t *testing.T) {
	const fixtureValue = "rlt321-fixture-not-a-real-credential" // gitleaks:allow
	t.Setenv(rlt321MailEnv, "  "+fixtureValue+"\n")

	var logs bytes.Buffer
	sender, err := buildDashboardSender(config.DashboardConfig{
		ResendAPIKeyEnv: rlt321MailEnv,
		EmailFrom:       "Stellar Index <hello@stellarindex.io>",
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("buildDashboardSender: %v", err)
	}
	rs, ok := sender.(*notify.ResendSender)
	if !ok {
		t.Fatalf("sender = %T, want *notify.ResendSender", sender)
	}
	if rs.APIKey != fixtureValue {
		t.Errorf("credential not trimmed: a stray newline would corrupt the Authorization header")
	}
	if strings.Contains(logs.String(), fixtureValue) || strings.Contains(logs.String(), fixtureValue[:8]) {
		t.Errorf("the mail credential (or a prefix of it) reached the log")
	}
	if signupVerifyEmailerOrNil(sender, "Stellar Index <hello@stellarindex.io>") == nil {
		t.Errorf("signupVerifyEmailerOrNil = nil for a configured Resend transport")
	}
}
