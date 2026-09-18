// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestStoreCreate_PersistsMonthlyQuota pins the store primitive every
// mint site shares (self-service rotation, signup, admin mint, the ops
// CLI): a requested monthly ceiling reaches the persisted record, so
// the validator can map it onto the Subject the quota middleware reads.
// Pre-fix [CreateAPIKeyRequest] had no such field and every key the
// store minted was born with MonthlyQuota=0 — unmetered (RLT-404).
func TestStoreCreate_PersistsMonthlyQuota(t *testing.T) {
	const quota int64 = 1_000_000

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := NewRedisAPIKeyStore(rdb)
	rec, plaintext, err := store.Create(context.Background(), CreateAPIKeyRequest{
		Identifier:   "acct-metered",
		Label:        "metered",
		Tier:         TierAPIKey,
		MonthlyQuota: quota,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.MonthlyQuota != quota {
		t.Fatalf("record MonthlyQuota = %d, want %d", rec.MonthlyQuota, quota)
	}
	sub, err := NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.MonthlyQuota != quota {
		t.Fatalf("Subject MonthlyQuota = %d, want %d (the middleware reads this field; "+
			"zero short-circuits the ceiling entirely)", sub.MonthlyQuota, quota)
	}
}

// TestStoreCreate_NoQuotaRequestedStaysUncapped — the ceiling is
// opt-in: an unset request must not acquire one in the store.
func TestStoreCreate_NoQuotaRequestedStaysUncapped(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rec, _, err := NewRedisAPIKeyStore(rdb).Create(context.Background(), CreateAPIKeyRequest{
		Identifier: "acct-uncapped",
		Tier:       TierAPIKey,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.MonthlyQuota != 0 {
		t.Fatalf("record MonthlyQuota = %d, want 0 for an unset request", rec.MonthlyQuota)
	}
}
