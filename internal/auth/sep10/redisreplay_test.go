package sep10_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
)

func newTestReplayGuard(t *testing.T) (*sep10.RedisReplayGuard, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return sep10.NewRedisReplayGuard(rdb), mr
}

// TestRedisReplayGuard_FirstClaimSucceeds_SecondReturnsUnauthorized
// pins F-1224: a reserved challenge hash can be claimed once; a second
// claim returns auth.ErrUnauthorized so Verify classifies it as a
// replay rather than issuing a fresh JWT.
func TestRedisReplayGuard_FirstClaimSucceeds_SecondReturnsUnauthorized(t *testing.T) {
	g, _ := newTestReplayGuard(t)
	ctx := context.Background()
	const txHash = "abcdef1234567890"

	if err := g.Reserve(ctx, txHash, 15*time.Minute); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := g.Claim(ctx, txHash); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := g.Claim(ctx, txHash); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("second claim: want auth.ErrUnauthorized, got %v", err)
	}
}

// TestRedisReplayGuard_AbsentReservationFailsClosed pins Q184: a claim
// whose reservation is missing — never made, evicted by allkeys-lru, or
// expired — is refused, never admitted.
func TestRedisReplayGuard_AbsentReservationFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("never reserved", func(t *testing.T) {
		g, _ := newTestReplayGuard(t)
		if err := g.Claim(ctx, "never-issued"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("want auth.ErrUnauthorized, got %v", err)
		}
	})
	t.Run("evicted", func(t *testing.T) {
		g, mr := newTestReplayGuard(t)
		if err := g.Reserve(ctx, "evicted", 15*time.Minute); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		mr.FlushAll()
		if err := g.Claim(ctx, "evicted"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("want auth.ErrUnauthorized, got %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		g, mr := newTestReplayGuard(t)
		if err := g.Reserve(ctx, "expired", 30*time.Second); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		mr.FastForward(31 * time.Second)
		if err := g.Claim(ctx, "expired"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("want auth.ErrUnauthorized, got %v", err)
		}
	})
}

// TestRedisReplayGuard_DistinctHashesIndependentSlots confirms two
// distinct challenge hashes don't collide.
func TestRedisReplayGuard_DistinctHashesIndependentSlots(t *testing.T) {
	g, _ := newTestReplayGuard(t)
	ctx := context.Background()

	for _, h := range []string{"hash-a", "hash-b"} {
		if err := g.Reserve(ctx, h, time.Minute); err != nil {
			t.Fatalf("reserve %s: %v", h, err)
		}
	}
	for _, h := range []string{"hash-a", "hash-b"} {
		if err := g.Claim(ctx, h); err != nil {
			t.Fatalf("claim %s: %v", h, err)
		}
	}
}

// TestRedisReplayGuard_StoreErrorIsNotAReplay confirms an unreachable
// Redis surfaces as an error distinct from auth.ErrUnauthorized, on
// both halves of the protocol.
func TestRedisReplayGuard_StoreErrorIsNotAReplay(t *testing.T) {
	g, mr := newTestReplayGuard(t)
	ctx := context.Background()
	mr.Close()

	if err := g.Reserve(ctx, "h", time.Minute); err == nil || errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("reserve on dead store: want non-replay error, got %v", err)
	}
	if err := g.Claim(ctx, "h"); err == nil || errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("claim on dead store: want non-replay error, got %v", err)
	}
}
