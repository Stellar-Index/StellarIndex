package notify

import (
	"context"
	"errors"
	"fmt"
)

// Message is the transactional-email envelope every Sender
// accepts. To/From are addresses ("Name <addr@example>" or
// just "addr@example"); HTML and Text are both honoured by
// Resend (multipart) — when only one is set the other is
// auto-derived where supported.
type Message struct {
	From    string
	To      []string
	Subject string
	HTML    string
	Text    string
	// Tags surface in the provider dashboard for filtering and
	// per-template metric breakdowns. Resend supports up to 10
	// key/value tags per send; keep the keys short.
	Tags map[string]string
	// IdempotencyKey lets the caller dedupe retries without
	// double-sending. Resend honours this on its API; for
	// providers that don't support it the Noop driver falls
	// back to caller-side caching.
	IdempotencyKey string
}

// Sender ships Messages. Concrete impls must be safe for
// concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ErrInvalidMessage is returned when the message fails
// pre-send validation (missing To, missing Subject, both HTML
// and Text empty). Wraps with %w so handler-side error mapping
// can errors.Is.
var ErrInvalidMessage = errors.New("notify: invalid message")

// ErrProviderRejected is returned when the upstream provider
// rejected the message — bad From, bad recipient, unverified
// domain. Distinguishable from transient delivery failures so
// the caller can choose between "log + drop" and "retry".
var ErrProviderRejected = errors.New("notify: provider rejected")

// ErrTransient indicates a 5xx / network error from the
// provider. Caller may retry.
var ErrTransient = errors.New("notify: transient provider failure")

// ErrNotConfigured is returned by a transport that holds no provider
// credential and therefore cannot deliver anything. It is a FAILURE,
// never a success: a caller that counts sends must count it as
// failed, and a caller that reports delivery must not report "sent"
// (RLT-321 — an empty Resend key used to resolve to a transport whose
// Send returned nil, so undeliverable sign-in mail was counted and
// reported as sent and the failure-ratio alert read 0).
var ErrNotConfigured = errors.New("notify: mail transport is not configured")

// UnconfiguredSender is the transport a deployment gets when it has
// no provider credential. Every Send fails with ErrNotConfigured, so
// the absence of mail is an error at every call site rather than a
// silent drop. Wire this — not [NoopSender], which records and
// reports success — whenever production config lacks the credential.
type UnconfiguredSender struct {
	// Reason names WHAT is missing, for the operator (e.g. the env
	// var that is unset). It is appended to the error, so it must
	// never carry a credential value or any part of one.
	Reason string
}

// Send always fails with ErrNotConfigured.
func (u UnconfiguredSender) Send(context.Context, Message) error {
	if u.Reason == "" {
		return ErrNotConfigured
	}
	return fmt.Errorf("%w: %s", ErrNotConfigured, u.Reason)
}

// MailConfigured reports false: there is no credential to send with.
func (UnconfiguredSender) MailConfigured() bool { return false }

// IsUnconfigured reports whether s is known, before any Send, to be
// unable to deliver: nil, or a transport that declares (through an
// optional `MailConfigured() bool` method) that it holds no
// credential. Call sites use it to refuse up front — before minting
// a token or promising an email — instead of discovering the failure
// after the side effects. A Sender without the method is taken at its
// word; its Send error is the signal.
func IsUnconfigured(s Sender) bool {
	if s == nil {
		return true
	}
	if r, ok := s.(interface{ MailConfigured() bool }); ok {
		return !r.MailConfigured()
	}
	return false
}

// validate runs the common checks every concrete Sender does
// before hitting the wire. Centralised so the four error
// shapes (missing To, missing Subject, empty body, malformed
// From) stay consistent across drivers.
func validate(m Message) error {
	if len(m.To) == 0 {
		return errors.Join(ErrInvalidMessage, errors.New("recipient list is empty"))
	}
	if m.Subject == "" {
		return errors.Join(ErrInvalidMessage, errors.New("subject is empty"))
	}
	if m.HTML == "" && m.Text == "" {
		return errors.Join(ErrInvalidMessage, errors.New("html and text bodies are both empty"))
	}
	if m.From == "" {
		return errors.Join(ErrInvalidMessage, errors.New("from address is empty"))
	}
	return nil
}
