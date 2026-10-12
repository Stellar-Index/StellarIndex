package auth

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// RedisKeyCacheInvalidator evicts one API-key record from the Redis cache the Postgres validator populates; WIRING HAZARD: under the redis backend its DEL target is the canonical credential, so construct only via NewKeyCacheInvalidatorForBackend (nil/receiver-nil calls no-op).
type RedisKeyCacheInvalidator struct {
	cache redis.Cmdable
}

// BackendPostgres is the `[api].auth_backend` value running PostgresAPIKeyValidator with a rebuildable `apikey-cache:<hash>` cache; any other value (default "redis") runs RedisAPIKeyValidator, where `apikey:<hash>` is canonical.
const BackendPostgres = "postgres"

// NewKeyCacheInvalidatorForBackend returns the invalidator for authBackend, or nil when there is no separate cache (only BackendPostgres gets one; assign to an interface only when non-nil to avoid a typed-nil).
func NewKeyCacheInvalidatorForBackend(authBackend string, cache redis.Cmdable) *RedisKeyCacheInvalidator {
	if authBackend != BackendPostgres || cache == nil {
		return nil
	}
	return NewRedisKeyCacheInvalidator(cache)
}

// NewRedisKeyCacheInvalidator returns an invalidator over cache (nil makes Invalidate a no-op); production goes through NewKeyCacheInvalidatorForBackend, this form is for callers that already established the postgres backend.
func NewRedisKeyCacheInvalidator(cache redis.Cmdable) *RedisKeyCacheInvalidator {
	return &RedisKeyCacheInvalidator{cache: cache}
}

// InvalidateCachedKey deletes the records keyed by the SHA-256 hex hash of the plaintext; idempotent, and a no-op when the cache or receiver is nil.
func (i *RedisKeyCacheInvalidator) InvalidateCachedKey(ctx context.Context, hexHash string) error {
	if i == nil || i.cache == nil {
		return nil
	}
	return evictCachedKey(ctx, i.cache, hexHash)
}

// apiKeyCacheTombstoneTTL outlives any in-flight cache-miss Lookup: the
// default request timeout is 15s and the server write timeout 30s.
const apiKeyCacheTombstoneTTL = 60 * time.Second

// evictCachedKey DELs both records the postgres validator can serve from: `apikey-cache:` and the canonical `apikey:` it reads first, as separate commands so an ACL lacking `apikey-cache:*` cannot fail the canonical eviction.
// The tombstone is written first so a concurrent cache-miss Lookup that read Postgres before the change cannot re-cache the stale row (see cacheStore).
func evictCachedKey(ctx context.Context, rdb redis.Cmdable, hexHash string) error {
	return errors.Join(
		rdb.Set(ctx, cachekeys.APIKeyCacheEvicted(hexHash).String(), "1", apiKeyCacheTombstoneTTL).Err(),
		rdb.Del(ctx, cachekeys.APIKey(hexHash).String()).Err(),
		rdb.Del(ctx, cachekeys.APIKeyCache(hexHash).String()).Err(),
	)
}
