package auth

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// RedisKeyCacheInvalidator evicts a single API-key record from the
// Redis read-through cache that [PostgresAPIKeyValidator] populates.
//
// It is the write-side counterpart for key-mutation paths that live
// OUTSIDE the dashboard's Revoke handler — most importantly the
// Stripe tier-upgrade webhook, which rewrites `RateLimitPerMin` on
// the Postgres `api_keys` rows. Without an eviction there, a
// deployment running `auth_backend=postgres` keeps serving the
// pre-upgrade rate-limit budget from the validator's read-through
// cache for up to the cache TTL (~1h) even though Postgres (the
// source of truth) already reflects the new plan. That stale window
// is the X6 "API-key split-brain" audit finding class in miniature —
// the cache and the store of record disagree until the TTL rolls the
// row off.
//
// Unlike [PostgresAPIKeyValidator] this type carries NO platform
// store handles — only the cache client — so it can be wired at any
// point where a Redis client is in scope (e.g. the Stripe webhook
// bundle, which is constructed before the dashboard bundle owns the
// full validator). It satisfies the same single-method
// `InvalidateCachedKey(ctx, hexHash)` contract as the validator, so
// either can be dropped into a bridge/handler that only needs
// eviction.
//
// A nil cache (or a nil receiver) makes every call a no-op: a
// Redis-less deployment — or one on `auth_backend=redis`, where the
// canonical record already lives in Redis and there is no separate
// read-through cache to evict — has nothing to invalidate, and the
// underlying Postgres write is already durable.
type RedisKeyCacheInvalidator struct {
	cache redis.Cmdable
}

// NewRedisKeyCacheInvalidator returns an invalidator over cache.
// cache may be nil (every Invalidate becomes a no-op).
func NewRedisKeyCacheInvalidator(cache redis.Cmdable) *RedisKeyCacheInvalidator {
	return &RedisKeyCacheInvalidator{cache: cache}
}

// InvalidateCachedKey deletes the read-through cache entry keyed by
// the SHA-256 hex hash of the plaintext (the same `apikey:<hash>`
// shape the validator reads). No-op when the cache (or the receiver)
// is nil. Idempotent — deleting an absent key is not an error.
func (i *RedisKeyCacheInvalidator) InvalidateCachedKey(ctx context.Context, hexHash string) error {
	if i == nil || i.cache == nil {
		return nil
	}
	return i.cache.Del(ctx, cachekeys.APIKey(hexHash).String()).Err()
}
