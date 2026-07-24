package auth_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestRedisSignupIPThrottle_Allows_UpToCap pins the first
// `Max` increments succeed.
func TestRedisSignupIPThrottle_Allows_UpToCap(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    3,
		Window: time.Hour,
	})
	ctx := context.Background()
	const ip = "203.0.113.7"

	for i := 0; i < 3; i++ {
		if err := tt.CheckIP(ctx, ip); err != nil {
			t.Fatalf("attempt %d: want nil, got %v", i+1, err)
		}
	}
}

// TestRedisSignupIPThrottle_Blocks_OverCap pins that the (Max+1)th
// increment returns ErrSignupRateLimited.
func TestRedisSignupIPThrottle_Blocks_OverCap(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    2,
		Window: time.Hour,
	})
	ctx := context.Background()
	const ip = "203.0.113.7"

	for i := 0; i < 2; i++ {
		if err := tt.CheckIP(ctx, ip); err != nil {
			t.Fatalf("attempt %d under cap: %v", i+1, err)
		}
	}
	err := tt.CheckIP(ctx, ip)
	if !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Fatalf("attempt 3 over cap: want ErrSignupRateLimited, got %v", err)
	}
}

// TestRedisSignupIPThrottle_DistinctIPs_IndependentBuckets
// confirms two IPs share no state.
func TestRedisSignupIPThrottle_DistinctIPs_IndependentBuckets(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    1,
		Window: time.Hour,
	})
	ctx := context.Background()

	if err := tt.CheckIP(ctx, "203.0.113.1"); err != nil {
		t.Fatalf("ip1 first: %v", err)
	}
	if err := tt.CheckIP(ctx, "203.0.113.2"); err != nil {
		t.Fatalf("ip2 first: %v", err)
	}
	if err := tt.CheckIP(ctx, "203.0.113.1"); !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Fatalf("ip1 second: want ErrSignupRateLimited, got %v", err)
	}
	if err := tt.CheckIP(ctx, "203.0.113.2"); !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Fatalf("ip2 second: want ErrSignupRateLimited, got %v", err)
	}
}

// TestRedisSignupIPThrottle_EmptyIP_FallsOpen pins that an
// IP-less request (production shouldn't see — Caddy + Cloudflare
// always populate one) doesn't trigger the throttle.
func TestRedisSignupIPThrottle_EmptyIP_FallsOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    1,
		Window: time.Hour,
	})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := tt.CheckIP(ctx, ""); err != nil {
			t.Fatalf("attempt %d (empty ip): want nil, got %v", i+1, err)
		}
	}
}

// TestRedisSignupIPThrottle_DefaultsApplied confirms zero-value
// options pick the documented defaults (5/hour, "signup-ip:" prefix).
func TestRedisSignupIPThrottle_DefaultsApplied(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{})
	ctx := context.Background()
	const ip = "203.0.113.42"

	for i := 0; i < 5; i++ {
		if err := tt.CheckIP(ctx, ip); err != nil {
			t.Fatalf("default-cap attempt %d: %v", i+1, err)
		}
	}
	if err := tt.CheckIP(ctx, ip); !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Fatalf("default-cap attempt 6: want ErrSignupRateLimited, got %v", err)
	}

	// Confirm the key prefix used (sanity-check the namespace
	// without coupling tightly to the format).
	for _, k := range mr.Keys() {
		if len(k) >= len("signup-ip:") && k[:len("signup-ip:")] == "signup-ip:" {
			return
		}
	}
	t.Errorf("no key with `signup-ip:` prefix found in miniredis (have %v)", mr.Keys())
}

// TestRedisSignupIPThrottle_DwellTime_FailsOpenInsideWindow pins
// the F-0049 / F-0149 inversion: Redis errors observed inside the
// dwell-time window still propagate as wrapped Redis errors (so
// the handler falls open), preserving the transient-blip UX.
func TestRedisSignupIPThrottle_DwellTime_FailsOpenInsideWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Close the client immediately to force every CheckIP into the
	// Redis-error path; same shape as ratelimit's
	// TestBucket_TakeReturnsErrorOnRedisOutage.
	if err := rdb.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}

	fakeNow := time.Unix(1_750_000_000, 0)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:       3,
		Window:    time.Hour,
		DwellTime: 30 * time.Second,
		NowFn:     func() time.Time { return fakeNow },
	})

	err := tt.CheckIP(context.Background(), "203.0.113.7")
	if err == nil {
		t.Fatal("first error: want wrapped Redis err, got nil")
	}
	if errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("first error: must NOT be ErrThrottleUnavailable yet (dwell-clock just armed): %v", err)
	}

	// Advance to dwell-time edge but not past — still inside the
	// fail-open window.
	fakeNow = fakeNow.Add(20 * time.Second)
	err = tt.CheckIP(context.Background(), "203.0.113.7")
	if errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("err inside window: must remain fail-open, got ErrThrottleUnavailable")
	}
	if err == nil {
		t.Fatal("err inside window: want wrapped Redis err, got nil")
	}
}

// TestRedisSignupIPThrottle_DwellTime_FailsClosedAfterWindow pins
// the J40 protection: sustained Redis errors past the dwell-time
// return ErrThrottleUnavailable so the handler emits 503.
func TestRedisSignupIPThrottle_DwellTime_FailsClosedAfterWindow(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := rdb.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}

	fakeNow := time.Unix(1_750_000_000, 0)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:       3,
		Window:    time.Hour,
		DwellTime: 30 * time.Second,
		NowFn:     func() time.Time { return fakeNow },
	})

	// Arm the dwell-clock.
	if err := tt.CheckIP(context.Background(), "203.0.113.7"); err == nil {
		t.Fatal("arm: want err, got nil")
	}

	// Cross the dwell-time threshold. The doc string says ">" so
	// pick a duration that comfortably exceeds 30s.
	fakeNow = fakeNow.Add(31 * time.Second)
	err := tt.CheckIP(context.Background(), "203.0.113.7")
	if !errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("after dwell-time: want ErrThrottleUnavailable, got %v", err)
	}
}

// TestRedisSignupIPThrottle_DwellTime_FlapVsSustainedRecovery pins the REL-06
// recovery semantic: a single Redis success must NOT reset the fail-closed dwell
// clock (a flapping Redis still trips fail-closed after dwellTime); only a
// sustained healthy streak (dwellTime of unbroken successes) clears it and
// restores fail-open. The prior behaviour — one success wipes the clock — let a
// flapping Redis keep this signup throttle fail-open indefinitely.
func TestRedisSignupIPThrottle_DwellTime_FlapVsSustainedRecovery(t *testing.T) {
	mr := miniredis.RunT(t)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	fakeNow := time.Unix(1_750_000_000, 0)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:       3,
		Window:    time.Hour,
		DwellTime: 30 * time.Second,
		NowFn:     func() time.Time { return fakeNow },
	})

	// A poison value makes INCR fail (miniredis "value is not an integer");
	// deleting it heals Redis. Window=1h ⇒ windowStart = unix/3600; the ≤76s the
	// clock advances below stays in the same window, so the key is stable.
	const ip = "203.0.113.42"
	windowStart := fakeNow.Unix() / 3600
	key := "signup-ip:" + ip + ":" + strconv.FormatInt(windowStart, 10)
	poison := func() { mr.Set(key, "not-a-number") }
	heal := func() { mr.Del(key) }
	check := func() error { return tt.CheckIP(context.Background(), ip) }

	// Arm the fail-closed clock with an error.
	poison()
	if err := check(); err == nil {
		t.Fatal("arm: want INCR err, got nil")
	}

	// FLAPPING: one lucky success, then continued error past the dwell window.
	// The stray success must NOT reset the clock → fail-CLOSED (the REL-06 fix).
	heal()
	_ = check() // single success
	poison()
	fakeNow = fakeNow.Add(45 * time.Second)
	err := check()
	if err == nil {
		t.Fatal("flap: want INCR err, got nil")
	}
	if !errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("flap: a stray success must not reset the dwell clock — want fail-CLOSED, got %v", err)
	}

	// SUSTAINED RECOVERY: heal and succeed continuously past dwellTime → the clock
	// clears, so a later error opens a fresh window and falls open (not 503).
	heal()
	_ = check()                             // healthySince starts here
	fakeNow = fakeNow.Add(31 * time.Second) // unbroken healthy > dwellTime
	_ = check()                             // clears redisErrorSince
	poison()
	err = check()
	if err == nil {
		t.Fatal("recovered: want INCR err, got nil")
	}
	if errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("recovered: sustained recovery should have cleared the clock — want fail-OPEN, got ErrThrottleUnavailable")
	}
}

// TestRedisSignupIPThrottle_DwellTime_Disabled pins that a negative
// DwellTime preserves the pre-F-0049 fail-open-always behaviour —
// operators who explicitly opt out never see ErrThrottleUnavailable.
func TestRedisSignupIPThrottle_DwellTime_Disabled(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := rdb.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}

	fakeNow := time.Unix(1_750_000_000, 0)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:       3,
		Window:    time.Hour,
		DwellTime: -1, // disabled
		NowFn:     func() time.Time { return fakeNow },
	})

	// First error arms the (unused) clock.
	if err := tt.CheckIP(context.Background(), "203.0.113.7"); err == nil {
		t.Fatal("arm: want err, got nil")
	}
	// Advance well past any plausible dwell-time.
	fakeNow = fakeNow.Add(10 * time.Minute)
	err := tt.CheckIP(context.Background(), "203.0.113.7")
	if errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Errorf("disabled: must never return ErrThrottleUnavailable, got %v", err)
	}
}

// TestRedisSignupIPThrottle_DwellTime_DefaultApplied confirms the
// zero-value DwellTime picks DefaultSignupThrottleDwellTime (30s).
func TestRedisSignupIPThrottle_DwellTime_DefaultApplied(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := rdb.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}

	fakeNow := time.Unix(1_750_000_000, 0)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    3,
		Window: time.Hour,
		// DwellTime intentionally unset.
		NowFn: func() time.Time { return fakeNow },
	})

	if err := tt.CheckIP(context.Background(), "203.0.113.7"); err == nil {
		t.Fatal("arm: want err, got nil")
	}
	// Just under default 30s — must still fail-open.
	fakeNow = fakeNow.Add(29 * time.Second)
	if err := tt.CheckIP(context.Background(), "203.0.113.7"); errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("at t+29s: must fail-open under default 30s, got ErrThrottleUnavailable")
	}
	// Past default 30s — must fail-closed.
	fakeNow = fakeNow.Add(5 * time.Second) // total t+34s
	if err := tt.CheckIP(context.Background(), "203.0.113.7"); !errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("at t+34s: want ErrThrottleUnavailable under default 30s, got %v", err)
	}
}

// keep strconv import live in case future tests want explicit
// window-bucket assertions.
var _ = strconv.Itoa
