// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisPasskeyCeremonyGuard is the Redis-SETNX adapter for the
// dashboardauth PasskeyCeremonyGuard seam (audit-2026-08-13).
//
// It records every WebAuthn ceremony challenge that has been spent so
// a captured `finish-login` / `finish-register` request — ceremony
// cookie plus the authenticator's response, both of which are just
// bytes on the wire — cannot be replayed into a second session or a
// second credential. Redis rather than process memory so the
// spent-set is shared: with two API instances behind the proxy, an
// in-process set would happily let the replay through on the other
// instance.
//
// Same shape and same reasoning as [sep10.RedisReplayGuard] (F-1224),
// which does this for SEP-10 challenge transactions.
//
// Key layout: `passkey:ceremony:<sha256-hex>` — the digest is built
// by the dashboardauth handler, so no challenge material reaches
// Redis in the clear. NOTE for operators: this prefix must appear in
// the Redis ACL key allow-list
// (configs/ansible/roles/redis-sentinel/templates/users.acl.j2), or a
// lockdown deployment authenticates fine and then ACL-denies the
// SETNX — which the handler treats as "cannot prove freshness" and
// refuses the sign-in.
//
// Eviction caveat: R1 runs `maxmemory-policy allkeys-lru`, so under
// memory pressure a spent-marker can be evicted before its TTL. The
// exposure that opens is bounded by the ceremony's own 5-minute
// expiry, which is enforced independently of this guard.
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
