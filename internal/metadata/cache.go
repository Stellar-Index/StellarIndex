package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"github.com/RatesEngine/rates-engine/internal/cachekeys"
)

// Cache wraps a [Resolver] with Redis-backed value caching +
// in-process singleflight. Read-through + coalesced-miss is the
// access pattern: many concurrent API requests for the same
// home-domain should share one upstream fetch.
//
// TTL is [cachekeys.TOMLTTL] (15 minutes). Negative results are NOT
// cached — a 404 from the issuer is a real signal callers should
// see, and a transient one at that.
//
// Safe for concurrent use.
type Cache struct {
	resolver *Resolver
	rdb      redis.Cmdable
	ttl      time.Duration
	sf       singleflight.Group
}

// NewCache constructs a cache-wrapped resolver.
//
// rdb may be nil for local/test runs that want no caching — the
// cache falls through to the resolver every call. Production always
// provides a live redis client.
func NewCache(resolver *Resolver, rdb redis.Cmdable) *Cache {
	return &Cache{
		resolver: resolver,
		rdb:      rdb,
		ttl:      cachekeys.TOMLTTL,
	}
}

// Resolve returns a cached SEP1 or fetches + caches a fresh one.
//
// Flow: Redis GET → hit: decode + return. Miss: acquire singleflight
// slot, re-check Redis (another goroutine may have won the race),
// fetch upstream, SET with [cachekeys.TOMLTTL], return.
func (c *Cache) Resolve(ctx context.Context, domain string) (*SEP1, error) {
	key := cachekeys.TOML(domain)

	if sep, ok := c.getCached(ctx, key); ok {
		return sep, nil
	}

	v, err, _ := c.sf.Do(key, func() (any, error) {
		// Re-check inside the singleflight slot: if another caller
		// already populated while we were queued, skip the upstream
		// fetch entirely.
		if sep, ok := c.getCached(ctx, key); ok {
			return sep, nil
		}

		sep, err := c.resolver.Resolve(ctx, domain)
		if err != nil {
			return nil, err
		}
		c.setCached(ctx, key, sep)
		return sep, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*SEP1), nil
}

func (c *Cache) getCached(ctx context.Context, key string) (*SEP1, bool) {
	if c.rdb == nil {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		// Any error — key-miss (redis.Nil), connection blip, decode
		// failure — falls through to upstream. Cache is an
		// optimisation, never a hard dependency.
		return nil, false
	}
	sep := &SEP1{}
	if err := json.Unmarshal(b, sep); err != nil {
		return nil, false
	}
	return sep, true
}

func (c *Cache) setCached(ctx context.Context, key string, sep *SEP1) {
	if c.rdb == nil {
		return
	}
	b, err := json.Marshal(sep)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, key, b, c.ttl).Err()
}

// Invalidate drops the cached entry for domain. Called when an
// operator indicates a fresh fetch is needed (e.g., asset metadata
// admin action).
func (c *Cache) Invalidate(ctx context.Context, domain string) error {
	if c.rdb == nil {
		return nil
	}
	if err := c.rdb.Del(ctx, cachekeys.TOML(domain)).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("metadata: invalidate %q: %w", domain, err)
	}
	return nil
}
