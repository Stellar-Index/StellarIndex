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
	ok, err := locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("first acquire err: %v", err)
	}
	if !ok {
		t.Fatal("first acquire returned false; expected true")
	}

	// Second Acquire (under the same key) must lose.
	ok, err = locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if ok {
		t.Fatal("second acquire returned true; expected false (lock held)")
	}

	// Release clears the key.
	if err := locker.Release(ctx, key); err != nil {
		t.Fatalf("release err: %v", err)
	}

	// Third Acquire (post-release) wins again.
	ok, err = locker.Acquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("post-release acquire err: %v", err)
	}
	if !ok {
		t.Fatal("post-release acquire returned false; expected true")
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

	ok, err := locker.Acquire(ctx, key, 1*time.Second)
	if err != nil || !ok {
		t.Fatalf("initial acquire: ok=%v err=%v", ok, err)
	}

	// Advance miniredis' clock past the TTL.
	mr.FastForward(2 * time.Second)

	ok, err = locker.Acquire(ctx, key, 1*time.Second)
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
	if err := locker.Release(context.Background(), "never-acquired"); err != nil {
		t.Fatalf("release on absent key returned err: %v", err)
	}
}
