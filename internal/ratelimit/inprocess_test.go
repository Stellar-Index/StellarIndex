// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// TestInProcess_LimitsWhenRedisNil is the C3-13 / C3-22 regression: a
// Bucket built with a nil Redis client must STILL enforce the limit
// (the old behaviour omitted the limiter entirely so the API ran
// uncapped). The Nth+1 request inside the window is rejected.
func TestInProcess_LimitsWhenRedisNil(t *testing.T) {
	b := ratelimit.New(nil, 3, time.Minute)
	if !b.InProcess() {
		t.Fatal("nil rdb should select the in-process fallback")
	}
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		r, err := b.Take(ctx, "anon:1.2.3.4")
		if err != nil {
			t.Fatalf("take %d: unexpected error (in-process must never error): %v", i, err)
		}
		if !r.Allowed {
			t.Fatalf("request %d within the budget should be allowed", i)
		}
		if r.Count != i {
			t.Errorf("count = %d, want %d", r.Count, i)
		}
		if r.Remaining != 3-i {
			t.Errorf("remaining = %d, want %d", r.Remaining, 3-i)
		}
	}

	// 4th request over the window is rejected — this is the line that is
	// RED on the unfixed code (there was no limiter at all with nil rdb).
	r, err := b.Take(ctx, "anon:1.2.3.4")
	if err != nil {
		t.Fatalf("over-limit take: unexpected error: %v", err)
	}
	if r.Allowed {
		t.Fatal("4th request over a 3/window budget must be DENIED by the in-process fallback")
	}
	if r.RetryAfter <= 0 {
		t.Errorf("retry_after must be > 0 on a denial, got %v", r.RetryAfter)
	}
	if r.Remaining != 0 {
		t.Errorf("remaining must be 0 when over budget, got %d", r.Remaining)
	}
}

// TestInProcess_KeysAreIsolated confirms one flooding key can't exhaust
// another key's budget — the per-subject property the anon-IP tier
// relies on.
func TestInProcess_KeysAreIsolated(t *testing.T) {
	b := ratelimit.New(nil, 1, time.Minute)
	ctx := context.Background()

	if r, _ := b.Take(ctx, "anon:10.0.0.1"); !r.Allowed {
		t.Fatal("first request for key A should be allowed")
	}
	if r, _ := b.Take(ctx, "anon:10.0.0.1"); r.Allowed {
		t.Fatal("second request for key A should be denied")
	}
	// A different subject still has its full budget.
	if r, _ := b.Take(ctx, "anon:10.0.0.2"); !r.Allowed {
		t.Fatal("first request for key B must be allowed — keys are isolated")
	}
}

// TestInProcess_WindowRollover confirms the FIXED-window reset: once the
// clock advances past the window boundary the counter resets and the
// caller regains budget.
func TestInProcess_WindowRollover(t *testing.T) {
	fakeNow := time.Unix(1_700_000_000, 0)
	b := ratelimit.New(nil, 1, time.Minute,
		ratelimit.WithClock(func() time.Time { return fakeNow }),
	)
	ctx := context.Background()

	if r, _ := b.Take(ctx, "k"); !r.Allowed {
		t.Fatal("first request should be allowed")
	}
	if r, _ := b.Take(ctx, "k"); r.Allowed {
		t.Fatal("second request in the same window should be denied")
	}

	// Advance past the 1-minute window boundary.
	fakeNow = fakeNow.Add(61 * time.Second)
	if r, _ := b.Take(ctx, "k"); !r.Allowed {
		t.Fatal("after the window rolls over the budget must reset (fixed-window)")
	}
}

// TestInProcess_TakeNOverride confirms the per-subject limit override
// (paid-tier custom plan) flows through the in-process path too.
func TestInProcess_TakeNOverride(t *testing.T) {
	b := ratelimit.New(nil, 1, time.Minute) // default max 1
	ctx := context.Background()

	// Override raises this key's budget to 2.
	if r, _ := b.TakeN(ctx, "k", 2); !r.Allowed {
		t.Fatal("1st with override=2 should be allowed")
	}
	if r, _ := b.TakeN(ctx, "k", 2); !r.Allowed {
		t.Fatal("2nd with override=2 should be allowed")
	}
	if r, _ := b.TakeN(ctx, "k", 2); r.Allowed {
		t.Fatal("3rd with override=2 should be denied")
	}
}
