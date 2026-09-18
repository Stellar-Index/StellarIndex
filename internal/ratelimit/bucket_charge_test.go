// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// chargeBackends runs fn against both limiter backends. The in-process
// fallback is what enforces the limit when Redis is absent at boot, so
// a weight that only the Redis path honoured would leave the
// amplification live in exactly the degraded mode the fallback exists
// for.
func chargeBackends(t *testing.T, fn func(t *testing.T, rdb redis.Cmdable)) {
	t.Helper()
	t.Run("redis", func(t *testing.T) {
		rdb, _ := newRedis(t)
		fn(t, rdb)
	})
	t.Run("in-process", func(t *testing.T) {
		fn(t, nil)
	})
}

// newChargeBucket avoids handing ratelimit.New a typed-nil interface:
// New selects the in-process store on `rdb == nil`, which a nil
// *redis.Client wrapped in a redis.Cmdable would not satisfy.
func newChargeBucket(rdb redis.Cmdable, limit int) *ratelimit.Bucket {
	if rdb == nil {
		return ratelimit.New(nil, limit, time.Minute)
	}
	return ratelimit.New(rdb, limit, time.Minute)
}

// TestBucket_Charge_SpendsCostTokens is the F046 regression: the
// limiter had no notion of cost — TakeN's N is a per-subject LIMIT and
// the script did a plain INCR — so a 1000-id batch and a one-id read
// spent the same single token. A charge of N must move the counter by
// N, in one call.
func TestBucket_Charge_SpendsCostTokens(t *testing.T) {
	chargeBackends(t, func(t *testing.T, rdb redis.Cmdable) {
		b := newChargeBucket(rdb, 100)
		ctx := context.Background()

		r, err := b.Charge(ctx, "k", 40, 0)
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if !r.Allowed || r.Count != 40 || r.Remaining != 60 {
			t.Fatalf("charge(40) = allowed %v count %d remaining %d; want true/40/60",
				r.Allowed, r.Count, r.Remaining)
		}

		// A plain Take shares the counter the charge moved.
		r, err = b.Take(ctx, "k")
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		if r.Count != 41 || r.Remaining != 59 {
			t.Fatalf("take after charge(40): count %d remaining %d; want 41/59", r.Count, r.Remaining)
		}

		// 41 + 60 = 101 > 100: denied, with a usable Retry-After.
		r, err = b.Charge(ctx, "k", 60, 0)
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if r.Allowed {
			t.Fatalf("charge(60) at count 41 of 100 was allowed (count %d)", r.Count)
		}
		if r.Remaining != 0 {
			t.Errorf("remaining on denial = %d, want 0", r.Remaining)
		}
		if r.RetryAfter < time.Second || r.RetryAfter > time.Minute {
			t.Errorf("RetryAfter = %v, want within (0, 1m]", r.RetryAfter)
		}
	})
}

// TestBucket_Charge_ExactFitIsAllowed pins the boundary: a charge that
// lands the counter exactly ON the limit fits; one more token does not.
func TestBucket_Charge_ExactFitIsAllowed(t *testing.T) {
	chargeBackends(t, func(t *testing.T, rdb redis.Cmdable) {
		b := newChargeBucket(rdb, 10)
		ctx := context.Background()

		r, err := b.Charge(ctx, "k", 10, 0)
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if !r.Allowed || r.Remaining != 0 {
			t.Fatalf("charge(10) of 10 = allowed %v remaining %d; want true/0", r.Allowed, r.Remaining)
		}
		r, err = b.Take(ctx, "k")
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		if r.Allowed {
			t.Fatal("the token after an exact-fit charge must be denied")
		}
	})
}

// TestBucket_Charge_ClampsCost pins both ends of the normalisation.
//
// Above the limit: a request priced over the ceiling is charged the
// whole ceiling, not refused forever — it is servable into an untouched
// window and leaves nothing behind it.
//
// Below one: zero and negative costs spend a token. A negative INCRBY
// would hand budget BACK, which is the one thing a limiter must never
// do on caller-computed input.
func TestBucket_Charge_ClampsCost(t *testing.T) {
	chargeBackends(t, func(t *testing.T, rdb redis.Cmdable) {
		ctx := context.Background()

		b := newChargeBucket(rdb, 60)
		r, err := b.Charge(ctx, "big", 1000, 0)
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if !r.Allowed || r.Count != 60 || r.Remaining != 0 {
			t.Fatalf("charge(1000) of 60 = allowed %v count %d remaining %d; want true/60/0",
				r.Allowed, r.Count, r.Remaining)
		}
		if r, _ = b.Take(ctx, "big"); r.Allowed {
			t.Fatal("an over-limit charge must consume the whole window")
		}

		// The clamp follows the per-subject override, not the bucket max.
		r, err = b.Charge(ctx, "big-override", 1000, 200)
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if !r.Allowed || r.Count != 200 {
			t.Fatalf("charge(1000) under override 200 = allowed %v count %d; want true/200", r.Allowed, r.Count)
		}

		for _, cost := range []int{0, -5} {
			before, err := b.Take(ctx, "small")
			if err != nil {
				t.Fatalf("take: %v", err)
			}
			after, err := b.Charge(ctx, "small", cost, 0)
			if err != nil {
				t.Fatalf("charge(%d): %v", cost, err)
			}
			if after.Count != before.Count+1 {
				t.Errorf("charge(%d) moved the counter %d -> %d; want +1", cost, before.Count, after.Count)
			}
		}
	})
}

// TestBucket_Charge_FirstWriteArmsExpiry guards the script's
// expire-on-create branch. It used to key on `current == 1`; a key
// first written by a cost-N charge lands on N, so an unported check
// would leave it with no TTL and the counter would never drain.
func TestBucket_Charge_FirstWriteArmsExpiry(t *testing.T) {
	rdb, mr := newRedis(t)
	b := ratelimit.New(rdb, 100, time.Minute, ratelimit.WithKeyPrefix("ttl:"))

	if _, err := b.Charge(context.Background(), "k", 25, 0); err != nil {
		t.Fatalf("charge: %v", err)
	}
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly one", keys)
	}
	if ttl := mr.TTL(keys[0]); ttl != 2*time.Minute {
		t.Fatalf("TTL after a cost-25 first write = %v, want 2m (2x window)", ttl)
	}
}

// TestBucket_TakeN_StillSpendsOne pins that routing TakeN through
// Charge changed nothing for every existing caller: the third argument
// is still the per-subject LIMIT, and the spend is still one.
func TestBucket_TakeN_StillSpendsOne(t *testing.T) {
	chargeBackends(t, func(t *testing.T, rdb redis.Cmdable) {
		b := newChargeBucket(rdb, 5)
		r, err := b.TakeN(context.Background(), "k", 50)
		if err != nil {
			t.Fatalf("TakeN: %v", err)
		}
		if r.Count != 1 || r.Remaining != 49 {
			t.Fatalf("TakeN(limit=50) = count %d remaining %d; want 1/49", r.Count, r.Remaining)
		}
	})
}
