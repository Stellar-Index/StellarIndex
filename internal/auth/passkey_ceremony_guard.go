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
// Eviction hardening: R1 runs `maxmemory-policy allkeys-lru`, which
// evicts ANY key (TTL or not, this namespace or not) under memory
// pressure — a longer TTL, a separate logical DB, or a volatile-lru
// namespace do NOT exempt it, because eviction is instance-wide. A
// bare SETNX spent-marker is therefore unsafe: if the marker is
// evicted before its TTL (e.g. while an attacker floods the shared
// instance with no-expiry apikey mirror writes via open registration),
// a replayed `finish-login` finds the slot free and SETNX re-claims
// it — minting a SECOND session for the victim (W1-auth-passkey-1,
// audit-2026-08-14). To make eviction fail CLOSED instead of open,
// production consumes through [RedisPasskeyCeremonyGuard.Reserve] +
// [RedisPasskeyCeremonyGuard.ClaimReserved]: the ceremony is reserved
// at begin, and the claim REQUIRES the reservation to still exist — an
// evicted (absent) marker refuses the sign-in rather than freeing it
// for a captured request. [RedisPasskeyCeremonyGuard.Consume] is
// retained for interface conformance but is not the production path.
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
// ceremony digest. Distinct from the (superseded) spent-marker
// passkeyCeremonyKey writes, but under the same `passkey:ceremony:`
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
// challenge. A challenge is 32 random bytes, so a pre-existing marker
// (SETNX returning false) is a re-begin of the same ceremony, not a
// collision — treated as already-reserved, not an error.
func (g *RedisPasskeyCeremonyGuard) Reserve(ctx context.Context, digest string, ttl time.Duration) error {
	key := liveCeremonyKey(digest)
	if _, err := g.rdb.SetNX(ctx, key, "1", ttl).Result(); err != nil {
		return fmt.Errorf("redis setnx %s: %w", key, err)
	}
	return nil
}

// ClaimReserved spends a ceremony reserved by [RedisPasskeyCeremonyGuard.Reserve].
// It returns (true, nil) for the caller that removes the live marker —
// the one and only presentation that may mint a session — and (false,
// nil) when the marker is ABSENT for ANY reason: already claimed (a
// replay), never reserved, or evicted under memory pressure. Treating
// absence as "refuse" is the fix: unlike the SETNX spent-set, an
// evicted marker can no longer be re-claimed, because the claim needs
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
