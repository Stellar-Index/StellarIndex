package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisSignupVerifier_RoundTrip — Reserve + Consume return
// the keyID; second Consume returns ErrSignupVerifyNotFound
// (single-use semantics via GETDEL). F-1218 (codex audit-2026-05-12).
func TestRedisSignupVerifier_RoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	const tok = "tok_first_round_trip"
	const keyID = "kid_abc123"

	if err := v.Reserve(ctx, tok, keyID, 10*time.Minute); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	got, err := v.Consume(ctx, tok)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got != keyID {
		t.Errorf("Consume = %q, want %q", got, keyID)
	}

	// Second Consume must fail — single-use.
	_, err = v.Consume(ctx, tok)
	if !errors.Is(err, ErrSignupVerifyNotFound) {
		t.Errorf("second Consume err = %v, want ErrSignupVerifyNotFound", err)
	}
}

// TestRedisSignupVerifier_TTLExpires — after the TTL elapses
// without consumption, the token is gone (Reserve TTL is the
// safety net for an email that's never opened).
func TestRedisSignupVerifier_TTLExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	if err := v.Reserve(ctx, "tok_ttl", "kid_x", time.Second); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	mr.FastForward(2 * time.Second)
	if _, err := v.Consume(ctx, "tok_ttl"); !errors.Is(err, ErrSignupVerifyNotFound) {
		t.Errorf("post-TTL Consume err = %v, want ErrSignupVerifyNotFound", err)
	}
}

// TestRedisSignupVerifier_IdempotentReserve — the same (token,
// keyID) pair can be re-Reserved without error, useful when a
// network blip causes the email-send to retry.
func TestRedisSignupVerifier_IdempotentReserve(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	const tok = "tok_idem"
	const keyID = "kid_idem"
	if err := v.Reserve(ctx, tok, keyID, 10*time.Minute); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	if err := v.Reserve(ctx, tok, keyID, 10*time.Minute); err != nil {
		t.Errorf("idempotent re-Reserve: %v", err)
	}
}

// TestRedisSignupVerifier_DifferentKeyIDForSameTokenRejected —
// two distinct keyIDs racing the same plaintext token must
// surface the conflict. (Practically unreachable with crypto/rand
// 32-byte tokens; defence-in-depth.)
func TestRedisSignupVerifier_DifferentKeyIDForSameTokenRejected(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	if err := v.Reserve(ctx, "tok_collision", "kid_first", 10*time.Minute); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	err := v.Reserve(ctx, "tok_collision", "kid_second", 10*time.Minute)
	if !errors.Is(err, ErrSignupVerifyReserved) {
		t.Errorf("second Reserve (different keyID) err = %v, want ErrSignupVerifyReserved", err)
	}
}

// TestRedisSignupVerifier_EmptyInputs — Reserve rejects empty
// token or keyID; Consume returns ErrSignupVerifyNotFound on
// empty.
func TestRedisSignupVerifier_EmptyInputs(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	if err := v.Reserve(ctx, "", "kid", 0); err == nil {
		t.Error("Reserve(\"\", kid) returned nil; want error")
	}
	if err := v.Reserve(ctx, "tok", "", 0); err == nil {
		t.Error("Reserve(tok, \"\") returned nil; want error")
	}
	if _, err := v.Consume(ctx, ""); !errors.Is(err, ErrSignupVerifyNotFound) {
		t.Errorf("Consume(\"\") = %v, want ErrSignupVerifyNotFound", err)
	}
}

// TestNewSignupVerifyToken_Unique — successive calls return
// distinct hex strings.
func TestNewSignupVerifyToken_Unique(t *testing.T) {
	a, err := NewSignupVerifyToken()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := NewSignupVerifyToken()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a == b {
		t.Errorf("two crypto/rand tokens collided: %q == %q", a, b)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Errorf("token lengths = (%d, %d), want both 64", len(a), len(b))
	}
}
