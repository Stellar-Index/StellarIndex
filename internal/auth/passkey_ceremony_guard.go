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

// RedisPasskeyCeremonyGuard records spent WebAuthn challenges so a captured finish-login/finish-register cannot be replayed; production uses Reserve + ClaimReserved so an allkeys-lru eviction fails CLOSED.
type RedisPasskeyCeremonyGuard struct {
	rdb redis.Cmdable
}

// NewRedisPasskeyCeremonyGuard constructs a guard; rdb MUST be non-nil.
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

// liveCeremonyKey names the begin-time reservation marker for a ceremony digest. Distinct from the spent-marker passkeyCeremonyKey writes
// for [RedisPasskeyCeremonyGuard.Consume], but under the same `passkey:ceremony:` prefix so no Redis ACL change is needed.
func liveCeremonyKey(digest string) string {
	return "passkey:ceremony:live:" + digest
}

// Reserve records at ceremony BEGIN that a challenge is live and redeemable once; ttl must outlive the ceremony, and an existing marker returns ErrPasskeyCeremonyAlreadyReserved.
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

// ErrPasskeyCeremonyAlreadyReserved reports a Reserve whose live marker already existed.
var ErrPasskeyCeremonyAlreadyReserved = errors.New("passkey ceremony already reserved")

// ClaimReserved spends a reserved ceremony: (true, nil) only for the one caller whose atomic DEL removed the marker, (false, nil) when it is absent for any reason (which fails eviction closed), error on store outage.
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
