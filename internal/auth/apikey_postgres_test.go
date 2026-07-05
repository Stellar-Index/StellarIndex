package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/StellarIndex/stellar-index/internal/auth"
	"github.com/StellarIndex/stellar-index/internal/cachekeys"
	"github.com/StellarIndex/stellar-index/internal/platform"
)

func TestPostgresValidator_HappyPath_PostgresHit(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}

	plaintext := "sip_postgres_test_001"
	acct := seedActiveAccount(accounts, "acme")
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1500)

	sub, err := v.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sub.Identifier != "acct:acme" {
		t.Errorf("Identifier = %q", sub.Identifier)
	}
	if sub.Tier != auth.TierAPIKey {
		t.Errorf("Tier = %v", sub.Tier)
	}
	if sub.RateLimitPerMin != 1500 {
		t.Errorf("RateLimitPerMin = %d", sub.RateLimitPerMin)
	}
}

// TestPostgresValidator_AccountRateLimitOverride_RaisesFloor pins the
// admin Phase 1.5 enforcement: an account-level
// rate_limit_per_min_override acts as a floor — it raises a key whose
// per-key budget is below it, but never lowers a key budgeted above.
func TestPostgresValidator_AccountRateLimitOverride_RaisesFloor(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})

	// Account override 50k; a key minted at the 1k starter default is
	// raised to 50k.
	acct := seedActiveAccount(accounts, "enterprise-comp")
	acct.RateLimitPerMinOverride = 50000
	accounts.byID[acct.ID] = acct
	seedKey(keys, "sip_below_override", acct.ID, platform.APIKeyTierAPIKey, 1000)

	sub, err := v.Lookup(context.Background(), "sip_below_override")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sub.RateLimitPerMin != 50000 {
		t.Errorf("RateLimitPerMin = %d, want 50000 (override floors up)", sub.RateLimitPerMin)
	}

	// A second key explicitly budgeted ABOVE the override keeps its
	// higher value (override never shrinks a paid-for limit).
	seedKey(keys, "sip_above_override", acct.ID, platform.APIKeyTierAPIKey, 80000)
	sub2, err := v.Lookup(context.Background(), "sip_above_override")
	if err != nil {
		t.Fatalf("lookup 2: %v", err)
	}
	if sub2.RateLimitPerMin != 80000 {
		t.Errorf("RateLimitPerMin = %d, want 80000 (key above override wins)", sub2.RateLimitPerMin)
	}
}

// TestPostgresValidator_NoOverride_UsesPerKey pins that with no
// account override set, the per-key budget is used verbatim.
func TestPostgresValidator_NoOverride_UsesPerKey(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})
	acct := seedActiveAccount(accounts, "no-override")
	seedKey(keys, "sip_no_override", acct.ID, platform.APIKeyTierAPIKey, 3000)
	sub, err := v.Lookup(context.Background(), "sip_no_override")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if sub.RateLimitPerMin != 3000 {
		t.Errorf("RateLimitPerMin = %d, want 3000 (per-key, no override)", sub.RateLimitPerMin)
	}
}

func TestPostgresValidator_RevokedKey_Unauthorized(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})

	plaintext := "sip_revoked"
	acct := seedActiveAccount(accounts, "x")
	rec := seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)
	rec.RevokedAt = time.Now()
	keys.byID[rec.ID] = rec
	keys.byHash[hexHashOf(plaintext)] = rec

	_, err := v.Lookup(context.Background(), plaintext)
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestPostgresValidator_ExpiredKey_TokenExpired(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Now:      func() time.Time { return time.Now() },
	})
	plaintext := "sip_expired"
	acct := seedActiveAccount(accounts, "expired")
	rec := seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 100)
	rec.ExpiresAt = time.Now().Add(-1 * time.Minute)
	keys.byID[rec.ID] = rec
	keys.byHash[hexHashOf(plaintext)] = rec

	_, err := v.Lookup(context.Background(), plaintext)
	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("expected ErrTokenExpired, got %v", err)
	}
}

func TestPostgresValidator_SuspendedAccount_Unauthorized(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})
	plaintext := "sip_suspended_acct"
	acct := seedActiveAccount(accounts, "s")
	acct.Status = platform.AccountSuspended
	accounts.byID[acct.ID] = acct
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 100)

	_, err := v.Lookup(context.Background(), plaintext)
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for suspended account, got %v", err)
	}
}

func TestPostgresValidator_AbsentKey_Unauthorized(t *testing.T) {
	keys, accounts, _ := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
	})
	_, err := v.Lookup(context.Background(), "sip_nonexistent")
	if !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestPostgresValidator_CacheReadThrough(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
		CacheTTL: 5 * time.Minute,
	})

	plaintext := "sip_cache_test"
	acct := seedActiveAccount(accounts, "cached")
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 2000)

	// First lookup: Postgres miss → write-back to cache.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	pgCount := keys.byHashCallCount
	if pgCount == 0 {
		t.Fatal("expected Postgres lookup on cold cache")
	}

	// Second lookup: cache hit → no additional Postgres call.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if keys.byHashCallCount != pgCount {
		t.Errorf("Postgres lookup count grew on cache hit: was %d now %d", pgCount, keys.byHashCallCount)
	}
}

func TestPostgresValidator_Invalidate(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys: keys, Accounts: accounts, Cache: rdb,
	})
	plaintext := "sip_invalidate"
	acct := seedActiveAccount(accounts, "invalid")
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)

	// Populate the cache.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if err := v.InvalidateCachedKey(context.Background(), hexHashOf(plaintext)); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := rdb.Get(context.Background(), cachekeys.APIKey(hexHashOf(plaintext))).Result(); !errors.Is(err, redis.Nil) {
		t.Errorf("cache entry not removed after invalidate: %v", err)
	}
}

// TestPostgresValidator_CacheExpiry_FallsThroughToPostgres proves the
// read-through cache has a bounded lifetime: once the CacheTTL elapses
// the entry is gone and the next Lookup falls through to Postgres and
// re-populates it. This is the safety net behind the invalidation
// contract — even if a revoke/update DEL is ever missed, a revoked or
// rate-changed key can only be served stale for at most one TTL window
// rather than indefinitely.
func TestPostgresValidator_CacheExpiry_FallsThroughToPostgres(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	keys := newStubKeyStore()
	accounts := newStubAccountStore()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys: keys, Accounts: accounts, Cache: rdb, CacheTTL: 30 * time.Second,
	})
	plaintext := "sip_ttl_expiry"
	acct := seedActiveAccount(accounts, "ttl")
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)

	// Cold lookup → Postgres hit + write-back.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("cold lookup: %v", err)
	}
	cold := keys.byHashCallCount
	if cold == 0 {
		t.Fatal("expected a Postgres lookup on cold cache")
	}

	// Warm lookup within TTL → cache hit, no new Postgres call.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}
	if keys.byHashCallCount != cold {
		t.Fatalf("cache miss inside TTL: Postgres calls grew %d → %d", cold, keys.byHashCallCount)
	}

	// Advance past the TTL — miniredis expires the cache entry.
	mr.FastForward(31 * time.Second)

	// Post-expiry lookup must fall through to Postgres again.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("post-expiry lookup: %v", err)
	}
	if keys.byHashCallCount <= cold {
		t.Errorf("expected a fresh Postgres lookup after cache TTL expiry: cold=%d now=%d", cold, keys.byHashCallCount)
	}
}

// TestPostgresValidator_CacheRoundTripsPolicy — when a key has
// IP/Referer allowlists and per-endpoint permissions set, the
// cache-hit Subject MUST carry the same fields as the cache-
// miss Subject. Without this F-1226 (codex audit-2026-05-12)
// reopens — the KeyPolicy middleware silently bypasses
// enforcement on cache hits.
func TestPostgresValidator_CacheRoundTripsPolicy(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
		CacheTTL: 5 * time.Minute,
	})
	plaintext := "sip_policy_roundtrip"
	acct := seedActiveAccount(accounts, "policy")
	seedKeyWithPolicy(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey,
		[]string{"10.0.0.0/8", "192.168.1.0/24"},
		[]string{"app.example.com", "console.example.com"},
		platform.KeyPermissions{
			All: false,
			Allow: []platform.KeyPermissionEntry{
				{Endpoint: "GET /v1/price"},
				{EndpointPrefix: "/v1/history"},
			},
			Deny: []platform.KeyPermissionEntry{
				{Endpoint: "POST /v1/admin"},
			},
		})

	// First lookup: Postgres miss → populates cache.
	first, err := v.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if len(first.IPAllowlist) != 2 || len(first.RefererAllowlist) != 2 || len(first.AllowPermissions) != 2 || len(first.DenyPermissions) != 1 {
		t.Fatalf("first lookup policy fields incomplete: ip=%v ref=%v allow=%v deny=%v",
			first.IPAllowlist, first.RefererAllowlist, first.AllowPermissions, first.DenyPermissions)
	}

	// Second lookup: cache hit. Must carry the same policy.
	second, err := v.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("second lookup (cache hit): %v", err)
	}
	if got := len(second.IPAllowlist); got != 2 {
		t.Errorf("cache-hit IPAllowlist len = %d, want 2 (cache shed the field)", got)
	}
	if got := len(second.RefererAllowlist); got != 2 {
		t.Errorf("cache-hit RefererAllowlist len = %d, want 2", got)
	}
	if got := len(second.AllowPermissions); got != 2 {
		t.Errorf("cache-hit AllowPermissions len = %d, want 2", got)
	}
	if got := len(second.DenyPermissions); got != 1 {
		t.Errorf("cache-hit DenyPermissions len = %d, want 1", got)
	}
	if second.AllowAllPermissions {
		t.Error("cache-hit AllowAllPermissions = true, want false")
	}
	// IP prefixes must round-trip as the same parsed value.
	for i, p := range second.IPAllowlist {
		if p.String() != first.IPAllowlist[i].String() {
			t.Errorf("ip[%d] round-trip = %q, want %q", i, p.String(), first.IPAllowlist[i].String())
		}
	}
}

func TestPostgresValidator_RequiresStores(t *testing.T) {
	if _, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{}); err == nil {
		t.Error("expected error when Keys nil")
	}
	stub, _, _ := newStubs()
	if _, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{Keys: stub}); err == nil {
		t.Error("expected error when Accounts nil")
	}
}

// ─── stubs ───────────────────────────────────────────────────────

func hexHashOf(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func newStubs() (*stubKeyStore, *stubAccountStore, redis.Cmdable) {
	mr, _ := miniredis.Run()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return newStubKeyStore(), newStubAccountStore(), rdb
}

func seedActiveAccount(s *stubAccountStore, slug string) platform.Account {
	a := platform.Account{
		ID:     uuid.New(),
		Name:   slug,
		Slug:   slug,
		Tier:   platform.TierFree,
		Status: platform.AccountActive,
	}
	s.byID[a.ID] = a
	return a
}

func seedKey(s *stubKeyStore, plaintext string, accountID uuid.UUID, tier platform.APIKeyTier, rateLimit int) platform.APIKey {
	sum := sha256.Sum256([]byte(plaintext))
	prefix := plaintext
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	rec := platform.APIKey{
		ID:              "kid_" + uuid.New().String()[:12],
		AccountID:       accountID,
		Name:            "test",
		KeyHash:         sum[:],
		KeyPrefix:       prefix,
		Tier:            tier,
		RateLimitPerMin: rateLimit,
		CreatedAt:       time.Now().UTC(),
	}
	s.byID[rec.ID] = rec
	s.byHash[hex.EncodeToString(sum[:])] = rec
	return rec
}

// seedKeyWithPolicy is like seedKey but stamps IP/Referer/
// permission policy fields so cache round-trip tests can verify
// the cache-hit Subject carries them. F-1226 (codex audit-
// 2026-05-12).
func seedKeyWithPolicy(s *stubKeyStore, plaintext string, accountID uuid.UUID, tier platform.APIKeyTier, ipCIDRs, referers []string, perms platform.KeyPermissions) platform.APIKey {
	sum := sha256.Sum256([]byte(plaintext))
	rec := platform.APIKey{
		ID:               "kid_" + uuid.New().String()[:12],
		AccountID:        accountID,
		Name:             "policy",
		KeyHash:          sum[:],
		KeyPrefix:        plaintext[:12],
		Tier:             tier,
		RateLimitPerMin:  1000,
		IPAllowlist:      mustParsePrefixes(ipCIDRs),
		RefererAllowlist: referers,
		Permissions:      perms,
		CreatedAt:        time.Now().UTC(),
	}
	s.byID[rec.ID] = rec
	s.byHash[hex.EncodeToString(sum[:])] = rec
	return rec
}

func mustParsePrefixes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			panic(err)
		}
		out = append(out, p)
	}
	return out
}

type stubKeyStore struct {
	mu              sync.Mutex
	byID            map[string]platform.APIKey
	byHash          map[string]platform.APIKey
	byHashCallCount int
}

func newStubKeyStore() *stubKeyStore {
	return &stubKeyStore{
		byID:   map[string]platform.APIKey{},
		byHash: map[string]platform.APIKey{},
	}
}

func (s *stubKeyStore) Create(_ context.Context, k platform.APIKey, _ int) (platform.APIKey, error) {
	return k, nil
}

func (s *stubKeyStore) Get(_ context.Context, id string) (platform.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return platform.APIKey{}, platform.ErrNotFound
	}
	return k, nil
}

func (s *stubKeyStore) GetByHash(_ context.Context, hash []byte) (platform.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHashCallCount++
	k, ok := s.byHash[hex.EncodeToString(hash)]
	if !ok {
		return platform.APIKey{}, platform.ErrNotFound
	}
	return k, nil
}

func (s *stubKeyStore) ListForAccount(_ context.Context, _ uuid.UUID) ([]platform.APIKey, error) {
	return nil, nil
}
func (s *stubKeyStore) Update(_ context.Context, _ platform.APIKey) error { return nil }
func (s *stubKeyStore) Revoke(_ context.Context, _ string, _ uuid.UUID, _ string) error {
	return nil
}

func (s *stubKeyStore) TouchUsage(_ context.Context, _ string, _ net.IP, _ string) error {
	return nil
}

type stubAccountStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]platform.Account
}

func newStubAccountStore() *stubAccountStore {
	return &stubAccountStore{byID: map[uuid.UUID]platform.Account{}}
}

func (s *stubAccountStore) Create(_ context.Context, a platform.Account) (platform.Account, error) {
	return a, nil
}

func (s *stubAccountStore) Get(_ context.Context, id uuid.UUID) (platform.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return platform.Account{}, platform.ErrNotFound
	}
	return a, nil
}

func (s *stubAccountStore) GetBySlug(_ context.Context, _ string) (platform.Account, error) {
	return platform.Account{}, platform.ErrNotFound
}

func (s *stubAccountStore) GetByStripeCustomerID(_ context.Context, _ string) (platform.Account, error) {
	return platform.Account{}, platform.ErrNotFound
}
func (s *stubAccountStore) Update(_ context.Context, _ platform.Account) error { return nil }
func (s *stubAccountStore) Suspend(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
func (s *stubAccountStore) Unsuspend(_ context.Context, _ uuid.UUID) error { return nil }

var (
	_ platform.APIKeyStore  = (*stubKeyStore)(nil)
	_ platform.AccountStore = (*stubAccountStore)(nil)
)

// TestPostgresValidator_ScopesPopulateAndRoundTrip pins the scope
// plumbing: the Postgres-built Subject carries the key's scopes and
// the Redis cache round-trips them (the cache is the only other
// path that constructs the Subject, so shedding the field there
// would silently un-scope a key for the cache TTL).
func TestPostgresValidator_ScopesPopulateAndRoundTrip(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
		CacheTTL: 5 * time.Minute,
	})
	plaintext := "sip_scopes_roundtrip"
	acct := seedActiveAccount(accounts, "scoped")
	rec := seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)
	rec.Scopes = []string{platform.KeyScopeRead, platform.KeyScopeAccount}
	keys.byID[rec.ID] = rec
	sum := sha256.Sum256([]byte(plaintext))
	keys.byHash[hex.EncodeToString(sum[:])] = rec

	first, err := v.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if len(first.Scopes) != 2 || first.Scopes[0] != platform.KeyScopeRead {
		t.Fatalf("postgres-built Subject.Scopes = %v", first.Scopes)
	}

	second, err := v.Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("second lookup (cache hit): %v", err)
	}
	if len(second.Scopes) != 2 || second.Scopes[1] != platform.KeyScopeAccount {
		t.Errorf("cache-hit Subject.Scopes = %v (cache shed the field)", second.Scopes)
	}
}
