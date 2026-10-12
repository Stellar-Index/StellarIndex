// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FixedWindowCounter is the "N per window" primitive behind the auth throttles: one atomic Lua INCR plus EXPIRE-on-first with a 2x window drain TTL, returning the count for the caller to compare; allows up to 2x the cap across a boundary, fine for abuse prevention, NOT for billing (use internal/usage).
type FixedWindowCounter struct {
	rdb    redis.Cmdable
	window time.Duration
	nowFn  func() time.Time
}

// NewFixedWindowCounter constructs a counter; rdb must be non-nil and window >= 1s (as in [New], since key derivation divides by int64(window.Seconds()) and would panic on a sub-second window); a nil nowFn defaults to [time.Now].
func NewFixedWindowCounter(rdb redis.Cmdable, window time.Duration, nowFn func() time.Time) *FixedWindowCounter {
	if rdb == nil {
		panic("ratelimit: NewFixedWindowCounter: rdb must not be nil")
	}
	if window < time.Second {
		panic("ratelimit: NewFixedWindowCounter: window must be >= 1s")
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &FixedWindowCounter{rdb: rdb, window: window, nowFn: nowFn}
}

// incrLua is the atomic increment: INCR, plus the drain EXPIRE on the call that created the key.
// KEYS[1] is the window-bucketed key, ARGV[1] the TTL in seconds (2x window); it returns the count.
//
// A client-side INCR-then-EXPIRE pair is non-atomic: a dropped connection, MISCONF or OOM
// between the calls leaves a TTL-less key that is never revisited (the suffix moves with the
// clock), growing the namespace without bound. Inside EVAL, Redis runs both commands or neither.
const incrLua = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return current
`

// incrScript is compiled once at package init — the Lua source is
// immutable and go-redis caches the SHA inside the value.
var incrScript = redis.NewScript(incrLua)

// Incr increments the counter behind keyBase for the current window (bucket `unix_seconds / window_seconds` appended as ":<n>") and returns the post-increment count; the 2x drain TTL is set atomically with the creating increment.
func (c *FixedWindowCounter) Incr(ctx context.Context, keyBase string) (int64, error) {
	windowStart := c.nowFn().Unix() / int64(c.window.Seconds())
	key := fmt.Sprintf("%s:%d", keyBase, windowStart)
	// Floored at 1s: Redis reads EXPIRE 0 as "no expiry", the leak this script prevents.
	ttlSeconds := int(c.window.Seconds() * 2)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}
	raw, err := incrScript.Run(ctx, c.rdb, []string{key}, ttlSeconds).Result()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: fixed-window INCR %s: %w", key, err)
	}
	count, ok := raw.(int64)
	if !ok {
		return 0, fmt.Errorf("ratelimit: fixed-window INCR %s: count not int64: %T", key, raw)
	}
	return count, nil
}

// LocalFixedWindowCounter is the in-process twin of [FixedWindowCounter] with no error path, keeping a throttle enforcing its cap per process while Redis is down instead of failing open; shares the bounded [localStore].
type LocalFixedWindowCounter struct {
	store  *localStore
	window time.Duration
	nowFn  func() time.Time
}

// NewLocalFixedWindowCounter constructs the in-process counter; window must be >= 1s (as [NewFixedWindowCounter]) and a nil nowFn defaults to [time.Now].
func NewLocalFixedWindowCounter(window time.Duration, nowFn func() time.Time) *LocalFixedWindowCounter {
	if window < time.Second {
		panic("ratelimit: NewLocalFixedWindowCounter: window must be >= 1s")
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &LocalFixedWindowCounter{store: newLocalStore(), window: window, nowFn: nowFn}
}

// Allow increments keyBase's counter for the current window and reports whether the count is within limit; safe for concurrent use.
func (c *LocalFixedWindowCounter) Allow(keyBase string, limit int) bool {
	now := c.nowFn()
	window := now.Unix() / int64(c.window.Seconds())
	_, allowed := c.store.take(keyBase, window, limit, now, c.window)
	return allowed
}
