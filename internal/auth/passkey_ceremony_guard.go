// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisPasskeyCeremonyGuard is the Redis adapter for dashboardauth.PasskeyCeremonyGuard: it records spent WebAuthn challenges (key `passkey:ceremony:<sha256-hex>`, digest built by the handler) so a captured finish-login/finish-register cannot be replayed.
// The prefix must be in the Redis ACL allow-list (users.acl.j2) or sign-in fails. Redis runs allkeys-lru, so a bare SETNX spent-marker could be evicted and replayed into a SECOND session;
// production therefore uses Reserve + ClaimReserved, where the claim REQUIRES the begin-time marker, so eviction fails CLOSED. Consume is interface-only.
type RedisPasskeyCeremonyGuard struct {
	rdb redis.Cmdable
}

// NewRedisPasskeyCeremonyGuard constructs a guard. rdb MUST be
// non-nil — the api binary only wires this when Redis is reachable,
// and Redis-less deployments leave the dashboardauth field nil so
// that package's in-process default takes over.
func NewRedisPasskeyCeremonyGuard(rdb redis.Cmdable) *RedisPasskeyCeremonyGuard {
	if rdb == nil {
		panic("auth: NewRedisPasskeyCeremonyGuard: rdb must not be nil")
	}
	return &RedisPasskeyCeremonyGuard{rdb: rdb}
}

// passkeyCeremonyKey returns the Redis key for a ceremony digest.
// Sole-builder pattern, matching signupLockKey / redisReplayKey.
func passkeyCeremonyKey(digest string) string {
	return "passkey:ceremony:" + digest
}

// liveCeremonyKey names the begin-time reservation marker for a
// ceremony digest. Distinct from the spent-marker passkeyCeremonyKey
// writes for [RedisPasskeyCeremonyGuard.Consume], but under the same `passkey:ceremony:`
// prefix so no Redis ACL allow-list change is needed.
func liveCeremonyKey(digest string) string {
	return "passkey:ceremony:live:" + digest
}

// Reserve records at ceremony BEGIN that a challenge is live and redeemable once; ClaimReserved needs this marker, so an allkeys-lru eviction fails the sign-in closed.
// ttl must outlive the ceremony's validity. A pre-existing marker (challenges are fresh 32-byte randoms) returns ErrPasskeyCeremonyAlreadyReserved and the caller must not issue the challenge.
func (g *RedisPasskeyCeremonyGuard) Reserve(ctx context.Context, digest string, ttl time.Duration) error {
	key := liveCeremonyKey(digest)
	created, err := g.rdb.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return fmt.Errorf("redis setnx %s: %w", key, err)
	}
	if !created {
		return fmt.Errorf("%w: %s", ErrPasskeyCeremonyAlreadyReserved, key)
	}
	return nil
}

// ErrPasskeyCeremonyAlreadyReserved reports a Reserve whose live marker
// already existed.
var ErrPasskeyCeremonyAlreadyReserved = errors.New("passkey ceremony already reserved")

// ClaimReserved spends a reserved ceremony: (true, nil) for the one caller whose DEL removed the marker, (false, nil) when it is absent for ANY reason (replayed, never reserved, evicted), which is what makes eviction fail closed.
// DEL is atomic, so concurrent presentations yield one claimant; a store outage is an error and the caller fails closed.
func (g *RedisPasskeyCeremonyGuard) ClaimReserved(ctx context.Context, digest string) (bool, error) {
	key := liveCeremonyKey(digest)
	removed, err := g.rdb.Del(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("redis del %s: %w", key, err)
	}
	return removed == 1, nil
}

// Consume implements dashboardauth.PasskeyCeremonyGuard: (true, nil) if this caller claimed first, (false, nil) if already spent, (false, err) if Redis is unreachable; the caller fails CLOSED on error.
func (g *RedisPasskeyCeremonyGuard) Consume(ctx context.Context, digest string, ttl time.Duration) (bool, error) {
	key := passkeyCeremonyKey(digest)
	ok, err := g.rdb.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx %s: %w", key, err)
	}
	return ok, nil
}
