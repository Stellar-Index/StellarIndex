package notify

import (
	"context"
	"errors"
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
