package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// A suspension must stop a key served from the read-through cache
// without relying on the best-effort eviction having run.
func TestPostgresValidator_CacheHitRejectsSuspendedAccount(t *testing.T) {
	keys, accounts, rdb := newStubs()
	clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
		Now:      func() time.Time { return clock },
	})
	const plaintext = "sip_suspend_cached"
	acct := seedActiveAccount(accounts, "suspend-me")
	seedKey(keys, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)

	for i := range 2 { // miss then hit, so the hit path has seen the account active
		if _, err := v.Lookup(context.Background(), plaintext); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}

	acct.Status = platform.AccountSuspended
	accounts.byID[acct.ID] = acct
	clock = clock.Add(auth.DefaultAccountStatusCacheTTL + time.Second)

	if _, err := v.Lookup(context.Background(), plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("lookup after suspension (cache row not evicted) = %v, want ErrUnauthorized", err)
	}
	if n, _ := rdb.Exists(context.Background(), cachekeys.APIKeyCache(hexHashOf(plaintext)).String()).Result(); n != 1 {
		t.Fatal("cache row was evicted; the test must prove rejection without eviction")
	}
}

// A canonical `apikey:` record naming an account has no api_keys row for
// an enforcement-change eviction to find; account status is its only gate.
func TestPostgresValidator_CanonicalRecordRejectsSuspendedAccount(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
	})
	acct := seedActiveAccount(accounts, "legacy-self-service")
	acct.Status = platform.AccountSuspended
	accounts.byID[acct.ID] = acct

	const plaintext = "sip_legacy_redis_only"
	body, _ := json.Marshal(auth.APIKeyRecord{
		KeyID:      "kid_legacy",
		Identifier: auth.AccountIdentifier(acct.Slug),
		Tier:       auth.TierAPIKey,
	})
	if err := rdb.Set(context.Background(), cachekeys.APIKey(hexHashOf(plaintext)).String(), body, 0).Err(); err != nil {
		t.Fatalf("seed canonical record: %v", err)
	}

	if _, err := v.Lookup(context.Background(), plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("lookup of suspended account's canonical record = %v, want ErrUnauthorized", err)
	}
}

// revokeMidLookupKeyStore revokes and evicts the key after GetByHash has
// returned the still-active row: the revoke lands inside a cache-miss
// Lookup, before its cache write-back.
type revokeMidLookupKeyStore struct {
	*stubKeyStore
	onRead func()
}

func (s *revokeMidLookupKeyStore) GetByHash(ctx context.Context, hash []byte) (platform.APIKey, error) {
	k, err := s.stubKeyStore.GetByHash(ctx, hash)
	if s.onRead != nil {
		f := s.onRead
		s.onRead = nil
		f()
	}
	return k, err
}

func TestPostgresValidator_RevokeDuringCacheMissIsNotReCached(t *testing.T) {
	stub, accounts, rdb := newStubs()
	keys := &revokeMidLookupKeyStore{stubKeyStore: stub}
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keys,
		Accounts: accounts,
		Cache:    rdb,
	})
	const plaintext = "sip_revoke_race"
	acct := seedActiveAccount(accounts, "race")
	rec := seedKey(stub, plaintext, acct.ID, platform.APIKeyTierAPIKey, 1000)

	keys.onRead = func() {
		revoked := rec
		revoked.RevokedAt = time.Now().UTC()
		stub.byHash[hexHashOf(plaintext)] = revoked
		stub.byID[rec.ID] = revoked
		if err := v.InvalidateCachedKey(context.Background(), hexHashOf(plaintext)); err != nil {
			t.Errorf("InvalidateCachedKey: %v", err)
		}
	}
	// The in-flight lookup read the key before the revoke committed.
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("in-flight lookup: %v", err)
	}

	if _, err := v.Lookup(context.Background(), plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("lookup after revoke = %v, want ErrUnauthorized (stale row re-cached by the in-flight lookup)", err)
	}
}
