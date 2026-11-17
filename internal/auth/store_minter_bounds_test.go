// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A request that names its minter but not an expiry (the admin mint shape) must not outlive the minter:
// the bound lives in Create, so no handler can build a request that routes around it.
func TestRedisAPIKeyStore_CreateBoundsExpiryToMinter(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	minterExpiry := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	minter := Subject{Identifier: "operator:staff-rotation", Tier: TierOperator, KeyID: "kid_ops", ExpiresAt: minterExpiry}

	rec, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !rec.ExpiresAt.Equal(minterExpiry) {
		t.Fatalf("minted ExpiresAt = %v, want the minter's %v: a 30-day key must not mint a never-expiring one", rec.ExpiresAt, minterExpiry)
	}

	_, _, err = s.Create(ctx, CreateAPIKeyRequest{
		Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter, ExpiresAt: minterExpiry.Add(time.Hour),
	})
	if !errors.Is(err, ErrMintExceedsCaller) {
		t.Fatalf("expiry past the minter's: err = %v, want ErrMintExceedsCaller", err)
	}

	earlier := minterExpiry.Add(-time.Hour)
	rec, _, err = s.Create(ctx, CreateAPIKeyRequest{
		Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter, ExpiresAt: earlier,
	})
	if err != nil || !rec.ExpiresAt.Equal(earlier) {
		t.Fatalf("earlier expiry: ExpiresAt = %v, err = %v; want %v, nil", rec.ExpiresAt, err, earlier)
	}
}

// A metered minter cannot mint a key whose monthly quota exceeds its own, whether inherited or explicit.
func TestRedisAPIKeyStore_CreateBoundsMonthlyQuotaToMinter(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	minter := Subject{Identifier: "operator:staff", Tier: TierOperator, KeyID: "kid_ops", MonthlyQuota: 50_000}

	rec, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter})
	if err != nil || rec.MonthlyQuota != 50_000 {
		t.Fatalf("inherited quota: MonthlyQuota = %d, err = %v; want 50000, nil", rec.MonthlyQuota, err)
	}
	_, _, err = s.Create(ctx, CreateAPIKeyRequest{
		Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter, MonthlyQuota: 50_001,
	})
	if !errors.Is(err, ErrMintExceedsCaller) {
		t.Fatalf("explicit quota past the minter's: err = %v, want ErrMintExceedsCaller", err)
	}
	rec, _, err = s.Create(ctx, CreateAPIKeyRequest{
		Identifier: "ops-misc", Tier: TierOperator, MintedBy: &minter, MonthlyQuota: 10,
	})
	if err != nil || rec.MonthlyQuota != 10 {
		t.Fatalf("lower quota: MonthlyQuota = %d, err = %v; want 10, nil", rec.MonthlyQuota, err)
	}
}
