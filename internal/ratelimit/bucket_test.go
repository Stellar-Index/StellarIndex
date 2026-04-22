package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/RatesEngine/rates-engine/internal/ratelimit"
)

func newRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func TestBucket_AllowsUpToLimit(t *testing.T) {
	rdb, _ := newRedis(t)
	b := ratelimit.New(rdb, 3, time.Minute)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		r, err := b.Take(ctx, "key")
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if !r.Allowed {
			t.Errorf("request %d should be allowed", i)
		}
		if r.Count != i {
			t.Errorf("count = %d, want %d", r.Count, i)
		}
		if r.Remaining != 3-i {
			t.Errorf("remaining = %d, want %d", r.Remaining, 3-i)
		}
	}
}

func TestBucket_RejectsOverLimit(t *testing.T) {
	rdb, _ := newRedis(t)
	b := ratelimit.New(rdb, 2, time.Minute)
	ctx := context.Background()

	// Use up the budget.
	for i := 0; i < 2; i++ {
		r, _ := b.Take(ctx, "key")
		if !r.Allowed {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// Third request should be denied.
	r, err := b.Take(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	if r.Allowed {
		t.Error("third request should be denied")
	}
	if r.RetryAfter <= 0 {
		t.Errorf("retry_after should be > 0 when denied, got %v", r.RetryAfter)
	}
	if r.Remaining != 0 {
		t.Errorf("remaining should be 0 when over, got %d", r.Remaining)
	}
}

func TestBucket_WindowsAreIsolated(t *testing.T) {
	rdb, mr := newRedis(t)

	// Inject a controllable clock so the minute-bucket derivation
	// advances with the fake redis TTL clock. Production uses
	// time.Now directly.
	fakeNow := time.Unix(1_750_000_000, 0) // arbitrary fixed point
	b := ratelimit.New(rdb, 1, time.Minute,
		ratelimit.WithClock(func() time.Time { return fakeNow }),
	)
	ctx := context.Background()

	// Exhaust window 1.
	r1, _ := b.Take(ctx, "k")
	if !r1.Allowed {
		t.Fatal("first request should be allowed")
	}
	r2, _ := b.Take(ctx, "k")
	if r2.Allowed {
		t.Fatal("second in same window should be denied")
	}

	// Advance BOTH Go clock (bucket key derivation) and redis TTL
	// clock (key expiration) by 1 minute.
	fakeNow = fakeNow.Add(61 * time.Second)
	mr.FastForward(61 * time.Second)

	r3, _ := b.Take(ctx, "k")
	if !r3.Allowed {
		t.Error("new window should allow again")
	}
}

func TestBucket_KeysAreIndependent(t *testing.T) {
	rdb, _ := newRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)
	ctx := context.Background()

	// Exhaust one key; the other should still be allowed.
	if r, _ := b.Take(ctx, "alice"); !r.Allowed {
		t.Fatal("alice 1st should be allowed")
	}
	if r, _ := b.Take(ctx, "alice"); r.Allowed {
		t.Fatal("alice 2nd should be denied")
	}
	if r, _ := b.Take(ctx, "bob"); !r.Allowed {
		t.Fatal("bob 1st should be allowed (independent key)")
	}
}

func TestBucket_AtomicUnderConcurrency(t *testing.T) {
	// Atomicity check — fire 100 concurrent takes with limit=5 and
	// verify exactly 5 are allowed. A non-atomic INCR+EXPIRE would
	// let duplicates through.
	rdb, _ := newRedis(t)
	b := ratelimit.New(rdb, 5, time.Minute)
	ctx := context.Background()

	const parallel = 100
	results := make(chan bool, parallel)

	for i := 0; i < parallel; i++ {
		go func() {
			r, err := b.Take(ctx, "concurrent")
			if err != nil {
				results <- false
				return
			}
			results <- r.Allowed
		}()
	}

	allowed := 0
	for i := 0; i < parallel; i++ {
		if <-results {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("got %d allowed (want exactly 5)", allowed)
	}
}

func TestBucket_RetryAfterApproximatesWindowTTL(t *testing.T) {
	rdb, _ := newRedis(t)
	b := ratelimit.New(rdb, 1, time.Minute)
	ctx := context.Background()

	_, _ = b.Take(ctx, "k")   // consume
	r, _ := b.Take(ctx, "k")  // should reject
	if r.Allowed {
		t.Fatal("second should be denied")
	}
	// Window is 60s; Take() sets TTL to 2×window = 120s. TTL returned
	// should be ≤ 120s (fresh) and > 0. Miniredis's fake clock doesn't
	// advance on its own, so this is effectively ~120s.
	if r.RetryAfter <= 0 || r.RetryAfter > 2*time.Minute {
		t.Errorf("retry_after = %v (want (0, 120s])", r.RetryAfter)
	}
}

func TestBucket_ZeroArgsPanic(t *testing.T) {
	rdb, _ := newRedis(t)

	for _, tc := range []struct {
		name   string
		max    int
		window time.Duration
	}{
		{"zero max", 0, time.Minute},
		{"negative max", -1, time.Minute},
		{"zero window", 5, 0},
		{"negative window", 5, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected panic")
				}
			}()
			ratelimit.New(rdb, tc.max, tc.window)
		})
	}
}
