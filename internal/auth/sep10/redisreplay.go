package sep10

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RedisReplayGuard is the production [ReplayGuard]. [Validator.Challenge]
// reserves each issued challenge transaction's hash and
// [Validator.Verify] spends it with an atomic DEL, so a challenge mints
// at most one JWT; a second submission of the same signed XDR finds no
// reservation and returns [auth.ErrUnauthorized].
//
// Eviction hardening: R1 runs `maxmemory-policy allkeys-lru`, which can
// evict ANY key before its TTL. A SETNX spent-marker fails OPEN under
// that — an evicted marker lets a captured XDR re-claim the slot and
// mint a second JWT. Requiring the reservation to be PRESENT makes an
// eviction refuse the redemption instead (the same protocol as
// [auth.RedisPasskeyCeremonyGuard.Reserve] / ClaimReserved).
//
// Cache key:
//
//	sep10:seen:live:<hex transaction hash>
//
// The suffix is the challenge TRANSACTION's canonical hash (see
// [challengeTxHash]), never a digest of the caller-supplied XDR string:
// the same transaction has many valid spellings, and keying on the
// spelling let one redemption be replayed by re-encoding it (CON-05).
// The key stays under the `sep10:seen:` prefix the Redis ACL allow-list
// (configs/ansible/roles/redis-sentinel/templates/users.acl.j2) grants.
//
// Owned by this file rather than internal/cachekeys/ because the
// SEP-10 replay set is conceptually an auth concern, not a price-
// cache one — the cachekeys package is the price-cache namespace
// per ADR-0007.
type RedisReplayGuard struct {
	rdb redis.UniversalClient
}

// NewRedisReplayGuard constructs a replay guard against the supplied
// Redis client. Callers typically pass the same client used by the
// rest of the auth subsystem (`internal/auth/apikey_redis.go`).
func NewRedisReplayGuard(rdb redis.UniversalClient) *RedisReplayGuard {
	return &RedisReplayGuard{rdb: rdb}
}

// Reserve records at challenge issuance that txHash is live and
// redeemable once. A pre-existing reservation (SETNX false) cannot be a
// collision — every challenge carries a random nonce — so it is not an
// error. Redis errors propagate so issuance fails closed.
func (g *RedisReplayGuard) Reserve(ctx context.Context, txHash string, ttl time.Duration) error {
	key := redisReplayKey(txHash)
	if _, err := g.rdb.SetNX(ctx, key, "1", ttl).Result(); err != nil {
		return fmt.Errorf("sep10 replay-guard: SETNX %s: %w", key, err)
	}
	return nil
}

// Claim spends the reservation for txHash. It returns
// [auth.ErrUnauthorized] when the reservation is absent for any reason
// — already redeemed, never issued, expired or evicted — and propagates
// other Redis errors so callers can tell "refused" from "couldn't
// check". DEL is atomic, so concurrent submissions yield one claimant.
func (g *RedisReplayGuard) Claim(ctx context.Context, txHash string) error {
	key := redisReplayKey(txHash)
	removed, err := g.rdb.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("sep10 replay-guard: DEL %s: %w", key, err)
	}
	if removed != 1 {
		return fmt.Errorf("%w: challenge not reserved or already redeemed", auth.ErrUnauthorized)
	}
	return nil
}

// redisReplayKey returns the Redis namespace key for a hashed
// challenge tx. Sole-builder pattern matches the cachekeys/
// convention; tests can compare keys without re-deriving.
func redisReplayKey(txHash string) string {
	return "sep10:seen:live:" + txHash
}

// Compile-time interface conformance check.
var _ ReplayGuard = (*RedisReplayGuard)(nil)

// ErrReplayGuardUnavailable is returned by callers that wrapped a
// nil ReplayGuard but were configured to require one. Exposed so
// the binary wiring can fail-loud at startup rather than silently
// disabling replay defence in production.
var ErrReplayGuardUnavailable = errors.New("sep10: ReplayGuard required but not configured")
