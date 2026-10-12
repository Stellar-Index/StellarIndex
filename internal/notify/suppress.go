package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrSuppressed is returned by SuppressingSender when every send was withheld because a recipient is on the suppression list; count it as `suppressed`, not `failed`, since no provider call was made.
var ErrSuppressed = errors.New("notify: recipient suppressed")

// Checker reports whether a canonical recipient must not be mailed.
type Checker interface {
	IsSuppressed(ctx context.Context, canonical string) (bool, error)
}

// HashSetChecker suppresses recipients by the hex SHA-256 of their CanonicalRecipient form.
type HashSetChecker map[string]struct{}

// NewHashSetChecker validates 64-char hex digests (case-insensitive) and returns the checker; an empty list suppresses nothing.
func NewHashSetChecker(hashes []string) (HashSetChecker, error) {
	set := make(HashSetChecker, len(hashes))
	for _, h := range hashes {
		h = strings.ToLower(strings.TrimSpace(h))
		if b, err := hex.DecodeString(h); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("notify: suppression entry is not a 64-char hex SHA-256")
		}
		set[h] = struct{}{}
	}
	return set, nil
}

// IsSuppressed implements [Checker].
func (c HashSetChecker) IsSuppressed(_ context.Context, canonical string) (bool, error) {
	_, ok := c[RecipientHash(canonical)]
	return ok, nil
}

// RecipientHash is the suppression-list digest of a [CanonicalRecipient] address.
func RecipientHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// resendFeedbackEvent is the subset of a Resend webhook event that decides suppression.
type resendFeedbackEvent struct {
	Type string `json:"type"`
	Data struct {
		To     []string `json:"to"`
		Bounce struct {
			Type string `json:"type"`
		} `json:"bounce"`
	} `json:"data"`
}

// FeedbackHashes returns the suppression digests one Resend webhook event adds: every recipient of
// email.complained or email.bounced (Resend's permanent-rejection event) unless the bounce is
// marked Temporary; any other event adds none. The caller must
// verify the webhook signature first, or anyone could suppress any address.
func FeedbackHashes(payload []byte) ([]string, error) {
	var ev resendFeedbackEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, fmt.Errorf("notify: feedback event: %w", err)
	}
	switch {
	case ev.Type == "email.complained":
	case ev.Type == "email.bounced" && ev.Data.Bounce.Type != "Temporary":
	default:
		return nil, nil
	}
	out := make([]string, 0, len(ev.Data.To))
	for _, raw := range ev.Data.To {
		canon, err := CanonicalRecipient(raw)
		if err != nil {
			return nil, fmt.Errorf("notify: feedback event recipient: %w", err)
		}
		out = append(out, RecipientHash(canon))
	}
	return out, nil
}

// SuppressingSender withholds a message whose To list contains a suppressed recipient; everything else passes to Next untouched.
type SuppressingSender struct {
	Next    Sender
	Checker Checker
}

// Send validates, checks every recipient (fail closed on checker error), then forwards; a suppressed message returns ErrSuppressed.
func (s SuppressingSender) Send(ctx context.Context, msg Message) error {
	if err := validate(msg); err != nil {
		return err
	}
	for _, to := range msg.To {
		hit, err := s.Checker.IsSuppressed(ctx, to)
		if err != nil {
			return fmt.Errorf("notify: suppression check: %w", err)
		}
		if hit {
			return ErrSuppressed
		}
	}
	return s.Next.Send(ctx, msg)
}

// MailConfigured forwards to Next so [IsUnconfigured] still sees through.
func (s SuppressingSender) MailConfigured() bool { return !IsUnconfigured(s.Next) }

// WithSuppression wraps next so recipients whose hashes are listed are never mailed; an empty list returns next unchanged.
func WithSuppression(next Sender, hashes []string) (Sender, error) {
	if len(hashes) == 0 {
		return next, nil
	}
	c, err := NewHashSetChecker(hashes)
	if err != nil {
		return nil, err
	}
	return SuppressingSender{Next: next, Checker: c}, nil
}
