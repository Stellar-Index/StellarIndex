// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// TestCreateCapped_ChildKeySlidingIdleTTL — a self-service child is
// written with the idle TTL, and a validated Lookup slides it back up.
func TestCreateCapped_ChildKeySlidingIdleTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	parent := Subject{Identifier: AccountIdentifier("child-ttl"), Tier: TierAPIKey, KeyID: "kid_parent"}
	_, plaintext, err := NewRedisAPIKeyStore(rdb).CreateCapped(ctx, ChildKeyRequest(parent, "rotated", nil), 25)
	if err != nil {
		t.Fatalf("CreateCapped: %v", err)
	}
	key := cachekeys.APIKey(hashAPIKey(plaintext)).String()
	if ttl := mr.TTL(key); ttl <= 0 || ttl > MirroredKeyIdleTTL {
		t.Fatalf("child key TTL = %s, want in (0, %s]: a self-service key must age out once abandoned", ttl, MirroredKeyIdleTTL)
	}

	mr.FastForward(80 * 24 * time.Hour)
	if _, err := NewRedisAPIKeyValidator(rdb).Lookup(ctx, plaintext); err != nil {
		t.Fatalf("Lookup(child key): %v", err)
	}
	if ttl := mr.TTL(key); ttl < MirroredKeyIdleTTL-time.Minute {
		t.Fatalf("child key TTL after Lookup = %s, want ~%s: use must slide the idle window", ttl, MirroredKeyIdleTTL)
	}
}

// TestCreate_OperatorIssuedKeyStaysPersistent — a request that is not
// SelfService (ops CLI, admin mint) keeps no Redis expiry.
func TestCreate_OperatorIssuedKeyStaysPersistent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	_, plaintext, err := NewRedisAPIKeyStore(rdb).Create(context.Background(), CreateAPIKeyRequest{Identifier: "ops-seeded", Tier: TierOperator})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	key := cachekeys.APIKey(hashAPIKey(plaintext)).String()
	if !mr.Exists(key) {
		t.Fatalf("record %s not written", key)
	}
	if ttl := mr.TTL(key); ttl != 0 {
		t.Fatalf("operator-issued key TTL = %s, want none", ttl)
	}
}

// TestCreate_RefusesUnmintableTier — the store refuses a tier with no
// rate-limit policy, and writes nothing.
func TestCreate_RefusesUnmintableTier(t *testing.T) {
	for _, tier := range []Tier{TierAnonymous, "gold"} {
		t.Run(string(tier), func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })

			_, plaintext, err := NewRedisAPIKeyStore(rdb).Create(context.Background(), CreateAPIKeyRequest{Identifier: "acct:x", Tier: tier})
			if err == nil {
				t.Fatalf("Create(tier=%q) succeeded, want refusal", tier)
			}
			if plaintext != "" {
				t.Error("refused Create surfaced a plaintext")
			}
			if keys := mr.Keys(); len(keys) != 0 {
				t.Errorf("refused Create wrote %v", keys)
			}
		})
	}
}
