// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// failingAccountStore answers Get with a transport error.
type failingAccountStore struct{ *stubAccountStore }

func (failingAccountStore) Get(context.Context, uuid.UUID) (platform.Account, error) {
	return platform.Account{}, errors.New("postgres: connection reset")
}

func requireUnavailableNotRejected(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v: a store that could not be read must not reject the credential as invalid", err)
	}
	if !errors.Is(err, auth.ErrKeyStoreUnavailable) && !errors.Is(err, auth.ErrAccountStatusUnavailable) {
		t.Fatalf("err = %v, want ErrKeyStoreUnavailable or ErrAccountStatusUnavailable", err)
	}
}

// A Redis-only credential (operator key, self-service child) has no api_keys row: with Redis down its only
// home is unread, so the answer is a retryable 503, not a 401 charged to the failed-auth budget.
func TestPostgresValidator_RedisDown_RedisOnlyKeyIsUnavailable(t *testing.T) {
	keys, accounts, _ := newStubs()
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = dead.Close() })
	v, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{Keys: keys, Accounts: accounts, Cache: dead})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Lookup(context.Background(), "sip_redis_only_key")
	requireUnavailableNotRejected(t, err)

	// A key with a Postgres row still authenticates through the outage.
	acct := seedActiveAccount(accounts, "acme")
	seedKey(keys, "sip_pg_row_key", acct.ID, platform.APIKeyTierAPIKey, 60)
	if _, err := v.Lookup(context.Background(), "sip_pg_row_key"); err != nil {
		t.Fatalf("key with a Postgres row during a Redis outage: %v", err)
	}
}

// An absent key with Redis reachable is still a plain rejection.
func TestPostgresValidator_RedisUp_AbsentKeyIsUnauthorized(t *testing.T) {
	keys, accounts, rdb := newStubs()
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{Keys: keys, Accounts: accounts, Cache: rdb})
	if _, err := v.Lookup(context.Background(), "sip_absent"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestPostgresValidator_AccountReadFailureIsUnavailable(t *testing.T) {
	keys, accounts, _ := newStubs()
	acct := seedActiveAccount(accounts, "acme")
	seedKey(keys, "sip_pg_row_key", acct.ID, platform.APIKeyTierAPIKey, 60)
	v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys: keys, Accounts: failingAccountStore{accounts},
	})
	_, err := v.Lookup(context.Background(), "sip_pg_row_key")
	requireUnavailableNotRejected(t, err)
}

// failCanonicalGet fails every GET of a canonical `apikey:` record; the derived `apikey-cache:` row stays readable.
type failCanonicalGet struct{}

func (failCanonicalGet) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failCanonicalGet) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if args := cmd.Args(); cmd.Name() == "get" && len(args) == 2 {
			if k, _ := args[1].(string); strings.HasPrefix(k, "apikey:") {
				err := errors.New("redis: i/o timeout")
				cmd.SetErr(err)
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (failCanonicalGet) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// An unread canonical record may be the one carrying revoked_at, so the derived cache row (whose best-effort
// delete may have failed) must not be trusted in its place: Postgres decides.
func TestPostgresValidator_CanonicalUnreadSkipsDerivedCacheRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*stubKeyStore, *stubAccountStore, platform.APIKey, platform.Account)
		wantErr error
	}{
		{"RevokedRowRefused", func(k *stubKeyStore, _ *stubAccountStore, rec platform.APIKey, _ platform.Account) {
			rec.RevokedAt = time.Now()
			k.byID[rec.ID], k.byHash[hexHashOf("sip_canon_unread")] = rec, rec
		}, auth.ErrUnauthorized},
		{"SuspendedAccountRefused", func(_ *stubKeyStore, a *stubAccountStore, _ platform.APIKey, acct platform.Account) {
			acct.Status = platform.AccountSuspended
			a.byID[acct.ID] = acct
		}, auth.ErrUnauthorized},
		{"ValidRowAccepted", func(*stubKeyStore, *stubAccountStore, platform.APIKey, platform.Account) {}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, accounts, rdb := newStubs()
			v, _ := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{Keys: keys, Accounts: accounts, Cache: rdb})
			acct := seedActiveAccount(accounts, "canon")
			rec := seedKey(keys, "sip_canon_unread", acct.ID, platform.APIKeyTierAPIKey, 1000)
			for i := range 2 { // miss then hit: the derived cache row exists and the account status is cached
				if _, err := v.Lookup(context.Background(), "sip_canon_unread"); err != nil {
					t.Fatalf("lookup %d: %v", i, err)
				}
			}
			tc.change(keys, accounts, rec, acct)
			rdb.(*redis.Client).AddHook(failCanonicalGet{})

			_, err := v.Lookup(context.Background(), "sip_canon_unread")
			if (tc.wantErr == nil && err != nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("lookup with the canonical record unread = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
