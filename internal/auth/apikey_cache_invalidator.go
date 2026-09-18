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
// OUTSIDE the dashboard's Revoke handler — e.g. the admin tier
// clamp, which rewrites `RateLimitPerMin` on the Postgres
// `api_keys` rows. Without an eviction there, a deployment running
// `auth_backend=postgres` keeps serving the pre-change rate-limit
// budget from the validator's read-through cache for up to the
// cache TTL (~1h) even though Postgres (the source of truth)
// already reflects the new budget. That stale window is the X6
// "API-key split-brain" audit finding class in miniature — the
// cache and the store of record disagree until the TTL rolls the
// row off.
//
// Unlike [PostgresAPIKeyValidator] this type carries NO platform
// store handles — only the cache client. It satisfies the same
// single-method `InvalidateCachedKey(ctx, hexHash)` contract as the
// validator, so either can be dropped into a bridge/handler that only
// needs eviction.
//
// WIRING HAZARD — it is NOT safe to wire wherever a Redis client is in
// scope. `apikey:<hash>` is a read-through cache ONLY under
// `auth_backend=postgres`. Under the default `auth_backend=redis` the
// very same key IS the canonical credential ([RedisAPIKeyValidator]
// reads nothing else, and a POST /v1/register credential exists there
// as the mirror written by [RedisAPIKeyStore.CreateWithSecret]), so a
// DEL is not an eviction but the irrecoverable destruction of a
// customer's key: the plaintext was shown once and the Postgres row
// holds only its hash, so nothing can rebuild the record. Construct
// through [NewKeyCacheInvalidatorForBackend], which returns nil for
// every backend but postgres.
//
// A nil cache (or a nil receiver) makes every call a no-op: a
// Redis-less deployment has nothing to invalidate, and the underlying
// Postgres write is already durable.
type RedisKeyCacheInvalidator struct {
	cache redis.Cmdable
}

// BackendPostgres is the `[api].auth_backend` value under which the
// runtime validator is [PostgresAPIKeyValidator] and `apikey:<hash>`
// is a rebuildable read-through cache. Every other value (the default
// "redis", or unset) runs [RedisAPIKeyValidator], for which that key
// is the canonical record.
const BackendPostgres = "postgres"

// NewKeyCacheInvalidatorForBackend returns the invalidator a mutation
// path outside the validator should use under authBackend, or nil when
// that backend has no separate key cache to evict.
//
// Only [BackendPostgres] gets one. Under the redis backend the
// canonical record is rewritten in place by the store
// ([RedisAPIKeyStore.UpdateRateLimit] and friends) and account-level
// state is enforced by the validator's own account-status gate, so
// there is nothing to evict — and the only thing a DEL could hit is
// the credential itself (see the wiring hazard on
// [RedisKeyCacheInvalidator]).
//
// Callers holding the result in an interface-typed field must assign
// it only when non-nil; a typed-nil pointer in an interface defeats the
// `== nil` guards the eviction call sites use to skip (and to avoid
// counting) evictions that never happened.
func NewKeyCacheInvalidatorForBackend(authBackend string, cache redis.Cmdable) *RedisKeyCacheInvalidator {
	if authBackend != BackendPostgres || cache == nil {
		return nil
	}
	return NewRedisKeyCacheInvalidator(cache)
}

// NewRedisKeyCacheInvalidator returns an invalidator over cache.
// cache may be nil (every Invalidate becomes a no-op). Production
// wiring goes through [NewKeyCacheInvalidatorForBackend]; this
// unconditional form is for callers that have already established the
// postgres backend.
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
