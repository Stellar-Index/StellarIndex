// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FixedWindowCounter is the shared low-level "N per rolling window"
// primitive behind the auth throttles (login magic-link sends, signup
// per-IP caps): INCR a per-window Redis key, set a 2×window drain TTL
// on first touch, and return the post-increment count for the caller
// to compare against its cap.
//
// Like [Bucket] the INCR + EXPIRE-on-first pair is a single atomic Lua
// EVAL: a counter key must never outlive its drain TTL. The window
// bucketing is sliding-window approximate — at most 2× the cap during a
// window-crossing burst — which is fine for an abuse-prevention
// threshold, NOT for a strict billing meter (use internal/usage for
// metering).
type FixedWindowCounter struct {
	rdb    redis.Cmdable
	window time.Duration
	nowFn  func() time.Time
}

// NewFixedWindowCounter constructs a counter. rdb must be non-nil and
// window >= 1s; nowFn nil defaults to [time.Now] (tests inject a
// synthetic clock to drive window transitions deterministically).
//
// The 1s floor matches [New]'s: the bucket key derivation divides by
// `int64(window.Seconds())`, which truncates to zero — a divide-by-zero
// panic at first Incr — for any sub-second window. Rejecting it at
// construction fails fast at wiring time instead of on the first
// request.
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
// REL-05 (audit-2026-07-23): as a client-side INCR-then-EXPIRE pair the
// EXPIRE was both non-atomic and best-effort, so any dropped connection
// / MISCONF / OOM between the two calls left a TTL-less key behind. Those
// keys are never revisited (the bucket suffix moves on with the clock),
// so each leak was permanent and the throttle namespace grew without
// bound. Inside EVAL, Redis runs both commands or neither.
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

// Incr increments the counter behind keyBase for the current window —
// the window bucket (`unix_seconds / window_seconds`, same derivation
// as [Bucket]) is appended to the key as ":<n>" — and returns the
// post-increment count. The 2×window drain TTL is set atomically with
// the increment that creates the key.
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

// LocalFixedWindowCounter is the in-process twin of
// [FixedWindowCounter]: same `unix / window_seconds` bucketing, same
// Nth+1-is-over semantics, no backend and therefore no error path. It
// exists so a Redis-backed throttle can keep enforcing its cap —
// degraded to per-process accounting — while Redis is unreachable,
// instead of falling open for the whole outage (SEC-15 / REL-06).
//
// Backed by the same bounded [localStore] as the [Bucket] fallback, so
// it inherits the once-per-window sweep and the hard key cap.
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
