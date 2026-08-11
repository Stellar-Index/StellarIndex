package v1_test

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

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// OBS-04 / audit PRV1: no handler in package v1 may write a customer's
// plaintext email address into the application log. The dashboardauth
// package already routes all 12 of its email log fields through a mask;
// the signup error path in this package did not, so an incident that
// made it noisy wrote a stream of live customer addresses into
// whatever sink the logs ship to.
//
// These tests assert the property at the CALL SITE (drive the handler,
// read the log), not on the helper — a helper-only test would pass
// against the un-fixed code, which still logged the raw address.

// captureLogger returns a slog.Logger writing JSON into buf, plus a
// mutex-guarded read-back. Debug level so nothing is filtered out.
func captureLogger() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// failingVerifyEmailer always fails the send, driving signup.go's
// "signup verification: send failed" warn path — the line that carried
// the plaintext address.
type failingVerifyEmailer struct{}

func (failingVerifyEmailer) SendSignupVerification(_ context.Context, _, _ string) error {
	return errAssertFailedSend
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errAssertFailedSend = constErr("smtp upstream refused")

// noopSignupVerifier reserves without persisting — enough to reach the
// emailer call.
type noopSignupVerifier struct{}

func (noopSignupVerifier) Reserve(_ context.Context, _, _ string, _ time.Duration) error { return nil }

func (noopSignupVerifier) Consume(_ context.Context, _ string) (string, error) { return "", nil }

// TestSignupVerifySendFailure_DoesNotLogPlaintextEmail — POST /v1/signup
// with a working key mint but a failing verification emailer must log
// the masked address, never the raw one.
func TestSignupVerifySendFailure_DoesNotLogPlaintextEmail(t *testing.T) {
	const email = "alice.mcallister@example.com"

	logger, buf := captureLogger()
	srv := v1.New(v1.Options{
		Auth:     fakeAuthMiddleware(auth.Subject{}), // anonymous
		Logger:   logger,
		Accounts: &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_mask", Identifier: "signup-mask", Tier: auth.TierAPIKey, RateLimitPerMin: 100, CreatedAt: time.Now().UTC()}, plain: "sip_x"},
		Signups:  newFakeSignupTracker(),

		SignupVerifier:      noopSignupVerifier{},
		SignupVerifyEmailer: failingVerifyEmailer{},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/signup", "application/json",
		strings.NewReader(`{"email":"`+email+`","label":"mask-test"}`))
	if err != nil {
		t.Fatalf("POST /v1/signup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	logged := buf.String()
	if !strings.Contains(logged, "signup verification: send failed") {
		t.Fatalf("send-failure line never logged; the test never exercised the path.\nlog:\n%s", logged)
	}
	if strings.Contains(logged, email) {
		t.Errorf("plaintext customer email %q leaked into the application log.\nlog:\n%s", email, logged)
	}
	if strings.Contains(logged, "alice.mcallister") {
		t.Errorf("email local part leaked into the application log.\nlog:\n%s", logged)
	}
	if !strings.Contains(logged, `a***@example.com`) {
		t.Errorf("masked address %q absent — the log must stay correlatable to a domain.\nlog:\n%s",
			"a***@example.com", logged)
	}
}
