package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// auth-ks-1 (audit-2026-08-14) — the Redis validator's account-level
// kill switch used to do an uncached per-request Postgres GetBySlug on
// the hot auth path and, on ANY non-ErrNotFound error, return a bare
// wrapped error that the middleware's default branch rendered as 401.
// So a transient Postgres blip 401'd EVERY active customer with the
// wrong "your credential is invalid" status. These tests pin the fix:
// a short-TTL status cache lets a blip ride out on last-known-active,
// and the truly-unknown case returns the retryable
// ErrAccountStatusUnavailable (503) rather than a 401.

// newStatusCacheValidator wires miniredis + a validator with the
// account-status gate enabled and a caller-controlled clock so the
// TTL / staleness transitions are deterministic.
func newStatusCacheValidator(t *testing.T, accounts AccountStatusReader, clock *time.Time) (*RedisAPIKeyValidator, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	v := NewRedisAPIKeyValidator(rdb,
		WithAccountStatus(accounts),
		WithClock(func() time.Time { return *clock }),
	)
	return v, mr
}

// TestRedisAPIKey_AccountStatusBlipRidesOutOnLastKnownActive is the
// core auth-ks-1 regression: an active customer seen moments ago must
// keep authenticating through a transient Postgres blip instead of
// being failed. Pre-fix the second Lookup returned an error (the
// uncached GetBySlug propagated the transport failure); post-fix it
// rides out on the cached last-known-active status.
func TestRedisAPIKey_AccountStatusBlipRidesOutOnLastKnownActive(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountActive)},
	}
	v, mr := newStatusCacheValidator(t, accounts, &clock)
	seedKey(t, mr, "sip_key", APIKeyRecord{
		KeyID:      "kid",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})

	// Warm the cache with one healthy read.
	if _, err := v.Lookup(context.Background(), "sip_key"); err != nil {
		t.Fatalf("warm Lookup: %v", err)
	}

	// Postgres now blips. Advance past the fresh TTL (30s) so the
	// validator re-reads — and hits the error — rather than serving the
	// still-fresh entry, isolating the ride-out branch.
	accounts.err = errors.New("postgres unreachable: connection refused")
	clock = clock.Add(45 * time.Second)

	sub, err := v.Lookup(context.Background(), "sip_key")
	if err != nil {
		t.Fatalf("Lookup during a Postgres blip returned %v; a transient blip must ride out on last-known-active, not fail the active customer", err)
	}
	if sub.KeyID != "kid" {
		t.Errorf("Subject.KeyID = %q, want kid", sub.KeyID)
	}
}

// TestRedisAPIKey_AccountStatusBlipNoCacheIsRetryable pins the
// mis-signalling fix: when there is no usable cached status (cold
// process, first request), a Postgres transport error is a retryable
// ErrAccountStatusUnavailable (→ 503), NOT ErrUnauthorized (→ 401
// "credential invalid"). Still fails closed, but with correct,
// non-key-rotating semantics.
func TestRedisAPIKey_AccountStatusBlipNoCacheIsRetryable(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{err: errors.New("postgres unreachable")}
	v, mr := newStatusCacheValidator(t, accounts, &clock)
	seedKey(t, mr, "sip_cold", APIKeyRecord{
		KeyID:      "kid_cold",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})

	sub, err := v.Lookup(context.Background(), "sip_cold")
	if !errors.Is(err, ErrAccountStatusUnavailable) {
		t.Fatalf("err = %v, want ErrAccountStatusUnavailable (retryable 503)", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Errorf("a Postgres blip must not map to ErrUnauthorized (401 'credential invalid')")
	}
	if sub.KeyID != "" {
		t.Errorf("Subject leaked on the error path: %+v", sub)
	}
}

// TestRedisAPIKey_AccountStatusRideOutBounded pins that the ride-out is
// bounded: once a cached status ages past the staleness bound
// (10×TTL = 5m) it is no longer trusted, so a still-degraded Postgres
// yields the retryable ErrAccountStatusUnavailable rather than an
// unbounded stale authentication.
func TestRedisAPIKey_AccountStatusRideOutBounded(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountActive)},
	}
	v, mr := newStatusCacheValidator(t, accounts, &clock)
	seedKey(t, mr, "sip_key", APIKeyRecord{
		KeyID:      "kid",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})
	if _, err := v.Lookup(context.Background(), "sip_key"); err != nil {
		t.Fatalf("warm Lookup: %v", err)
	}

	accounts.err = errors.New("postgres unreachable")
	clock = clock.Add(6 * time.Minute) // past 10×30s staleness bound

	if _, err := v.Lookup(context.Background(), "sip_key"); !errors.Is(err, ErrAccountStatusUnavailable) {
		t.Fatalf("err = %v, want ErrAccountStatusUnavailable once the cached status is beyond the staleness bound", err)
	}
}

// TestRedisAPIKey_AccountStatusReadAtMostOncePerWindow pins the
// load-bounding half of the fix: within the fresh TTL the status is
// served from cache (one Postgres read per account per window), and a
// read past the window re-reads.
func TestRedisAPIKey_AccountStatusReadAtMostOncePerWindow(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountActive)},
	}
	v, mr := newStatusCacheValidator(t, accounts, &clock)
	seedKey(t, mr, "sip_key", APIKeyRecord{
		KeyID:      "kid",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})

	for i := 0; i < 3; i++ {
		if _, err := v.Lookup(context.Background(), "sip_key"); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	if accounts.calls != 1 {
		t.Fatalf("account reads within the fresh window = %d, want 1 (cache should absorb the rest)", accounts.calls)
	}

	clock = clock.Add(45 * time.Second) // past the fresh TTL
	if _, err := v.Lookup(context.Background(), "sip_key"); err != nil {
		t.Fatalf("post-window Lookup: %v", err)
	}
	if accounts.calls != 2 {
		t.Fatalf("account reads after the window elapsed = %d, want 2 (should re-read)", accounts.calls)
	}
}

// TestRedisAPIKey_SuspendedRideOutStillRejected pins that the kill
// switch is preserved across a blip: a last-known-SUSPENDED account is
// still rejected off its cached status during a Postgres outage — the
// ride-out serves whatever status was last read, not a blanket allow.
func TestRedisAPIKey_SuspendedRideOutStillRejected(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountSuspended)},
	}
	v, mr := newStatusCacheValidator(t, accounts, &clock)
	seedKey(t, mr, "sip_key", APIKeyRecord{
		KeyID:      "kid",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})
	// Warm the cache with the suspended status.
	if _, err := v.Lookup(context.Background(), "sip_key"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("warm Lookup err = %v, want ErrUnauthorized", err)
	}

	accounts.err = errors.New("postgres unreachable")
	clock = clock.Add(45 * time.Second)

	if _, err := v.Lookup(context.Background(), "sip_key"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized — a suspended account must stay rejected off its cached status during a blip", err)
	}
}
