package notify

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Message is the transactional-email envelope every Sender
// accepts. From may carry a display name ("Name <addr@example>");
// each To element must be a bare [CanonicalRecipient] address,
// which validate enforces. HTML and Text are both honoured by
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

// maxRecipientLen is the RFC 5321 §4.5.3.1.3 path limit less the
// angle brackets.
const maxRecipientLen = 254

// ErrInvalidRecipient is returned by [CanonicalRecipient] for input
// that is not exactly one deliverable mailbox.
var ErrInvalidRecipient = errors.New("notify: invalid recipient address")

// CanonicalRecipient reduces raw user input to the one spelling of a
// single mailbox that every mail-keyed store and every Message.To uses:
// the lowercased RFC 5322 addr-spec, stripped of any display name or
// angle brackets. Without it `"x" <a@b.com>` and `a@b.com` are distinct
// accounts, signup identities and throttle keys for one inbox.
func CanonicalRecipient(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.ToLower(strings.TrimSpace(raw)))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidRecipient, err)
	}
	out := strings.ToLower(strings.TrimSpace(addr.Address))
	if len(out) > maxRecipientLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidRecipient, maxRecipientLen)
	}
	at := strings.LastIndexByte(out, '@')
	if at <= 0 || !strings.Contains(out[at+1:], ".") {
		return "", fmt.Errorf("%w: domain has no dot", ErrInvalidRecipient)
	}
	// A quoted local part (`"a b"@c.com`) unquotes to a string that no
	// longer parses; refuse it rather than store an unsendable spelling.
	if again, err := mail.ParseAddress(out); err != nil || again.Address != out {
		return "", fmt.Errorf("%w: address does not round-trip", ErrInvalidRecipient)
	}
	return out, nil
}

// validate runs the common checks every concrete Sender does
// before hitting the wire. Centralised so the four error
// shapes (missing To, missing Subject, empty body, malformed
// From) stay consistent across drivers. Every To element must
// already be in [CanonicalRecipient] form, so no caller can store
// one spelling of an inbox and mail another.
func validate(m Message) error {
	if len(m.To) == 0 {
		return errors.Join(ErrInvalidMessage, errors.New("recipient list is empty"))
	}
	for _, to := range m.To {
		if canon, err := CanonicalRecipient(to); err != nil || canon != to {
			return errors.Join(ErrInvalidMessage, errors.New("recipient is not a canonical single address"))
		}
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
