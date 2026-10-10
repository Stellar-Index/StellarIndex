package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fixedClock returns a deterministic now() for expiry tests.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// newTestValidator wires miniredis + a Redis client + a validator
// with a fixed clock. Returns the validator + the miniredis handle
// (so tests can SET records directly) + the clock anchor.
func newTestValidator(t *testing.T) (*RedisAPIKeyValidator, *miniredis.Miniredis, time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// Anchor time to a stable reference so expiry comparisons are
	// reproducible regardless of when the test runs.
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	v := NewRedisAPIKeyValidator(rdb, WithClock(fixedClock(now)))
	return v, mr, now
}

// seedKey serialises rec and writes it under the canonical
// cachekeys.APIKey(hash(key)) location. Mirrors what the admin
// seeding path does — the validator's contract is "if a record is
// present at this key shape, here's the lookup behaviour".
func seedKey(t *testing.T, mr *miniredis.Miniredis, key string, rec APIKeyRecord) {
	t.Helper()
	hash := HashAPIKey(key)
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}
	mr.Set(cachekeys.APIKey(hash).String(), string(body))
}

// TestRedisAPIKey_LookupHappyPath is the everyday allow-path: a
// freshly-seeded record with no expiry and no revocation returns the
// owner Subject with the apikey tier and the seeded scopes.
func TestRedisAPIKey_LookupHappyPath(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "rek_test_abc123", APIKeyRecord{
		Identifier: "owner-42",
		Tier:       TierAPIKey,
		Label:      "ci-bot",
		Scopes:     []string{"price:read", "history:read"},
	})

	got, err := v.Lookup(context.Background(), "rek_test_abc123")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Identifier != "owner-42" {
		t.Errorf("Identifier = %q, want owner-42", got.Identifier)
	}
	if got.Tier != TierAPIKey {
		t.Errorf("Tier = %q, want apikey", got.Tier)
	}
	if got.Label != "ci-bot" {
		t.Errorf("Label = %q, want ci-bot", got.Label)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "price:read" {
		t.Errorf("Scopes = %v, want [price:read, history:read]", got.Scopes)
	}
}

// TestRedisAPIKey_LookupNotFound covers the deny-path: an unknown
// key (no record in Redis) returns ErrUnauthorized. The 404 vs 401
// distinction matters — we must not leak "this key existed at some
// point" via a different sentinel for revoked vs absent.
func TestRedisAPIKey_LookupNotFound(t *testing.T) {
	v, _, _ := newTestValidator(t)
	_, err := v.Lookup(context.Background(), "rek_does_not_exist")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("Lookup of absent key: got %v, want ErrUnauthorized", err)
	}
}

// TestRedisAPIKey_LookupRevoked confirms a revoked record returns
// ErrUnauthorized — same surface as not-found, deliberately. Don't
// distinguish "wrong key" from "revoked key" on the wire; that's
// information leak.
func TestRedisAPIKey_LookupRevoked(t *testing.T) {
	v, mr, now := newTestValidator(t)
	seedKey(t, mr, "rek_revoked", APIKeyRecord{
		Identifier: "owner-12",
		Tier:       TierAPIKey,
		RevokedAt:  now.Add(-time.Hour),
	})

	_, err := v.Lookup(context.Background(), "rek_revoked")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoked: got %v, want ErrUnauthorized", err)
	}
}

// TestRedisAPIKey_LookupExpired confirms an expired record returns
// ErrTokenExpired (NOT ErrUnauthorized) — the middleware uses this
// to set a more useful WWW-Authenticate header so the client knows
// to refresh rather than guess at credential validity.
func TestRedisAPIKey_LookupExpired(t *testing.T) {
	v, mr, now := newTestValidator(t)
	seedKey(t, mr, "rek_expired", APIKeyRecord{
		Identifier: "owner-77",
		Tier:       TierAPIKey,
		ExpiresAt:  now.Add(-time.Minute),
	})

	_, err := v.Lookup(context.Background(), "rek_expired")
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("expired: got %v, want ErrTokenExpired", err)
	}
}

// TestRedisAPIKey_LookupExactlyAtExpiry pins the boundary. A record
// whose expires_at equals now() is treated as expired — `now.Before(exp)`
// is false at equality. This matters for tests that seed a record
// with ExpiresAt == fixed-clock and would otherwise flake on
// machine clock skew.
func TestRedisAPIKey_LookupExactlyAtExpiry(t *testing.T) {
	v, mr, now := newTestValidator(t)
	seedKey(t, mr, "rek_at_boundary", APIKeyRecord{
		Identifier: "owner-boundary",
		Tier:       TierAPIKey,
		ExpiresAt:  now,
	})

	_, err := v.Lookup(context.Background(), "rek_at_boundary")
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("at-expiry: got %v, want ErrTokenExpired", err)
	}
}

// TestRedisAPIKey_FutureExpiryStillValid is the inverse boundary —
// a record whose expiry is one second after now() is still active.
func TestRedisAPIKey_FutureExpiryStillValid(t *testing.T) {
	v, mr, now := newTestValidator(t)
	seedKey(t, mr, "rek_future", APIKeyRecord{
		Identifier: "owner-future",
		Tier:       TierAPIKey,
		ExpiresAt:  now.Add(time.Second),
	})

	got, err := v.Lookup(context.Background(), "rek_future")
	if err != nil {
		t.Fatalf("future-expiry: %v", err)
	}
	if got.Identifier != "owner-future" {
		t.Errorf("Identifier = %q", got.Identifier)
	}
}

// TestRedisAPIKey_DefaultsToAPIKeyTier ensures a record with an empty
// tier field decodes as TierAPIKey. Operator keys MUST be opt-in via
// explicit tier="operator"; an unset field never escalates.
func TestRedisAPIKey_DefaultsToAPIKeyTier(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "rek_no_tier", APIKeyRecord{
		Identifier: "owner-default",
		// Tier deliberately empty
	})

	got, err := v.Lookup(context.Background(), "rek_no_tier")
	if err != nil {
		t.Fatalf("no-tier: %v", err)
	}
	if got.Tier != TierAPIKey {
		t.Errorf("default tier = %q, want apikey", got.Tier)
	}
}

// TestRedisAPIKey_OperatorTierPreserved ensures an explicit
// tier=operator survives the round-trip. The middleware uses this
// to gate /v1/admin/* endpoints.
func TestRedisAPIKey_OperatorTierPreserved(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "rek_admin", APIKeyRecord{
		Identifier: "ops-bot",
		Tier:       TierOperator,
	})

	got, err := v.Lookup(context.Background(), "rek_admin")
	if err != nil {
		t.Fatalf("operator: %v", err)
	}
	if got.Tier != TierOperator {
		t.Errorf("Tier = %q, want operator", got.Tier)
	}
}

// TestRedisAPIKey_EmptyKeyRejected confirms an empty input is
// rejected without a Redis round-trip. An admin tool that calls
// Lookup directly with "" must not land on a record (and certainly
// not on the empty-hash record if one were ever seeded).
func TestRedisAPIKey_EmptyKeyRejected(t *testing.T) {
	v, _, _ := newTestValidator(t)
	_, err := v.Lookup(context.Background(), "")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("empty key: got %v, want ErrUnauthorized", err)
	}
}

// TestRedisAPIKey_MalformedRecord exercises the operator-corruption
// branch. A record whose JSON doesn't decode is logged-but-401'd; we
// must not leak the corruption to the caller via a different
// sentinel.
func TestRedisAPIKey_MalformedRecord(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	hash := HashAPIKey("rek_corrupt")
	mr.Set(cachekeys.APIKey(hash).String(), "{not-json")

	_, err := v.Lookup(context.Background(), "rek_corrupt")
	if err == nil {
		t.Fatal("malformed record: want error, got nil")
	}
	// Wrapped non-sentinel — middleware's default branch fires (401).
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrTokenExpired) {
		t.Errorf("malformed should not match sentinel; got %v", err)
	}
}

// TestHashAPIKey_Stable pins the hash output so a future change to
// the hash function (HMAC, BLAKE2, etc.) is a deliberate decision
// that breaks this test. Every existing record in production Redis
// is keyed off this hash; rotating it is a wire break.
func TestHashAPIKey_Stable(t *testing.T) {
	const key = "rek_test_hash_pin"
	const want = "1690aa3074d262b0800b274269069eb492fc94043a7cef98b9c6a6c85a39f737"
	got := HashAPIKey(key)
	if got != want {
		t.Errorf("HashAPIKey(%q)\n  got  %s\n  want %s", key, got, want)
	}
}

// TestNewRedisAPIKeyValidator_PanicsOnNil confirms the constructor
// rejects a nil client. The auth middleware fails-loud upstream when
// the validator is the Noop stub; passing nil here would yield a
// runtime panic on the first request instead of a startup panic,
// which is much harder to debug.
func TestNewRedisAPIKeyValidator_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewRedisAPIKeyValidator(nil) should panic")
		}
	}()
	_ = NewRedisAPIKeyValidator(nil)
}

// TestRedisAPIKey_LookupMapsMonthlyQuota pins that Lookup copies
// MonthlyQuota onto the Subject.
//
// MonthlyQuota is persisted on APIKeyRecord ("the per-key monthly
// request cap the runtime quota middleware enforces") and mapped by the
// Postgres validator. middleware.MonthlyQuota short-circuits on
// `subject.MonthlyQuota <= 0`, so if Lookup skipped it, on the default
// (redis) backend, which is what r1 runs, the cap would be dead for
// every key: a metered key would never be metered or 429'd, and nothing
// would log or alert. Same bug class as dropping the permission fields.
func TestRedisAPIKey_LookupMapsMonthlyQuota(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "rek_test_quota", APIKeyRecord{
		Identifier:   "owner-metered",
		Tier:         TierAPIKey,
		MonthlyQuota: 1_000_000,
	})

	got, err := v.Lookup(context.Background(), "rek_test_quota")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.MonthlyQuota != 1_000_000 {
		t.Errorf("MonthlyQuota = %d, want 1000000 — the quota middleware short-circuits on <= 0, so an unmapped quota means the cap is silently unenforced", got.MonthlyQuota)
	}
}

// fullSubject sets EVERY Subject field to a non-zero value, and fails the
// test when a field is left zero — so a field added to Subject must be
// added here, which then forces the round-trip tests below to carry it.
func fullSubject(t *testing.T) Subject {
	t.Helper()
	at := func(day int) time.Time { return time.Date(2026, 3, day, 12, 0, 0, 0, time.UTC) }
	sub := Subject{
		Identifier:          "signup-roundtrip",
		Tier:                TierOperator,
		Scopes:              []string{"read", "account"},
		KeyID:               "kid_roundtrip",
		RateLimitPerMin:     4321,
		CreatedAt:           at(1),
		Label:               "ci-bot",
		KeyPrefix:           "sip_roundtri",
		IPAllowlist:         []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
		RefererAllowlist:    []string{"example.com"},
		AllowAllPermissions: true,
		AllowPermissions:    []SubjectPermissionEntry{{Endpoint: "GET /v1/price"}},
		DenyPermissions:     []SubjectPermissionEntry{{EndpointPrefix: "/v1/account/"}},
		MonthlyQuota:        9_000_000,
		EmailVerifiedAt:     at(2),
		ExpiresAt:           at(28),
	}
	rv := reflect.ValueOf(sub)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Fatalf("fullSubject leaves Subject.%s zero — populate it so the round-trip tests cover it", rv.Type().Field(i).Name)
		}
	}
	return sub
}

// TestPostgresValidator_CacheRoundTripCarriesEverySubjectField pins
// that the Postgres validator's read-through cache hands back the
// Subject it was given, field for field. Rebuilding the record by hand
// can drop EmailVerifiedAt, so a verified signup customer served from a
// cache hit would read as permanently unverified.
func TestPostgresValidator_CacheRoundTripCarriesEverySubjectField(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	v := &PostgresAPIKeyValidator{cache: rdb, now: fixedClock(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)), cacheTTL: time.Hour}

	want := fullSubject(t)
	const hexHash = "ab12"
	v.cacheStore(context.Background(), hexHash, want)
	got, hit, err := v.cacheLookup(context.Background(), hexHash)
	if err != nil || !hit {
		t.Fatalf("cacheLookup = hit %v, err %v; want a hit", hit, err)
	}
	assertSameSubject(t, got, want)
}

// assertSameSubject reports every Subject field that differs, by name.
func assertSameSubject(t *testing.T, got, want Subject) {
	t.Helper()
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range wv.NumField() {
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("Subject.%s = %v, want %v", wv.Type().Field(i).Name, gv.Field(i).Interface(), wv.Field(i).Interface())
		}
	}
}

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
// core check: an active customer seen moments ago must keep
// authenticating through a transient Postgres blip instead of being
// failed. The second Lookup rides out on the cached last-known-active
// status rather than propagating the transport failure.
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

	// Postgres blips. Advance past the fresh TTL (30s) so the
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
// (10×TTL = 5m) it is not trusted, so a still-degraded Postgres
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
// load bound: within the fresh TTL the status is
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

// TestRedisAPIKey_AccountStatusCacheEvictsStaleEntries pins that
// a write past the staleness bound sweeps out entries that aged past
// it, bounding the cache to the working set of recently-seen accounts
// instead of every account slug ever seen for the life of the process.
func TestRedisAPIKey_AccountStatusCacheEvictsStaleEntries(t *testing.T) {
	clock := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{}}
	v, mr := newStatusCacheValidator(t, accounts, &clock)

	const n = 5
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("acct-%d", i)
		accounts.bySlug[slug] = acctWithStatus(slug, platform.AccountActive)
		seedKey(t, mr, "sip_"+slug, APIKeyRecord{
			KeyID:      "kid-" + slug,
			Identifier: AccountIdentifier(slug),
			Tier:       TierAPIKey,
		})
		if _, err := v.Lookup(context.Background(), "sip_"+slug); err != nil {
			t.Fatalf("warm Lookup %s: %v", slug, err)
		}
	}
	if got := statusCacheLen(v); got != n {
		t.Fatalf("cache size after warming %d distinct accounts = %d, want %d", n, got, n)
	}

	// Advance past the staleness bound (10x30s = 5m) and touch one more,
	// distinct account. The write must sweep the unreadable stale
	// entries rather than leaving them in the map forever.
	clock = clock.Add(10 * time.Minute)
	accounts.bySlug["acct-new"] = acctWithStatus("acct-new", platform.AccountActive)
	seedKey(t, mr, "sip_new", APIKeyRecord{
		KeyID:      "kid-new",
		Identifier: AccountIdentifier("acct-new"),
		Tier:       TierAPIKey,
	})
	if _, err := v.Lookup(context.Background(), "sip_new"); err != nil {
		t.Fatalf("warm Lookup acct-new: %v", err)
	}

	if got := statusCacheLen(v); got != 1 {
		t.Fatalf("cache size after the stale sweep = %d, want 1 (only the fresh entry) — stale entries were never evicted", got)
	}
}

// statusCacheLen reads the current status cache size under its mutex.
func statusCacheLen(v *RedisAPIKeyValidator) int {
	v.status.mu.RLock()
	defer v.status.mu.RUnlock()
	return len(v.status.cache)
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

// stubAccountStatusReader is a canned [AccountStatusReader].
type stubAccountStatusReader struct {
	bySlug map[string]platform.Account
	err    error
	calls  int
}

func (s *stubAccountStatusReader) GetBySlug(_ context.Context, slug string) (platform.Account, error) {
	s.calls++
	if s.err != nil {
		return platform.Account{}, s.err
	}
	a, ok := s.bySlug[slug]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

// newSuspendTestValidator wires miniredis + a validator with the
// account-status gate enabled.
func newSuspendTestValidator(t *testing.T, accounts AccountStatusReader) (*RedisAPIKeyValidator, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisAPIKeyValidator(rdb, WithAccountStatus(accounts)), mr
}

func acctWithStatus(slug string, st platform.AccountStatus) platform.Account {
	return platform.Account{ID: uuid.New(), Slug: slug, Name: slug, Tier: platform.TierPro, Status: st}
}

// TestRedisAPIKey_SuspendedAccountRejected is the core check: a
// live, unrevoked, unexpired key belonging to a SUSPENDED account must
// not authenticate.
func TestRedisAPIKey_SuspendedAccountRejected(t *testing.T) {
	for _, st := range []platform.AccountStatus{platform.AccountSuspended, platform.AccountClosed} {
		t.Run(string(st), func(t *testing.T) {
			accounts := &stubAccountStatusReader{
				bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", st)},
			}
			v, mr := newSuspendTestValidator(t, accounts)
			seedKey(t, mr, "sip_live_key", APIKeyRecord{
				KeyID:      "kid_live",
				Identifier: AccountIdentifier("acme"),
				Tier:       TierAPIKey,
			})

			_, err := v.Lookup(context.Background(), "sip_live_key")
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Lookup err = %v, want ErrUnauthorized — a %s account's key must not authenticate", err, st)
			}
			if accounts.calls != 1 {
				t.Errorf("account lookups = %d, want 1", accounts.calls)
			}
		})
	}
}

// TestRedisAPIKey_ActiveAccountAllowed pins the allow-path: the gate
// must not break the ordinary case, and must carry the record's Subject
// through unchanged.
func TestRedisAPIKey_ActiveAccountAllowed(t *testing.T) {
	accounts := &stubAccountStatusReader{
		bySlug: map[string]platform.Account{"acme": acctWithStatus("acme", platform.AccountActive)},
	}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_ok_key", APIKeyRecord{
		KeyID:           "kid_ok",
		Identifier:      AccountIdentifier("acme"),
		Tier:            TierAPIKey,
		RateLimitPerMin: 10000,
	})

	sub, err := v.Lookup(context.Background(), "sip_ok_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_ok" || sub.RateLimitPerMin != 10000 {
		t.Errorf("Subject = %+v, want kid_ok @ 10000/min", sub)
	}
}

// TestRedisAPIKey_LegacySignupIdentifierUnaffected pins the scope of
// the gate: a `signup-<emailhash>` record carries no account reference,
// so it must authenticate without any account lookup at all (the
// operator kill switch for those is DELETE /v1/admin/keys/{keyID}).
func TestRedisAPIKey_LegacySignupIdentifierUnaffected(t *testing.T) {
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{}}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_legacy_key", APIKeyRecord{
		KeyID:      "kid_legacy",
		Identifier: "signup-0011223344556677",
		Tier:       TierAPIKey,
	})

	sub, err := v.Lookup(context.Background(), "sip_legacy_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_legacy" {
		t.Errorf("Subject.KeyID = %q, want kid_legacy", sub.KeyID)
	}
	if accounts.calls != 0 {
		t.Errorf("account lookups = %d, want 0 for a legacy signup identifier", accounts.calls)
	}
}

// TestRedisAPIKey_MissingAccountRejected pins the fail-closed direction
// for a dangling reference: an `acct:` key whose account row is gone is
// the closed-account case, not an unknown one.
func TestRedisAPIKey_MissingAccountRejected(t *testing.T) {
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{}}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_orphan_key", APIKeyRecord{
		KeyID:      "kid_orphan",
		Identifier: AccountIdentifier("deleted-co"),
		Tier:       TierAPIKey,
	})

	if _, err := v.Lookup(context.Background(), "sip_orphan_key"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Lookup err = %v, want ErrUnauthorized", err)
	}
}

// TestRedisAPIKey_AccountReadErrorFailsClosed pins the degradation
// direction. Every other store failure in this package degrades toward
// serving the request; the suspension gate must not — "the account store
// blipped, so authenticate the suspended customer anyway" is the one
// degradation a kill switch cannot make.
func TestRedisAPIKey_AccountReadErrorFailsClosed(t *testing.T) {
	accounts := &stubAccountStatusReader{err: errors.New("postgres unreachable")}
	v, mr := newSuspendTestValidator(t, accounts)
	seedKey(t, mr, "sip_blip_key", APIKeyRecord{
		KeyID:      "kid_blip",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})

	sub, err := v.Lookup(context.Background(), "sip_blip_key")
	if err == nil {
		t.Fatalf("Lookup succeeded with Subject %+v; want an error (fail closed)", sub)
	}
	if sub.KeyID != "" {
		t.Errorf("Subject leaked on the error path: %+v", sub)
	}
}

// TestRedisAPIKey_NoAccountReaderIsPreFixBehaviour pins that the gate is
// strictly opt-in: without a reader wired there is no account lookup and
// no behaviour change at all.
func TestRedisAPIKey_NoAccountReaderIsPreFixBehaviour(t *testing.T) {
	v, mr, _ := newTestValidator(t)
	seedKey(t, mr, "sip_nogate_key", APIKeyRecord{
		KeyID:      "kid_nogate",
		Identifier: AccountIdentifier("acme"),
		Tier:       TierAPIKey,
	})
	sub, err := v.Lookup(context.Background(), "sip_nogate_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.KeyID != "kid_nogate" {
		t.Errorf("Subject.KeyID = %q, want kid_nogate", sub.KeyID)
	}
}

// TestRedisAPIKey_AccountOverridesResolved pins that, on the default
// redis backend an operator's account overrides are enforced on the next
// Lookup, with the Postgres validator's cascade (the rate-limit override is
// a floor, the monthly-quota override a ceiling), and a change lands once the
// account cache refreshes.
func TestRedisAPIKey_AccountOverridesResolved(t *testing.T) {
	acct := acctWithStatus("acme", platform.AccountActive)
	acct.RateLimitPerMinOverride = 50_000
	acct.MonthlyRequestQuotaOverride = 2_000
	accounts := &stubAccountStatusReader{bySlug: map[string]platform.Account{"acme": acct}}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	v := NewRedisAPIKeyValidator(rdb, WithAccountStatus(accounts), WithClock(func() time.Time { return now }))
	seedKey(t, mr, "sip_override_key", APIKeyRecord{
		KeyID: "kid_override", Identifier: AccountIdentifier("acme"), Tier: TierAPIKey,
		RateLimitPerMin: 1_000, MonthlyQuota: 100_000,
	})

	sub, err := v.Lookup(context.Background(), "sip_override_key")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.RateLimitPerMin != 50_000 || sub.MonthlyQuota != 2_000 {
		t.Fatalf("Subject budgets = %d/min, %d/month; want the overrides 50000/min, 2000/month",
			sub.RateLimitPerMin, sub.MonthlyQuota)
	}

	// Operator clears both overrides: the per-key budget is enforced again
	// once the account cache refreshes.
	acct.RateLimitPerMinOverride, acct.MonthlyRequestQuotaOverride = 0, 0
	accounts.bySlug["acme"] = acct
	now = now.Add(DefaultAccountStatusCacheTTL + time.Second)
	sub, err = v.Lookup(context.Background(), "sip_override_key")
	if err != nil {
		t.Fatalf("Lookup after refresh: %v", err)
	}
	if sub.RateLimitPerMin != 1_000 || sub.MonthlyQuota != 100_000 {
		t.Errorf("Subject budgets after clearing = %d/min, %d/month; want the per-key 1000/min, 100000/month",
			sub.RateLimitPerMin, sub.MonthlyQuota)
	}
}
