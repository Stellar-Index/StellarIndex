// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisTouchDebouncer is the Redis-SETNX adapter for middleware.TouchDebouncer: it gates APIKeyStore.TouchUsage to at most one UPDATE per (keyID, TTL) window on the hot api_keys row.
// Key `touch:apikey:<keyID>` with a sentinel value (only existence matters); the TTL covers a crash between the SETNX and TouchUsage.
type RedisTouchDebouncer struct {
	rdb redis.Cmdable
	ttl time.Duration
}

// DefaultTouchDebounceTTL is the recommended SETNX TTL when the
// caller doesn't pass a custom value. Operators reading "last
// seen" tolerate up-to-5-minute staleness; tighter intervals add
// UPDATE pressure with no product benefit.
const DefaultTouchDebounceTTL = 5 * time.Minute

// NewRedisTouchDebouncer constructs a debouncer. rdb MUST be
// non-nil — the api binary only wires this when Redis is
// reachable. Pass `0` for `ttl` to use [DefaultTouchDebounceTTL].
func NewRedisTouchDebouncer(rdb redis.Cmdable, ttl time.Duration) *RedisTouchDebouncer {
	if rdb == nil {
		panic("auth: NewRedisTouchDebouncer: rdb must not be nil")
	}
	if ttl <= 0 {
		ttl = DefaultTouchDebounceTTL
	}
	return &RedisTouchDebouncer{rdb: rdb, ttl: ttl}
}

// touchKey returns the Redis key for a debounce window, separate from the `signup:lock:` and `signup:` families; `touch:apikey:*` is pre-listed in the Redis ACL allow-list.
func touchKey(keyID string) string {
	return "touch:apikey:" + keyID
}

// ShouldTouch implements middleware.TouchDebouncer via SETNX: (true, nil) on win, (false, nil) if already touched in the window, (false, err) on Redis failure; the middleware skips on every non-success branch (last_used is bookkeeping).
func (d *RedisTouchDebouncer) ShouldTouch(ctx context.Context, keyID string) (bool, error) {
	if keyID == "" {
		return false, nil
	}
	ok, err := d.rdb.SetNX(ctx, touchKey(keyID), "1", d.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis setnx %s: %w", touchKey(keyID), err)
	}
	return ok, nil
}
