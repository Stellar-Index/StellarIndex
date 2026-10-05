package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// Peek must report the verdict of the window's latest Take without
// spending a token, on both backends.
func TestBucket_PeekDoesNotSpend(t *testing.T) {
	rdb, _ := newRedis(t)
	now := time.Unix(1_750_000_010, 0)
	clock := ratelimit.WithClock(func() time.Time { return now })
	for name, b := range map[string]*ratelimit.Bucket{
		"redis":      ratelimit.New(rdb, 2, time.Minute, clock),
		"in-process": ratelimit.New(nil, 2, time.Minute, clock),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			peek := func() ratelimit.Result {
				t.Helper()
				r, err := b.Peek(ctx, "k")
				if err != nil {
					t.Fatalf("peek: %v", err)
				}
				return r
			}
			if r := peek(); !r.Allowed || r.Count != 0 {
				t.Fatalf("untouched key: %+v, want allowed with count 0", r)
			}
			for i := 0; i < 2; i++ {
				_, _ = b.Take(ctx, "k")
				for j := 0; j < 3; j++ {
					if r := peek(); !r.Allowed || r.Count != i+1 {
						t.Fatalf("after %d takes: %+v, want allowed with count %d", i+1, r, i+1)
					}
				}
			}
			if r, _ := b.Take(ctx, "k"); r.Allowed {
				t.Fatal("third take within a limit of 2 was allowed")
			}
			r := peek()
			if r.Allowed || r.Count != 3 {
				t.Fatalf("after a denied take: %+v, want denied with count 3", r)
			}
			if r.RetryAfter != 10*time.Second {
				t.Errorf("RetryAfter = %v, want 10s (rest of the window)", r.RetryAfter)
			}
			if r := peek(); r.Count != 3 {
				t.Errorf("a second peek moved the count to %d", r.Count)
			}
		})
	}
}

// A Redis error from Peek must not arm the fail-closed dwell clock that
// Take reads: Peek is advisory and its callers fail open.
func TestBucket_PeekErrorDoesNotArmDwellClock(t *testing.T) {
	rdb, mr := newRedis(t)
	now := time.Unix(1_750_000_000, 0)
	b := ratelimit.New(rdb, 5, time.Minute,
		ratelimit.WithClock(func() time.Time { return now }),
		ratelimit.WithDwellTime(30*time.Second))
	mr.Close()
	if _, err := b.Peek(context.Background(), "k"); err == nil {
		t.Fatal("peek against a closed Redis returned no error")
	}
	now = now.Add(31 * time.Second)
	_, err := b.Take(context.Background(), "k")
	if err == nil {
		t.Fatal("take against a closed Redis returned no error")
	}
	if err == ratelimit.ErrThrottleUnavailable {
		t.Fatal("a failed Peek armed the dwell clock: first failing Take already fails closed")
	}
}
