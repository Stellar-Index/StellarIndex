package sep10

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// RedisReplayGuard is the production [ReplayGuard]: Challenge reserves each challenge transaction's hash and Verify spends it with an atomic DEL, so a challenge mints at most one JWT; the reservation must be PRESENT (a SETNX spent-marker would fail OPEN under allkeys-lru eviction), keyed `sep10:seen:live:<hex tx hash>` (the Redis ACL prefix) on the parsed transaction's hash ([challengeTxHash]), never a digest of the XDR string.
type RedisReplayGuard struct {
	rdb redis.UniversalClient
}

// NewRedisReplayGuard constructs a replay guard against the supplied Redis client, typically the one shared with the auth subsystem.
func NewRedisReplayGuard(rdb redis.UniversalClient) *RedisReplayGuard {
	return &RedisReplayGuard{rdb: rdb}
}

// Reserve records at challenge issuance that txHash is live and redeemable once; a pre-existing reservation is not an error (every challenge carries a random nonce), but Redis errors propagate so issuance fails closed.
func (g *RedisReplayGuard) Reserve(ctx context.Context, txHash string, ttl time.Duration) error {
	key := redisReplayKey(txHash)
	if _, err := g.rdb.SetNX(ctx, key, "1", ttl).Result(); err != nil {
		return fmt.Errorf("sep10 replay-guard: SETNX %s: %w", key, err)
	}
	return nil
}

// Claim spends the reservation for txHash: [auth.ErrUnauthorized] when absent for any reason (redeemed, never issued, expired, evicted); other Redis errors propagate so callers can tell refused from unchecked. DEL is atomic, so concurrent submissions yield one claimant.
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

// ErrReplayGuardUnavailable is returned when a nil ReplayGuard was configured where one is required, so wiring can fail loud at startup rather than silently disabling replay defence.
var ErrReplayGuardUnavailable = errors.New("sep10: ReplayGuard required but not configured")
