package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ErrSuppressed is returned by [SuppressingSender] when every send was
// withheld because a recipient is on the suppression list. Callers count
// it as `suppressed`, not `failed`: no provider call was made.
var ErrSuppressed = errors.New("notify: recipient suppressed")

// Checker reports whether a canonical recipient must not be mailed.
// A webhook-fed table can implement it later without touching callers.
type Checker interface {
	IsSuppressed(ctx context.Context, canonical string) (bool, error)
}

// HashSetChecker suppresses recipients by the hex SHA-256 of their
// [CanonicalRecipient] form.
type HashSetChecker map[string]struct{}

// NewHashSetChecker validates 64-char hex digests (case-insensitive) and
// returns the checker. An empty list suppresses nothing.
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
	sum := sha256.Sum256([]byte(canonical))
	_, ok := c[hex.EncodeToString(sum[:])]
	return ok, nil
}

// SuppressingSender withholds a message whose To list contains a
// suppressed recipient; everything else passes to Next untouched.
type SuppressingSender struct {
	Next    Sender
	Checker Checker
}

// Send validates, checks every recipient (fail closed on checker error),
// then forwards. A suppressed message returns ErrSuppressed.
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

// WithSuppression wraps next so recipients whose hashes are listed are
// never mailed. An empty list returns next unchanged.
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
