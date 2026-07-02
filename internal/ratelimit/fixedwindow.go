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
// Unlike [Bucket] (the atomic-Lua API rate limiter serving the
// 1000 req/min SLA) this is a plain INCR + best-effort EXPIRE pair.
// The window bucketing is sliding-window approximate — at most 2× the
// cap during a window-crossing burst — which is fine for an
// abuse-prevention threshold, NOT for a strict billing meter (use
// internal/usage for metering).
type FixedWindowCounter struct {
	rdb    redis.Cmdable
	window time.Duration
	nowFn  func() time.Time
}

// NewFixedWindowCounter constructs a counter. rdb must be non-nil and
// window > 0; nowFn nil defaults to [time.Now] (tests inject a
// synthetic clock to drive window transitions deterministically).
func NewFixedWindowCounter(rdb redis.Cmdable, window time.Duration, nowFn func() time.Time) *FixedWindowCounter {
	if rdb == nil {
		panic("ratelimit: NewFixedWindowCounter: rdb must not be nil")
	}
	if window <= 0 {
		panic("ratelimit: NewFixedWindowCounter: window must be > 0")
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &FixedWindowCounter{rdb: rdb, window: window, nowFn: nowFn}
}

// Incr increments the counter behind keyBase for the current window —
// the window bucket (`unix_seconds / window_seconds`, same derivation
// as [Bucket]) is appended to the key as ":<n>" — and returns the
// post-increment count. Sets the 2×window drain TTL on first
// increment (best-effort: if the EXPIRE fails the key persists until
// manual cleanup but the next increment still works).
func (c *FixedWindowCounter) Incr(ctx context.Context, keyBase string) (int64, error) {
	windowStart := c.nowFn().Unix() / int64(c.window.Seconds())
	key := fmt.Sprintf("%s:%d", keyBase, windowStart)
	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("ratelimit: fixed-window INCR %s: %w", key, err)
	}
	if count == 1 {
		_ = c.rdb.Expire(ctx, key, c.window*2).Err()
	}
	return count, nil
}
