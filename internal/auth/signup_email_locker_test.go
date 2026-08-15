package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisSignupEmailLocker_AcquireReleaseRoundTrip — the
// SETNX adapter's contract is: first Acquire wins, second
// Acquire (without intervening Release) loses, Release makes
// the key available again. F-1255 (codex audit-2026-05-12).
func TestRedisSignupEmailLocker_AcquireReleaseRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	locker := NewRedisSignupEmailLocker(rdb)
	ctx := context.Background()
	const key = "abcdef0123456789"
	ttl := 5 * time.Second

	// First Acquire wins.
	ok, token, err := locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("first acquire err: %v", err)
	}
	if !ok {
		t.Fatal("first acquire returned false; expected true")
	}
	if token == "" {
		t.Fatal("first acquire returned empty token; expected a fencing token")
	}

	// Second Acquire (under the same key) must lose.
	ok, _, err = locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok {
		t.Fatal("second acquire returned true; expected false (lock held)")
	}

	// Release clears the key.
	if err := locker.Release(ctx, key, token); err != nil {
		t.Fatalf("release err: %v", err)
	}

	// Third Acquire (post-release) wins again.
	ok, _, err = locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("post-release acquire err: %v", err)
	}
	if !ok {
		t.Fatal("post-release acquire returned false; expected true")
	}
}

// TestRedisSignupEmailLocker_ReleaseDoesNotDeleteSuccessorLock — the
// F-C fencing property: caller A's lock TTL-expires while A is still
// provisioning, caller B SETNX-acquires the now-free key, then A's
// deferred Release runs. A's Release MUST NOT delete B's lock (they
// carry different tokens), otherwise a third caller could acquire and
// race B — the exact F-1255 orphan race the lock exists to prevent.
func TestRedisSignupEmailLocker_ReleaseDoesNotDeleteSuccessorLock(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	locker := NewRedisSignupEmailLocker(rdb)
	ctx := context.Background()
	const key = "overrun-test"

	// A acquires with a 1s TTL.
	okA, tokenA, err := locker.Acquire(ctx, key, 1*time.Second)
	if err != nil || !okA {
		t.Fatalf("A acquire: ok=%v err=%v", okA, err)
	}

	// A's critical section overruns the TTL; the lock expires.
	mr.FastForward(2 * time.Second)

	// B acquires the now-free key with its own token.
	okB, tokenB, err := locker.Acquire(ctx, key, 5*time.Second)
	if err != nil || !okB {
		t.Fatalf("B acquire: ok=%v err=%v", okB, err)
	}
	if tokenA == tokenB {
		t.Fatalf("A and B share token %q; fencing tokens must be distinct", tokenA)
	}

	// A's deferred Release runs LATE, with A's stale token. It must be
	// a no-op — B still holds the lock.
	if err := locker.Release(ctx, key, tokenA); err != nil {
		t.Fatalf("A release err: %v", err)
	}

	// B's lock must survive: a fresh Acquire must LOSE.
	okC, _, err := locker.Acquire(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("C acquire err: %v", err)
	}
	if okC {
		t.Fatal("acquire succeeded after A's stale Release deleted B's lock; " +
			"blind DEL reopened the orphan race (F-C not fixed)")
	}

	// And B's own token-scoped Release still works.
	if err := locker.Release(ctx, key, tokenB); err != nil {
		t.Fatalf("B release err: %v", err)
	}
	okD, _, err := locker.Acquire(ctx, key, 5*time.Second)
	if err != nil || !okD {
		t.Fatalf("D acquire after B release: ok=%v err=%v", okD, err)
	}
}

// TestRedisSignupEmailLocker_TTLExpires — after the TTL elapses
// without an explicit Release, another caller can Acquire. This
// is the safety net for a process crash between Acquire and
// Release.
func TestRedisSignupEmailLocker_TTLExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	locker := NewRedisSignupEmailLocker(rdb)
	ctx := context.Background()
	const key = "ttl-test"

	ok, _, err := locker.Acquire(ctx, key, 1*time.Second)
	if err != nil || !ok {
		t.Fatalf("initial acquire: ok=%v err=%v", ok, err)
	}

	// Advance miniredis' clock past the TTL.
	mr.FastForward(2 * time.Second)

	ok, _, err = locker.Acquire(ctx, key, 1*time.Second)
	if err != nil {
		t.Fatalf("post-expiry acquire err: %v", err)
	}
	if !ok {
		t.Fatal("post-expiry acquire returned false; expected true (TTL should have cleared the key)")
	}
}

// TestRedisSignupEmailLocker_ReleaseOfAbsentKeyIsNoop — Release
// on a key that was never set (or has already TTL-expired) is a
// successful no-op. The dashboardauth handler always defers
// Release, so this matters when the lock TTL elapses before
// Account.Create + Users.CreateUser finishes.
func TestRedisSignupEmailLocker_ReleaseOfAbsentKeyIsNoop(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	locker := NewRedisSignupEmailLocker(rdb)
	if err := locker.Release(context.Background(), "never-acquired", "tok-x"); err != nil {
		t.Fatalf("release on absent key returned err: %v", err)
	}
}
