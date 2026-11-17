// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
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
