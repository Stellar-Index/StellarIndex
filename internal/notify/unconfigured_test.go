package notify_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

func wellFormedMessage() notify.Message {
	return notify.Message{
		From:    "Stellar Index <hello@stellarindex.io>",
		To:      []string{"alice@example.com"},
		Subject: "Sign in",
		Text:    "link",
	}
}

// RLT-321: a transport with no credential is an ERROR on every Send — never
// the nil that the old NoopSender fallback returned and callers counted as
// result="sent".
func TestUnconfiguredSender_SendIsAlwaysErrNotConfigured(t *testing.T) {
	err := notify.UnconfiguredSender{Reason: "env STELLARINDEX_RESEND_API_KEY is unset/empty"}.
		Send(context.Background(), wellFormedMessage())
	if !errors.Is(err, notify.ErrNotConfigured) {
		t.Fatalf("Send error = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "STELLARINDEX_RESEND_API_KEY is unset/empty") {
		t.Errorf("error does not carry the operator-facing reason: %v", err)
	}
	if err := (notify.UnconfiguredSender{}).Send(context.Background(), wellFormedMessage()); !errors.Is(err, notify.ErrNotConfigured) {
		t.Errorf("zero-value Send error = %v, want ErrNotConfigured", err)
	}
}

func TestIsUnconfigured(t *testing.T) {
	configured, err := notify.NewResendSender("re_test_token")
	if err != nil {
		t.Fatalf("NewResendSender: %v", err)
	}
	var typedNil *notify.ResendSender
	for _, tc := range []struct {
		name   string
		sender notify.Sender
		want   bool
	}{
		{"nil interface", nil, true},
		{"UnconfiguredSender", notify.UnconfiguredSender{}, true},
		{"ResendSender literal without a key", &notify.ResendSender{}, true},
		{"ResendSender literal with a blank key", &notify.ResendSender{APIKey: " \n"}, true},
		{"typed-nil ResendSender", typedNil, true},
		{"ResendSender from the constructor", configured, false},
		// The recording test double is a deliberate choice by whoever
		// constructs it, not a missing credential.
		{"NoopSender", &notify.NoopSender{}, false},
	} {
		if got := notify.IsUnconfigured(tc.sender); got != tc.want {
			t.Errorf("IsUnconfigured(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
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

func TestNewResendSender_TrimsAndRejectsBlank(t *testing.T) {
	if _, err := notify.NewResendSender(" \t\n"); !errors.Is(err, notify.ErrNotConfigured) {
		t.Errorf("blank-only key error = %v, want ErrNotConfigured", err)
	}
	s, err := notify.NewResendSender("  re_test_token\n")
	if err != nil {
		t.Fatalf("NewResendSender: %v", err)
	}
	if s.APIKey != "re_test_token" {
		t.Errorf("key was not trimmed; a trailing newline corrupts the Authorization header")
	}
}
