package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

type recSender struct{ n int }

func (r *recSender) Send(context.Context, Message) error { r.n++; return nil }

type errChecker struct{}

func (errChecker) IsSuppressed(context.Context, string) (bool, error) {
	return false, errors.New("boom")
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func msgTo(to ...string) Message {
	return Message{From: "Stellar Index <hello@stellarindex.io>", To: to, Subject: "s", Text: "t"}
}

func newSuppressor(t *testing.T, next Sender, hashes ...string) SuppressingSender {
	t.Helper()
	c, err := NewHashSetChecker(hashes)
	if err != nil {
		t.Fatal(err)
	}
	return SuppressingSender{Next: next, Checker: c}
}

func TestSuppressingSender_SuppressedNeverReachesInner(t *testing.T) {
	inner := &recSender{}
	s := newSuppressor(t, inner, hashOf("bounced@example.com"))
	if err := s.Send(context.Background(), msgTo("bounced@example.com")); !errors.Is(err, ErrSuppressed) {
		t.Fatalf("err = %v, want ErrSuppressed", err)
	}
	if inner.n != 0 {
		t.Fatalf("inner called %d times", inner.n)
	}
}

func TestSuppressingSender_OthersPassThrough(t *testing.T) {
	inner := &recSender{}
	s := newSuppressor(t, inner, hashOf("bounced@example.com"))
	if err := s.Send(context.Background(), msgTo("ok@example.com")); err != nil {
		t.Fatal(err)
	}
	if inner.n != 1 {
		t.Fatalf("inner called %d times, want 1", inner.n)
	}
}

func TestSuppressingSender_MatchCaseInsensitiveHexAndAddress(t *testing.T) {
	inner := &recSender{}
	// Upper-case digest in config; the address is canonicalised before hashing.
	s := newSuppressor(t, inner, " "+upper(hashOf("bounced@example.com"))+" ")
	if err := s.Send(context.Background(), msgTo("bounced@example.com")); !errors.Is(err, ErrSuppressed) {
		t.Fatalf("err = %v, want ErrSuppressed", err)
	}
	if err := s.Send(context.Background(), msgTo("Bounced@Example.com")); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("non-canonical To must stay invalid, got %v", err)
	}
	c, _ := NewHashSetChecker([]string{hashOf("a@b.com")})
	if canon, _ := CanonicalRecipient("  A@B.com "); canon != "a@b.com" {
		t.Fatal("canonical form changed")
	} else if hit, _ := c.IsSuppressed(context.Background(), canon); !hit {
		t.Fatal("mixed-case input must hash as its canonical form")
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestSuppressingSender_EmptyListSuppressesNothing(t *testing.T) {
	inner := &recSender{}
	s := newSuppressor(t, inner)
	if err := s.Send(context.Background(), msgTo("a@example.com")); err != nil || inner.n != 1 {
		t.Fatalf("err=%v n=%d", err, inner.n)
	}
}

func TestSuppressingSender_AnyRecipientHitSendsNothing(t *testing.T) {
	inner := &recSender{}
	s := newSuppressor(t, inner, hashOf("b@example.com"))
	if err := s.Send(context.Background(), msgTo("a@example.com", "b@example.com")); !errors.Is(err, ErrSuppressed) {
		t.Fatalf("err = %v", err)
	}
	if inner.n != 0 {
		t.Fatal("inner reached")
	}
}

func TestSuppressingSender_CheckerErrorFailsClosedAsFailure(t *testing.T) {
	inner := &recSender{}
	err := SuppressingSender{Next: inner, Checker: errChecker{}}.Send(context.Background(), msgTo("a@example.com"))
	if err == nil || errors.Is(err, ErrSuppressed) || errors.Is(err, ErrTransient) || inner.n != 0 {
		t.Fatalf("err=%v n=%d", err, inner.n)
	}
}

func TestSuppressingSender_ForwardsMailConfigured(t *testing.T) {
	s := newSuppressor(t, UnconfiguredSender{Reason: "x"})
	if !IsUnconfigured(s) {
		t.Fatal("wrapper hid an unconfigured transport")
	}
	if IsUnconfigured(newSuppressor(t, &recSender{})) {
		t.Fatal("wrapper over a plain sender reads unconfigured")
	}
}

func TestNewHashSetChecker_RejectsBadDigest(t *testing.T) {
	for _, bad := range []string{"", "abc", "zz" + hashOf("x")[2:], "a@b.com"} {
		if _, err := NewHashSetChecker([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
