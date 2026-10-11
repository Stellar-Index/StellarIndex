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

// RedisPasskeyCeremonyGuard is the Redis-SETNX adapter for the
// dashboardauth PasskeyCeremonyGuard seam. It records every spent WebAuthn
// ceremony challenge so a captured `finish-login` / `finish-register`
// request cannot be replayed into a second session or credential. Redis,
// not process memory, so the spent-set is shared across API instances
// (same shape as [sep10.RedisReplayGuard]).
//
// Key layout: `passkey:ceremony:<sha256-hex>`, a digest built by the
// handler so no challenge material reaches Redis in the clear. The prefix
// must be in the Redis ACL allow-list (configs/ansible/roles/redis-sentinel/
// templates/users.acl.j2), or lockdown ACL-denies the SETNX and sign-in fails.
//
// Eviction hardening: R1 runs `maxmemory-policy allkeys-lru`, which evicts
// ANY key under memory pressure whatever its TTL. A bare SETNX
// spent-marker is unsafe: once evicted, a replayed `finish-login`
// re-claims the slot and mints a SECOND session for the victim. To fail
// CLOSED, production uses [RedisPasskeyCeremonyGuard.Reserve] +
// [RedisPasskeyCeremonyGuard.ClaimReserved]: the claim REQUIRES the
// reservation made at begin to still exist, so an evicted marker refuses
// the sign-in. [RedisPasskeyCeremonyGuard.Consume] is interface-only.
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

// Reserve records, at ceremony BEGIN, that a challenge is live and
// redeemable exactly once. It is the first half of the eviction-safe
// single-use protocol (see the type doc and [RedisPasskeyCeremonyGuard.ClaimReserved]):
// because the later claim requires this marker to still exist, an
// allkeys-lru eviction of it makes the sign-in fail CLOSED rather than
// re-open the replay window. ttl must outlive the ceremony's own
// validity so the reservation never expires under a still-valid
// challenge. Every begin mints a fresh 32-byte random challenge, so a
// pre-existing marker (SETNX false) is never a legitimate re-begin: it
// returns [ErrPasskeyCeremonyAlreadyReserved] and the caller refuses to
// issue a challenge whose single-use reservation it did not create.
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

// ClaimReserved spends a ceremony reserved by [RedisPasskeyCeremonyGuard.Reserve].
// It returns (true, nil) for the caller that removes the live marker —
// the one and only presentation that may mint a session — and (false,
// nil) when the marker is ABSENT for ANY reason: already claimed (a
// replay), never reserved, or evicted under memory pressure. Treating
// absence as "refuse" is what makes eviction fail closed: unlike the
// SETNX spent-set, an evicted marker cannot be re-claimed, because the claim needs
// the marker present rather than absent. DEL is atomic, so two
// concurrent presentations of one captured request resolve to exactly
// one claimant. A store outage surfaces as an error; the caller fails
// closed on it just as it does for [RedisPasskeyCeremonyGuard.Consume].
func (g *RedisPasskeyCeremonyGuard) ClaimReserved(ctx context.Context, digest string) (bool, error) {
	key := liveCeremonyKey(digest)
	removed, err := g.rdb.Del(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("redis del %s: %w", key, err)
	}
	return removed == 1, nil
}

// Consume implements the dashboardauth.PasskeyCeremonyGuard contract:
// (true, nil) when this caller claimed the ceremony first, (false,
// nil) when it was already spent, and (false, err) when Redis could
// not be reached. The caller fails CLOSED on the error — an
// unverifiable freshness claim must not mint a session.
func (g *RedisPasskeyCeremonyGuard) Consume(ctx context.Context, digest string, ttl time.Duration) (bool, error) {
	key := passkeyCeremonyKey(digest)
	ok, err := g.rdb.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx %s: %w", key, err)
	}
	return ok, nil
}
