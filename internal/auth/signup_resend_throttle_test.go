package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newResendTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// TestRedisSignupResendThrottle_CapsPerAddressAndPerIP — the address cap
// holds across IPs, the IP cap holds across addresses.
func TestRedisSignupResendThrottle_CapsPerAddressAndPerIP(t *testing.T) {
	_, rdb := newResendTestRedis(t)
	th := NewRedisSignupResendThrottle(rdb)
	ctx := context.Background()

	for i := 0; i < signupResendMaxPerAddress; i++ {
		if err := th.Allow(ctx, fmt.Sprintf("203.0.113.%d", i+1), "addr-a"); err != nil {
			t.Fatalf("address call %d: %v", i, err)
		}
	}
	if err := th.Allow(ctx, "203.0.113.99", "addr-a"); !errors.Is(err, ErrSignupRateLimited) {
		t.Errorf("over address cap err = %v, want ErrSignupRateLimited", err)
	}

	for i := 0; i < signupResendMaxPerIP; i++ {
		if err := th.Allow(ctx, "198.51.100.7", fmt.Sprintf("addr-ip-%d", i)); err != nil {
			t.Fatalf("ip call %d: %v", i, err)
		}
	}
	if err := th.Allow(ctx, "198.51.100.7", "addr-fresh"); !errors.Is(err, ErrSignupRateLimited) {
		t.Errorf("over IP cap err = %v, want ErrSignupRateLimited", err)
	}
}

// TestRedisSignupResendThrottle_RedisDownFailsClosed — an unreachable
// store is an error, never a nil that would let mail through.
func TestRedisSignupResendThrottle_RedisDownFailsClosed(t *testing.T) {
	mr, rdb := newResendTestRedis(t)
	th := NewRedisSignupResendThrottle(rdb)
	mr.Close()
	err := th.Allow(context.Background(), "203.0.113.1", "addr-a")
	if err == nil || errors.Is(err, ErrSignupRateLimited) {
		t.Errorf("err = %v, want a non-sentinel error", err)
	}
}

// TestRedisSignupVerifier_ReserveRetiresPreviousToken — a second Reserve
// for the same keyID invalidates the first token; other keys are untouched.
func TestRedisSignupVerifier_ReserveRetiresPreviousToken(t *testing.T) {
	_, rdb := newResendTestRedis(t)
	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	for _, r := range []struct{ tok, key string }{{"tok_old", "kid_a"}, {"tok_other", "kid_b"}, {"tok_new", "kid_a"}} {
		if err := v.Reserve(ctx, r.tok, r.key, time.Hour); err != nil {
			t.Fatalf("Reserve %s: %v", r.tok, err)
		}
	}
	if _, err := v.Consume(ctx, "tok_old"); !errors.Is(err, ErrSignupVerifyNotFound) {
		t.Errorf("Consume(old) err = %v, want ErrSignupVerifyNotFound", err)
	}
	if got, err := v.Consume(ctx, "tok_new"); err != nil || got != "kid_a" {
		t.Errorf("Consume(new) = %q, %v; want kid_a", got, err)
	}
	if got, err := v.Consume(ctx, "tok_other"); err != nil || got != "kid_b" {
		t.Errorf("Consume(other key) = %q, %v; want kid_b", got, err)
	}
}

// TestRedisSignupVerifier_ReserveSameTokenKeepsItLive — the idempotent
// retry must not retire its own token.
func TestRedisSignupVerifier_ReserveSameTokenKeepsItLive(t *testing.T) {
	_, rdb := newResendTestRedis(t)
	v := NewRedisSignupVerifier(rdb)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := v.Reserve(ctx, "tok_same", "kid_a", time.Hour); err != nil {
			t.Fatalf("Reserve #%d: %v", i, err)
		}
	}
	if _, err := v.Consume(ctx, "tok_same"); err != nil {
		t.Errorf("Consume after retry: %v", err)
	}
}
