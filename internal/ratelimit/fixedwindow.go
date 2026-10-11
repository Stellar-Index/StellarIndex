// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FixedWindowCounter is the "N per window" primitive behind the auth throttles (login
// magic-link sends, signup per-IP caps): INCR a per-window Redis key, set a 2x window drain TTL
// on first touch, and return the post-increment count for the caller to compare to its cap.
//
// Like [Bucket], INCR plus EXPIRE-on-first is one atomic Lua EVAL, so a counter key never
// outlives its drain TTL. Window bucketing allows up to 2x the cap in a window-crossing burst,
// fine for abuse prevention, NOT for billing (use internal/usage for metering).
type FixedWindowCounter struct {
	rdb    redis.Cmdable
	window time.Duration
	nowFn  func() time.Time
}

// NewFixedWindowCounter constructs a counter. rdb must be non-nil and window >= 1s; a nil nowFn
// defaults to [time.Now] (tests inject a clock to drive window transitions).
//
// The 1s floor matches [New]: key derivation divides by `int64(window.Seconds())`, which is zero
// for a sub-second window and would panic at first Incr, so reject it at wiring time.
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

// incrLua is the atomic increment: INCR, plus the drain EXPIRE on the
// call that created the key.
//
// KEYS[1] — the window-bucketed counter key
// ARGV[1] — TTL in seconds (2× window so keys drain on their own)
//
// Returns — the post-increment count.
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

// Incr increments the counter behind keyBase for the current window and returns the
// post-increment count. The window bucket (`unix_seconds / window_seconds`, as in [Bucket]) is
// appended as ":<n>", and the 2x window drain TTL is set atomically with the creating increment.
func (c *FixedWindowCounter) Incr(ctx context.Context, keyBase string) (int64, error) {
	windowStart := c.nowFn().Unix() / int64(c.window.Seconds())
	key := fmt.Sprintf("%s:%d", keyBase, windowStart)
	// Seconds, floored at 1 — Redis reads EXPIRE 0 as "no expiry", the
	// very leak this script exists to prevent. The constructor already
	// rejects sub-second windows, so this is belt-and-braces.
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

// LocalFixedWindowCounter is the in-process twin of [FixedWindowCounter]: same bucketing and
// Nth+1-is-over semantics, no backend and so no error path. It lets a Redis-backed throttle keep
// enforcing its cap, with per-process accounting, while Redis is unreachable instead of falling
// open for the outage. It shares the bounded [localStore] of the [Bucket] fallback, inheriting
// the once-per-window sweep and the hard key cap.
type LocalFixedWindowCounter struct {
	store  *localStore
	window time.Duration
	nowFn  func() time.Time
}

// NewLocalFixedWindowCounter constructs the in-process counter. window
// must be >= 1s (same derivation, same divide-by-zero floor as
// [NewFixedWindowCounter]); nowFn nil defaults to [time.Now].
func NewLocalFixedWindowCounter(window time.Duration, nowFn func() time.Time) *LocalFixedWindowCounter {
	if window < time.Second {
		panic("ratelimit: NewLocalFixedWindowCounter: window must be >= 1s")
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &LocalFixedWindowCounter{store: newLocalStore(), window: window, nowFn: nowFn}
}

// Allow increments keyBase's in-process counter for the current window
// and reports whether the post-increment count is within limit. Safe
// for concurrent use.
func (c *LocalFixedWindowCounter) Allow(keyBase string, limit int) bool {
	now := c.nowFn()
	window := now.Unix() / int64(c.window.Seconds())
	_, allowed := c.store.take(keyBase, window, limit, now, c.window)
	return allowed
}
