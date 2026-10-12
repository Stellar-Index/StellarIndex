package notify

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Message is the transactional-email envelope every Sender accepts; From may carry a display name, and each To element must be a bare CanonicalRecipient address, which validate enforces.
type Message struct {
	From    string
	To      []string
	Subject string
	HTML    string
	Text    string
	// Tags feed provider-dashboard filtering and per-template metrics. Resend allows 10 per send.
	Tags map[string]string
}

// Sender ships Messages; concrete implementations must be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ErrInvalidMessage is returned when a message fails pre-send validation, wrapped with %w so callers can map it with errors.Is.
var ErrInvalidMessage = errors.New("notify: invalid message")

// ErrProviderRejected is returned when the provider rejected the message (bad From, bad recipient, unverified domain); distinct from ErrTransient so callers drop rather than retry.
var ErrProviderRejected = errors.New("notify: provider rejected")

// ErrTransient indicates a 429 / 5xx / network error from the provider, after ResendSender has spent its bounded retries.
var ErrTransient = errors.New("notify: transient provider failure")

// ErrNotConfigured is returned by a transport with no provider credential; it is a FAILURE, never a success, since counting it as sent would hide undeliverable sign-in mail from the failure-ratio alert.
var ErrNotConfigured = errors.New("notify: mail transport is not configured")

// UnconfiguredSender is the transport for a deployment with no provider credential: every Send fails with ErrNotConfigured; wire this, not NoopSender (which reports success), whenever production config lacks the credential.
type UnconfiguredSender struct {
	// Reason names WHAT is missing (e.g. the unset env var). It is appended to the error, so it
	// must never carry a credential value or any part of one.
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

// IsUnconfigured reports whether s is known, before any Send, to be unable to deliver: nil, or a transport whose optional `MailConfigured() bool` returns false; a Sender without the method is taken at its word.
func IsUnconfigured(s Sender) bool {
	if s == nil {
		return true
	}
	if r, ok := s.(interface{ MailConfigured() bool }); ok {
		return !r.MailConfigured()
	}
	return false
}

// maxRecipientLen is the RFC 5321 §4.5.3.1.3 path limit less the angle brackets.
const maxRecipientLen = 254

// ErrInvalidRecipient is returned by CanonicalRecipient for input that is not exactly one deliverable mailbox.
var ErrInvalidRecipient = errors.New("notify: invalid recipient address")

// CanonicalRecipient reduces raw input to the one lowercased RFC 5322 addr-spec spelling every mail-keyed store and Message.To uses, so display-name and bare forms cannot become distinct accounts and throttle keys.
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
	// A quoted local part (`"a b"@c.com`) unquotes to a string that no longer parses; refuse it.
	if again, err := mail.ParseAddress(out); err != nil || again.Address != out {
		return "", fmt.Errorf("%w: address does not round-trip", ErrInvalidRecipient)
	}
	return out, nil
}

// validate runs the checks every Sender does before the wire, keeping the four error shapes
// (missing To, missing Subject, empty body, malformed From) consistent across drivers. Every To
// must already be in [CanonicalRecipient] form, so no caller stores one spelling and mails another.
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
