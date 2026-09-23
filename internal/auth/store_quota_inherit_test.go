// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newQuotaInheritStore(t *testing.T, opts ...StoreOption) (*RedisAPIKeyStore, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisAPIKeyStore(rdb, opts...), rdb
}

func mintForQuotaTest(t *testing.T, store *RedisAPIKeyStore, req CreateAPIKeyRequest) (APIKeyRecord, string) {
	t.Helper()
	rec, plaintext, err := store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create(%s): %v", req.Identifier, err)
	}
	return rec, plaintext
}

// TestStoreCreate_UnsetQuotaInheritsIdentifierCeiling — a mint path that
// builds its request without a ceiling (POST /v1/admin/keys, the ops
// mint-key CLI) must still issue a METERED key to an identifier whose
// credentials are metered: the usage counter is shared per identifier,
// so an unmetered sibling bills the plan without limit. The most
// generous existing ceiling wins, so the mint neither lifts nor tightens
// the plan, and another identifier's ceiling never leaks across.
func TestStoreCreate_UnsetQuotaInheritsIdentifierCeiling(t *testing.T) {
	store, rdb := newQuotaInheritStore(t)

	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:metered", MonthlyQuota: 100_000})
	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:metered", MonthlyQuota: 250_000})
	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:other", MonthlyQuota: 9_000_000})

	rec, plaintext := mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:metered", Label: "operator-minted"})
	if rec.MonthlyQuota != 250_000 {
		t.Fatalf("record MonthlyQuota = %d, want 250000 (the identifier's most generous existing ceiling)",
			rec.MonthlyQuota)
	}
	sub, err := NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.MonthlyQuota != 250_000 {
		t.Fatalf("Subject MonthlyQuota = %d, want 250000", sub.MonthlyQuota)
	}
}

// TestStoreCreate_UnsetQuotaIgnoresExpiredUnmeteredRecord — an expired
// unmetered record is not a live grant, so it cannot make the
// identifier's plan look unmetered.
func TestStoreCreate_UnsetQuotaIgnoresExpiredUnmeteredRecord(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store, _ := newQuotaInheritStore(t, WithStoreClock(func() time.Time { return now }))

	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:mixed", MonthlyQuota: 250_000})
	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:mixed", ExpiresAt: now.Add(-time.Hour)})
	rec, _ := mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:mixed"})
	if rec.MonthlyQuota != 250_000 {
		t.Fatalf("MonthlyQuota = %d, want 250000", rec.MonthlyQuota)
	}
}

// TestStoreCreate_UnsetQuotaStaysUncappedBesideLiveUnmeteredKey — the
// ceiling stays opt-in: an identifier already holding a live unmetered
// credential is not capped by inheritance.
func TestStoreCreate_UnsetQuotaStaysUncappedBesideLiveUnmeteredKey(t *testing.T) {
	store, _ := newQuotaInheritStore(t)

	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:open"})
	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:open", MonthlyQuota: 250_000})
	rec, _ := mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:open"})
	if rec.MonthlyQuota != 0 {
		t.Fatalf("MonthlyQuota = %d, want 0 beside a live unmetered credential", rec.MonthlyQuota)
	}
}

// TestStoreCreate_UnsetQuotaIgnoresLapsedHigherCeiling — an expired key's
// larger ceiling is not the plan the identifier holds now, so it cannot
// lift the mint above the live credentials' ceiling.
func TestStoreCreate_UnsetQuotaIgnoresLapsedHigherCeiling(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store, _ := newQuotaInheritStore(t, WithStoreClock(func() time.Time { return now }))

	mintForQuotaTest(t, store, CreateAPIKeyRequest{
		Identifier: "acct:downgraded", MonthlyQuota: 1_000_000, ExpiresAt: now.Add(-time.Hour),
	})
	mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:downgraded", MonthlyQuota: 100_000})
	rec, _ := mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:downgraded"})
	if rec.MonthlyQuota != 100_000 {
		t.Fatalf("MonthlyQuota = %d, want 100000 (the live ceiling, not the lapsed one)", rec.MonthlyQuota)
	}
}

// TestStoreCreate_UnsetQuotaKeepsLapsedCeilingWithNoLiveKey — letting
// every metered key lapse must not reset the identifier to unmetered.
func TestStoreCreate_UnsetQuotaKeepsLapsedCeilingWithNoLiveKey(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store, _ := newQuotaInheritStore(t, WithStoreClock(func() time.Time { return now }))

	mintForQuotaTest(t, store, CreateAPIKeyRequest{
		Identifier: "acct:lapsed", MonthlyQuota: 250_000, ExpiresAt: now.Add(-time.Hour),
	})
	rec, _ := mintForQuotaTest(t, store, CreateAPIKeyRequest{Identifier: "acct:lapsed"})
	if rec.MonthlyQuota != 250_000 {
		t.Fatalf("MonthlyQuota = %d, want 250000", rec.MonthlyQuota)
	}
}
