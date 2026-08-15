package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisPasskeyCeremonyGuard_ConsumeIsOneShot — the adapter's
// contract: the first presentation of a ceremony claims it, every
// later presentation of the SAME ceremony is refused. That refusal is
// what stops a captured `/v1/auth/passkey/finish-login` request from
// minting a second session. audit-2026-08-13.
func TestRedisPasskeyCeremonyGuard_ConsumeIsOneShot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := NewRedisPasskeyCeremonyGuard(rdb)
	ctx := context.Background()
	const digest = "0123456789abcdef"

	claimed, err := guard.Consume(ctx, digest, 5*time.Minute)
	if err != nil {
		t.Fatalf("first consume err: %v", err)
	}
	if !claimed {
		t.Fatal("first consume returned false; the ceremony was never spent")
	}

	claimed, err = guard.Consume(ctx, digest, 5*time.Minute)
	if err != nil {
		t.Fatalf("replay consume err: %v", err)
	}
	if claimed {
		t.Fatal("replay consume returned true — a captured ceremony would mint a second session")
	}

	// A different ceremony is unaffected.
	if claimed, err = guard.Consume(ctx, "fedcba9876543210", 5*time.Minute); err != nil || !claimed {
		t.Fatalf("unrelated ceremony: claimed=%v err=%v", claimed, err)
	}
}

// TestRedisPasskeyCeremonyGuard_RecordExpires — the spent-set is
// TTL-bounded so it can't grow without limit. Expiry is safe: a
// ceremony whose spent-record has aged out is itself long past its
// 5-minute lifetime and refused by the expiry check instead.
func TestRedisPasskeyCeremonyGuard_RecordExpires(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	guard := NewRedisPasskeyCeremonyGuard(rdb)
	ctx := context.Background()

	if claimed, err := guard.Consume(ctx, "expiring", time.Minute); err != nil || !claimed {
		t.Fatalf("initial consume: claimed=%v err=%v", claimed, err)
	}
	mr.FastForward(2 * time.Minute)
	if claimed, err := guard.Consume(ctx, "expiring", time.Minute); err != nil || !claimed {
		t.Fatalf("post-TTL consume: claimed=%v err=%v, want the record gone", claimed, err)
	}
}

// TestRedisPasskeyCeremonyGuard_KeyNamespace pins the key prefix: it
// has to stay in step with the Redis ACL allow-list
// (configs/ansible/roles/redis-sentinel/templates/users.acl.j2), or a
// lockdown deployment ACL-denies the SETNX and — because the handler
// fails closed — passkey sign-in stops working.
func TestRedisPasskeyCeremonyGuard_KeyNamespace(t *testing.T) {
	if got := passkeyCeremonyKey("abc"); got != "passkey:ceremony:abc" {
		t.Fatalf("key = %q, want passkey:ceremony:abc", got)
	}
}

// TestRedisPasskeyCeremonyGuard_ErrorPropagates — a store failure must
// surface as an error, never as "claimed". The caller fails closed on
// it; a swallowed error would silently disable replay protection.
func TestRedisPasskeyCeremonyGuard_ErrorPropagates(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close() // the store is now unreachable

	claimed, err := NewRedisPasskeyCeremonyGuard(rdb).Consume(context.Background(), "digest", time.Minute)
	if err == nil {
		t.Fatal("unreachable Redis returned no error")
	}
	if claimed {
		t.Fatal("unreachable Redis reported the ceremony as claimed")
	}
}

// TestRedisPasskeyCeremonyGuard_ReserveClaimIsOneShot — the eviction-
// safe single-use protocol (W1-auth-passkey-1): a ceremony reserved at
// begin is claimable exactly once, and a second claim of the same
// digest — the captured-request replay — is refused.
func TestRedisPasskeyCeremonyGuard_ReserveClaimIsOneShot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	guard := NewRedisPasskeyCeremonyGuard(rdb)
	ctx := context.Background()
	const digest = "0123456789abcdef"

	if err := guard.Reserve(ctx, digest, 5*time.Minute); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	claimed, err := guard.ClaimReserved(ctx, digest)
	if err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v, want true", claimed, err)
	}
	claimed, err = guard.ClaimReserved(ctx, digest)
	if err != nil {
		t.Fatalf("replay claim err: %v", err)
	}
	if claimed {
		t.Fatal("replay claim returned true — a captured ceremony would mint a second session")
	}
}

// TestRedisPasskeyCeremonyGuard_ClaimFailsClosedOnEviction is the core
// of W1-auth-passkey-1. R1 runs allkeys-lru, so under memory pressure
// the reservation marker can be evicted before its TTL. When it is, the
// claim must REFUSE (fail closed), never treat the free slot as a fresh
// ceremony — otherwise a replayed finish-login mints a second session
// for the victim. The bare SETNX spent-set did the opposite: an evicted
// marker let SETNX re-claim. Eviction is simulated by deleting the live
// marker out from under the guard, exactly as an LRU pass would.
func TestRedisPasskeyCeremonyGuard_ClaimFailsClosedOnEviction(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	guard := NewRedisPasskeyCeremonyGuard(rdb)
	ctx := context.Background()
	const digest = "deadbeefcafef00d"

	if err := guard.Reserve(ctx, digest, 5*time.Minute); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// allkeys-lru evicts the marker before its TTL.
	mr.Del(liveCeremonyKey(digest))

	claimed, err := guard.ClaimReserved(ctx, digest)
	if err != nil {
		t.Fatalf("claim err: %v", err)
	}
	if claimed {
		t.Fatal("an evicted reservation was claimed — the single-use replay window is re-opened")
	}
}
