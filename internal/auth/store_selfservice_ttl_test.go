// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

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

// The rate and scope bounds live in the store, so every mint and re-budget
// surface (admin HTTP, dashboard, ops CLI) inherits them.
func TestCreate_EnforcesRateAndScopeBounds(t *testing.T) {
	s, _, _ := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		req  CreateAPIKeyRequest
		want string
	}{
		{"rate-above-ceiling", CreateAPIKeyRequest{Identifier: "acme", RateLimitPerMin: MaxKeyRateLimitPerMin + 1}, "must be in [0, 100000]"},
		{"negative-rate", CreateAPIKeyRequest{Identifier: "acme", RateLimitPerMin: -1}, "must be in [0, 100000]"},
		{"wildcard-scope", CreateAPIKeyRequest{Identifier: "acme", Scopes: []string{"*"}}, "unknown key scope"},
		{"unknown-scope", CreateAPIKeyRequest{Identifier: "acme", Scopes: []string{"read", "root"}}, "unknown key scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, plaintext, err := s.Create(ctx, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create(%+v) = (%s, %v), want an error containing %q", tc.req, rec.KeyID, err, tc.want)
			}
			if plaintext != "" {
				t.Error("a refused Create returned a plaintext")
			}
		})
	}
	if keys, err := s.ListKeysForIdentifier(ctx, "acme"); err != nil || len(keys) != 0 {
		t.Fatalf("refused creates persisted %d key(s) (err %v)", len(keys), err)
	}

	rec, _, err := s.Create(ctx, CreateAPIKeyRequest{Identifier: "acme", RateLimitPerMin: MaxKeyRateLimitPerMin, Scopes: []string{"read", "account"}})
	if err != nil {
		t.Fatalf("in-bounds Create: %v", err)
	}
	if _, err := s.UpdateRateLimit(ctx, rec.KeyID, MaxKeyRateLimitPerMin+1); err == nil || !strings.Contains(err.Error(), "must be in [0, 100000]") {
		t.Fatalf("UpdateRateLimit above the ceiling = %v, want a bound error", err)
	}
	got, err := s.GetByKeyID(ctx, rec.KeyID)
	if err != nil || got.RateLimitPerMin != MaxKeyRateLimitPerMin {
		t.Fatalf("GetByKeyID = (%d, %v), want the unchanged %d", got.RateLimitPerMin, err, MaxKeyRateLimitPerMin)
	}
}
