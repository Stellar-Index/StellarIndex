package notify_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

func TestValidate_HappyPath(t *testing.T) {
	n := &notify.NoopSender{}
	err := n.Send(context.Background(), notify.Message{
		From:    "Stellar Index <hello@stellarindex.io>",
		To:      []string{"alice@example.com"},
		Subject: "Test",
		Text:    "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n.SentCount() != 1 {
		t.Errorf("SentCount = %d, want 1", n.SentCount())
	}
}

func TestValidate_RejectsEmptyMessage(t *testing.T) {
	n := &notify.NoopSender{}
	cases := []struct {
		name string
		msg  notify.Message
	}{
		{"empty To", notify.Message{From: "x@y.com", Subject: "s", Text: "t"}},
		{"empty Subject", notify.Message{From: "x@y.com", To: []string{"a@b.com"}, Text: "t"}},
		{"empty body", notify.Message{From: "x@y.com", To: []string{"a@b.com"}, Subject: "s"}},
		{"empty From", notify.Message{To: []string{"a@b.com"}, Subject: "s", Text: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := n.Send(context.Background(), tc.msg)
			if !errors.Is(err, notify.ErrInvalidMessage) {
				t.Errorf("expected ErrInvalidMessage, got %v", err)
			}
		})
	}
}

func TestCanonicalRecipient(t *testing.T) {
	ok := map[string]string{
		"alice@example.com":                "alice@example.com",
		"  Alice@Example.COM ":             "alice@example.com",
		"<alice@example.com>":              "alice@example.com",
		`"Support" <Alice@Example.com>`:    "alice@example.com",
		"Bob <bob+tag@mail.example.co.uk>": "bob+tag@mail.example.co.uk",
	}
	for in, want := range ok {
		got, err := notify.CanonicalRecipient(in)
		if err != nil || got != want {
			t.Errorf("CanonicalRecipient(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "abc", "@b.co", "a@", "a@b",
		"a@b.com,c@d.com",
		`"a b"@example.com`,
		strings.Repeat("a", 256) + "@b.co",
	} {
		if got, err := notify.CanonicalRecipient(in); !errors.Is(err, notify.ErrInvalidRecipient) {
			t.Errorf("CanonicalRecipient(%q) = %q, %v; want ErrInvalidRecipient", in, got, err)
		}
	}
}

// The mail layer is the chokepoint: a To element in any spelling other
// than the canonical one is refused before the wire, so no caller can
// store one identity for an inbox and mail another.
func TestValidate_RejectsNonCanonicalRecipient(t *testing.T) {
	n := &notify.NoopSender{}
	for _, to := range []string{
		`"Support" <victim@example.com>`,
		"Victim@Example.com",
		"a@b.com,c@d.com",
	} {
		err := n.Send(context.Background(), notify.Message{
			From: "x@y.com", To: []string{to}, Subject: "s", Text: "t",
		})
		if !errors.Is(err, notify.ErrInvalidMessage) {
			t.Errorf("To=%q: expected ErrInvalidMessage, got %v", to, err)
		}
	}
	if n.SentCount() != 0 {
		t.Errorf("SentCount = %d, want 0", n.SentCount())
	}
}

func TestMagicLinkMessage_Renders(t *testing.T) {
	msg, err := notify.MagicLinkMessage(
		"Stellar Index <hello@stellarindex.io>",
		"alice@example.com",
		notify.MagicLinkInput{
			LinkURL:          "https://app.stellarindex.io/auth/callback?token=abc",
			Code:             "123456",
			ExpiresInMinutes: 15,
			IPAddress:        "203.0.113.1",
			UserAgent:        notify.ClientFromUserAgent("Mozilla/5.0 (Macintosh) Firefox/123"),
		},
	)
	if err != nil {
		t.Fatalf("MagicLinkMessage: %v", err)
	}
	if msg.Subject == "" || msg.HTML == "" || msg.Text == "" {
		t.Fatalf("required fields not populated: %+v", msg)
	}
	// Spot-check that the dynamic fields landed in the rendered
	// bodies — guards against a refactor that drops a field
	// from the template.
	for _, want := range []string{"https://app.stellarindex.io/auth/callback?token=abc", "123456", "15 minutes", "203.0.113.1", "(Firefox on macOS)"} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("HTML missing %q", want)
		}
		if !strings.Contains(msg.Text, want) {
			t.Errorf("Text missing %q", want)
		}
	}
	if tag, ok := msg.Tags["template"]; !ok || tag != "magic-link" {
		t.Errorf("tag template not set: %v", msg.Tags)
	}
}

func TestClientFromUserAgent(t *testing.T) {
	cases := []struct{ ua, want string }{
		{"", ""},
		{"   ", ""},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0", "Edge on Windows"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15", "Safari on macOS"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0 Mobile/15E148 Safari/604.1", "Chrome on iOS"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Mobile Safari/537.36", "Chrome on Android"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0", "Firefox on Linux"},
		{"curl/8.7.1", "an unrecognised browser"},
		{"Windows support: call +1-555-0100 now", "an unrecognised browser on Windows"},
	}
	for _, tc := range cases {
		if got := notify.ClientFromUserAgent(tc.ua).String(); got != tc.want {
			t.Errorf("ClientFromUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

// TestMagicLinkMessage_StatesSameBrowserConstraint: the callback
// 403s without the stellarindex_login_intent cookie set by the requesting
// browser, so the mail must say the link is browser-bound and point other
// devices at the code (HandleVerifyCode does not need the cookie).
func TestMagicLinkMessage_StatesSameBrowserConstraint(t *testing.T) {
	msg, err := notify.MagicLinkMessage(
		"Stellar Index <hello@stellarindex.io>",
		"alice@example.com",
		notify.MagicLinkInput{
			LinkURL:          "https://app.stellarindex.io/auth/callback?token=abc",
			Code:             "123456",
			ExpiresInMinutes: 15,
		},
	)
	if err != nil {
		t.Fatalf("MagicLinkMessage: %v", err)
	}
	for _, want := range []string{"only works in the browser where", "on any other device, enter the code instead"} {
		if !strings.Contains(strings.ReplaceAll(msg.HTML, "\n", " "), want) {
			t.Errorf("HTML missing %q", want)
		}
		if !strings.Contains(strings.ReplaceAll(msg.Text, "\n", " "), want) {
			t.Errorf("Text missing %q", want)
		}
	}
	if strings.Contains(msg.HTML, "Or just click to sign in") {
		t.Error("HTML still offers the link unconditionally")
	}
}

func TestResendSender_HappyPath(t *testing.T) {
	var receivedAuth, receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"em_123"}`))
	}))
	defer srv.Close()

	s, err := notify.NewResendSender("re_test_token")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	s.BaseURL = srv.URL

	err = s.Send(context.Background(), notify.Message{
		From:    "Stellar Index <hello@stellarindex.io>",
		To:      []string{"alice@example.com"},
		Subject: "Test",
		Text:    "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if receivedAuth != "Bearer re_test_token" {
		t.Errorf("Authorization header = %q", receivedAuth)
	}
	if !strings.Contains(receivedBody, `"to":["alice@example.com"]`) {
		t.Errorf("body missing To: %s", receivedBody)
	}
}

func TestResendSender_4xxIsProviderRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"name":"validation_error","message":"unverified domain","statusCode":422}`))
	}))
	defer srv.Close()

	s, _ := notify.NewResendSender("re_test")
	s.BaseURL = srv.URL

	err := s.Send(context.Background(), notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
	if !errors.Is(err, notify.ErrProviderRejected) {
		t.Errorf("expected ErrProviderRejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "unverified domain") {
		t.Errorf("err missing detail: %v", err)
	}
}

func TestResendSender_5xxIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s, _ := notify.NewResendSender("re_test")
	s.BaseURL = srv.URL

	err := s.Send(context.Background(), notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
	if !errors.Is(err, notify.ErrTransient) {
		t.Errorf("expected ErrTransient, got %v", err)
	}
}

func TestResendSender_429IsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"name":"rate_limit_exceeded","message":"too many requests","statusCode":429}`))
	}))
	defer srv.Close()

	s, _ := notify.NewResendSender("re_test")
	s.BaseURL = srv.URL

	err := s.Send(context.Background(), notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
	if !errors.Is(err, notify.ErrTransient) {
		t.Errorf("expected ErrTransient, got %v", err)
	}
	if errors.Is(err, notify.ErrProviderRejected) {
		t.Errorf("429 must not be ErrProviderRejected: %v", err)
	}
}

// statusSequenceServer answers each POST with the next status in seq
// (repeating the last) and counts the attempts.
func statusSequenceServer(t *testing.T, seq ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(hits.Add(1))
		w.WriteHeader(seq[min(n, len(seq))-1])
		_, _ = w.Write([]byte(`{"message":"m"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func sendTo(ctx context.Context, t *testing.T, url string) error {
	t.Helper()
	s, err := notify.NewResendSender("re_test")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	s.BaseURL = url
	return s.Send(ctx, notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
}

func TestResendSender_RetriesRateLimitThenDelivers(t *testing.T) {
	srv, hits := statusSequenceServer(t, http.StatusTooManyRequests, http.StatusOK)
	if err := sendTo(context.Background(), t, srv.URL); err != nil {
		t.Fatalf("Send = %v, want nil after one 429 then 200", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one retry, one delivery)", got)
	}
}

func TestResendSender_ProviderRejectedIsNotRetried(t *testing.T) {
	srv, hits := statusSequenceServer(t, http.StatusUnprocessableEntity, http.StatusOK)
	err := sendTo(context.Background(), t, srv.URL)
	if !errors.Is(err, notify.ErrProviderRejected) {
		t.Fatalf("Send = %v, want ErrProviderRejected", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (a rejection is final)", got)
	}
}

func TestResendSender_TransientRetriesAreBounded(t *testing.T) {
	srv, hits := statusSequenceServer(t, http.StatusServiceUnavailable)
	if err := sendTo(context.Background(), t, srv.URL); !errors.Is(err, notify.ErrTransient) {
		t.Fatalf("Send = %v, want ErrTransient", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestResendSender_CancelledCallerStopsRetries(t *testing.T) {
	srv, hits := statusSequenceServer(t, http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sendTo(ctx, t, srv.URL); !errors.Is(err, notify.ErrTransient) {
		t.Fatalf("Send = %v, want ErrTransient", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry after caller cancel)", got)
	}
}

// A caller whose request context is cancelled while the provider POST
// is in flight (the client hung up after the token row was written)
// must not abort the delivery: the send completes and reports success.
func TestResendSender_CallerCancelDoesNotAbortSend(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var delivered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		if r.Context().Err() != nil {
			return
		}
		delivered.Store(true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"em_123"}`))
	}))
	defer srv.Close()

	s, err := notify.NewResendSender("re_test")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	s.BaseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.Send(ctx, notify.Message{
			From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
		})
	}()
	<-arrived
	cancel()
	time.Sleep(50 * time.Millisecond) // let a cancellation reach the transport
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Send after caller cancel = %v, want nil (delivery must complete)", err)
	}
	if !delivered.Load() {
		t.Fatal("provider never saw a live request: the POST was aborted")
	}
}

func TestResendSender_RequiresAPIKey(t *testing.T) {
	if _, err := notify.NewResendSender(""); err == nil {
		t.Error("expected error for empty API key")
	}
}

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestResendSender_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	s, err := notify.NewResendSender("probe")
	if err != nil {
		t.Fatal(err)
	}
	s.BaseURL = trap.URL
	err = s.Send(context.Background(), notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
	if err == nil {
		t.Error("Send succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}

// A ResendSender built around the constructor must not put a blank bearer on
// the wire and must not report success whatever the far end answers.
func TestResendSender_NoKey_FailsBeforeTheWire(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK) // a permissive far end must not turn this into "sent"
	}))
	defer srv.Close()

	s := &notify.ResendSender{Client: srv.Client(), BaseURL: srv.URL}
	err := s.Send(context.Background(), wellFormedMessage())
	if !errors.Is(err, notify.ErrNotConfigured) {
		t.Errorf("Send error = %v, want ErrNotConfigured", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("provider was called %d time(s) with no key", n)
	}
}
