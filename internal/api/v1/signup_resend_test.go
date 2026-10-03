package v1_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

type resendHarness struct {
	ts       *httptest.Server
	verifier *auth.RedisSignupVerifier
	emailer  *fakeSignupVerifyEmailer
}

func newResendHarness(t *testing.T, throttle v1.SignupResendThrottle) *resendHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tracker := newFakeSignupTracker()
	sum := sha256.Sum256([]byte("known@example.com"))
	tracker.store[hex.EncodeToString(sum[:])] = "kid_known"

	verifier := auth.NewRedisSignupVerifier(rdb)
	if throttle == nil {
		throttle = auth.NewRedisSignupResendThrottle(rdb)
	}
	emailer := &fakeSignupVerifyEmailer{}
	srv := v1.New(v1.Options{
		Auth:                 fakeAuthMiddleware(auth.Subject{}),
		Signups:              tracker,
		SignupVerifier:       verifier,
		SignupVerifyEmailer:  emailer,
		SignupVerifyBaseURL:  "https://api.stellarindex.io/v1",
		SignupResendThrottle: throttle,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &resendHarness{ts: ts, verifier: verifier, emailer: emailer}
}

func (h *resendHarness) post(t *testing.T, email string) (int, string) {
	t.Helper()
	body := `{"email":"` + email + `"}`
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/signup/resend-verification", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *resendHarness) sentCount() int {
	h.emailer.mu.Lock()
	defer h.emailer.mu.Unlock()
	return len(h.emailer.sends)
}

func (h *resendHarness) waitSent(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.sentCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("sent %d mails, want %d", h.sentCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *resendHarness) lastToken(t *testing.T) string {
	t.Helper()
	h.emailer.mu.Lock()
	defer h.emailer.mu.Unlock()
	u, err := url.Parse(h.emailer.sends[len(h.emailer.sends)-1].verifyURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

// TestSignupResend_IdenticalResponseKnownAndUnknown — the reply must not
// reveal whether the address has an account.
func TestSignupResend_IdenticalResponseKnownAndUnknown(t *testing.T) {
	h := newResendHarness(t, nil)
	knownStatus, knownBody := h.post(t, "Known@Example.com")
	unknownStatus, unknownBody := h.post(t, "nobody@example.com")
	if knownStatus != http.StatusOK || unknownStatus != knownStatus {
		t.Fatalf("status known=%d unknown=%d, want both 200", knownStatus, unknownStatus)
	}
	norm := func(s string) string {
		// as_of differs by clock; compare the data member only.
		i := strings.Index(s, `"data"`)
		j := strings.Index(s, `"as_of"`)
		if i < 0 || j < i {
			return s
		}
		return s[i:j]
	}
	if norm(knownBody) != norm(unknownBody) {
		t.Errorf("bodies differ:\nknown:   %s\nunknown: %s", knownBody, unknownBody)
	}
	h.waitSent(t, 1)
	time.Sleep(50 * time.Millisecond)
	if got := h.sentCount(); got != 1 {
		t.Errorf("mails sent = %d, want exactly 1 (known address only)", got)
	}
}

// TestSignupResend_InvalidatesPriorToken — a resend retires the earlier
// link and the new one verifies the key.
func TestSignupResend_InvalidatesPriorToken(t *testing.T) {
	h := newResendHarness(t, nil)
	ctx := context.Background()
	if err := h.verifier.Reserve(ctx, "tok_from_signup", "kid_known", time.Hour); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, "known@example.com"); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	h.waitSent(t, 1)
	fresh := h.lastToken(t)
	if fresh == "" || fresh == "tok_from_signup" {
		t.Fatalf("fresh token = %q", fresh)
	}
	if _, err := h.verifier.Consume(ctx, "tok_from_signup"); !errors.Is(err, auth.ErrSignupVerifyNotFound) {
		t.Errorf("old token err = %v, want ErrSignupVerifyNotFound", err)
	}
	if got, err := h.verifier.Consume(ctx, fresh); err != nil || got != "kid_known" {
		t.Errorf("fresh token = %q, %v; want kid_known", got, err)
	}
}

// TestSignupResend_RateLimitedPerAddress — the cap counts unknown
// addresses too and answers 429 with Retry-After.
func TestSignupResend_RateLimitedPerAddress(t *testing.T) {
	h := newResendHarness(t, nil)
	for i := 0; i < 3; i++ {
		if status, _ := h.post(t, "nobody@example.com"); status != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200", i, status)
		}
	}
	if status, _ := h.post(t, "nobody@example.com"); status != http.StatusTooManyRequests {
		t.Errorf("4th call status = %d, want 429", status)
	}
}

type failingResendThrottle struct{}

func (failingResendThrottle) Allow(context.Context, string, string) error {
	return errors.New("redis down")
}

// TestSignupResend_ThrottleOutageFailsClosed — no mail when the budget
// cannot be consulted.
func TestSignupResend_ThrottleOutageFailsClosed(t *testing.T) {
	h := newResendHarness(t, failingResendThrottle{})
	if status, _ := h.post(t, "known@example.com"); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.sentCount(); got != 0 {
		t.Errorf("mails sent = %d, want 0", got)
	}
}
