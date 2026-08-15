// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// seedMirroredRecord writes a minimal valid mirrored credential at
// apikey:<hash> with the given Redis TTL (0 = no expiry), bypassing the
// store so a test can pin the exact starting TTL and isolate the
// validator's read-path behaviour.
func seedMirroredRecord(t *testing.T, rdb redis.Cmdable, plaintext string, ttl time.Duration) {
	t.Helper()
	body, err := json.Marshal(APIKeyRecord{
		KeyID:          "kid_refresh",
		Identifier:     AccountIdentifier("reg-refresh"),
		Tier:           TierAPIKey,
		PermissionsAll: true,
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	key := cachekeys.APIKey(hashAPIKey(plaintext)).String()
	if err := rdb.Set(context.Background(), key, body, ttl).Err(); err != nil {
		t.Fatalf("seed SET: %v", err)
	}
}

func mirroredTTL(t *testing.T, rdb redis.Cmdable, plaintext string) time.Duration {
	t.Helper()
	ttl, err := rdb.TTL(context.Background(), cachekeys.APIKey(hashAPIKey(plaintext)).String()).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	return ttl
}

// TestCreateWithSecret_WritesBoundedIdleTTL proves the register mirror is
// no longer written with TTL=0 (permanent). A permanent record in an
// allkeys-lru pool that open, anonymous registration can create without
// bound is the W1-flow-register-2 growth defect. RED on the pre-fix
// Set(...,0): TTL reports -1 (no expiry).
func TestCreateWithSecret_WritesBoundedIdleTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const plaintext = "sip_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	store := NewRedisAPIKeyStore(rdb)
	if err := store.CreateWithSecret(context.Background(), MirroredKey{
		Plaintext:  plaintext,
		KeyID:      "kid_ttl",
		Identifier: AccountIdentifier("reg-ttl"),
	}); err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}

	ttl := mirroredTTL(t, rdb, plaintext)
	if ttl <= 0 {
		t.Fatalf("mirrored key TTL = %s (<=0 means no expiry) — an open-registration credential that never ages out grows the allkeys-lru keyspace forever (W1-flow-register-2)", ttl)
	}
	if ttl > MirroredKeyIdleTTL {
		t.Errorf("mirrored key TTL = %s, want <= MirroredKeyIdleTTL (%s)", ttl, MirroredKeyIdleTTL)
	}
}

// TestRedisValidator_RefreshesIdleTTLOnUse proves the read-path re-warm:
// a successful Lookup slides the idle window forward. Without it a
// register-mirrored key HARD-EXPIRES at mint+TTL even while actively
// used — the exact "valid key -> permanent silent 401" the fix must not
// re-introduce. RED on the pre-fix bare-GET Lookup: the TTL keeps
// decaying because nothing re-warms it.
func TestRedisValidator_RefreshesIdleTTLOnUse(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const idle = 100 * time.Second
	plaintext := "sip_" + strings.Repeat("a", 64)
	seedMirroredRecord(t, rdb, plaintext, idle)

	// Age the key most of the way to expiry, then use it.
	mr.FastForward(80 * time.Second) // ~20s of TTL left
	if before := mirroredTTL(t, rdb, plaintext); before > 25*time.Second {
		t.Fatalf("precondition: TTL should have decayed to ~20s, got %s", before)
	}

	v := NewRedisAPIKeyValidator(rdb, WithMirroredKeyIdleTTL(idle))
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("Lookup(active key): %v", err)
	}

	if after := mirroredTTL(t, rdb, plaintext); after <= 90*time.Second {
		t.Fatalf("idle TTL not re-warmed on use: TTL after Lookup = %s, want ~%s — an actively-used key must never hard-expire (W1-flow-register-2)", after, idle)
	}
}

// TestRedisValidator_ActivelyUsedKeyDoesNotExpire is the end-to-end
// regression the AUTH-3 rejection demanded: a key used continuously over
// a span LONGER than one idle window must never 401. RED on the pre-fix
// validator: the key expires mid-sequence and Lookup returns
// ErrUnauthorized.
func TestRedisValidator_ActivelyUsedKeyDoesNotExpire(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const idle = 100 * time.Second
	plaintext := "sip_" + strings.Repeat("b", 64)
	seedMirroredRecord(t, rdb, plaintext, idle)

	v := NewRedisAPIKeyValidator(rdb, WithMirroredKeyIdleTTL(idle))

	// Three 60s hops => 180s total, well past the 100s idle window. Each
	// hop is < idle, so a key re-warmed on the previous use is always
	// still alive when used again.
	for i := 0; i < 3; i++ {
		mr.FastForward(60 * time.Second)
		if _, err := v.Lookup(context.Background(), plaintext); err != nil {
			t.Fatalf("hop %d (t=%ds): actively-used key rejected (%v) — a live credential hard-expired on a timer, the W1-flow-register-2 defect", i, (i+1)*60, err)
		}
	}
}

// TestRedisValidator_PersistentKeyKeepsNoTTL guards the blast radius:
// refresh-on-use is EXPIRE ... XX, so it must NOT graft a TTL onto a
// record written WITHOUT one (operator-seeded / self-service keys, which
// are deliberately permanent). Passes on both pre- and post-fix code;
// its job is to keep a future "just GETEX everything" simplification from
// silently making every persistent key evictable.
func TestRedisValidator_PersistentKeyKeepsNoTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	plaintext := "sip_" + strings.Repeat("c", 64)
	seedMirroredRecord(t, rdb, plaintext, 0) // no expiry

	v := NewRedisAPIKeyValidator(rdb, WithMirroredKeyIdleTTL(100*time.Second))
	if _, err := v.Lookup(context.Background(), plaintext); err != nil {
		t.Fatalf("Lookup(persistent key): %v", err)
	}
	if ttl := mirroredTTL(t, rdb, plaintext); ttl > 0 {
		t.Errorf("persistent (TTL-less) key gained a TTL of %s on use — refresh-on-use must only re-warm keys that already carry one", ttl)
	}
}
