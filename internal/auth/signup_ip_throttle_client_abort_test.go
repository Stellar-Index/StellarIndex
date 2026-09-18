package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// The signup throttle mirrors ratelimit.Bucket's dwell clock (REL-06)
// and inherited its blind spot with it: the per-IP increment ran on the
// CALLER's context, and the throttle cannot tell a caller-cancelled call
// from a Redis outage. A client that posts a signup and immediately RSTs
// a few times a second therefore kept redisErrorSince armed — the 30 s
// unbroken-success streak that disarms it can never accumulate — and
// past the window CheckIP answered ErrThrottleUnavailable, so the signup
// handler 503'd EVERY caller while Redis was healthy
// (REL-06 F059, reverification-2026-09-18).
//
// The second half matters just as much: an attempt that errors out is an
// attempt that was never COUNTED, so aborting mid-flight bought
// unlimited uncounted attempts — precisely the bulk-mint vector F-1232
// exists to close.
//
// Redis is HEALTHY throughout; every failure below is manufactured by
// the aborted caller alone.

func newAbortThrottleRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = c.Close()
		mr.Close()
	})
	return c
}

func deadContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRedisSignupIPThrottle_ClientAbortsDoNotArmFailClosed(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tt := auth.NewRedisSignupIPThrottle(newAbortThrottleRedis(t), auth.SignupIPThrottleOptions{
		Max:    100,
		Window: time.Hour,
		NowFn:  func() time.Time { return now },
	})
	const ip = "203.0.113.9"

	// First abort: arms the dwell clock pre-fix.
	if err := tt.CheckIP(deadContext(), ip); err != nil {
		t.Fatalf("aborted attempt returned %v, want nil — the increment must still reach a HEALTHY "+
			"Redis; an error here is an UNCOUNTED signup attempt, which is free bulk-mint capacity", err)
	}

	// Past the dwell window with nothing but aborts in between.
	now = now.Add(auth.DefaultSignupThrottleDwellTime + time.Second)
	if err := tt.CheckIP(deadContext(), ip); errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatal("CheckIP returned ErrThrottleUnavailable after two aborted attempts — client aborts " +
			"armed the fail-closed clock, so any client can take signup offline while Redis is healthy")
	}

	// The decisive one: an innocent caller from a different IP.
	if err := tt.CheckIP(context.Background(), "203.0.113.10"); err != nil {
		t.Fatalf("an innocent signup got %v, want nil — the abort flood must not fail the throttle "+
			"CLOSED for everyone else", err)
	}
}

// Aborted attempts are COUNTED: the cap is reached by aborts alone, so
// abandoning the connection mid-flight buys no extra attempts.
func TestRedisSignupIPThrottle_AbortedAttemptsStillCountTowardTheCap(t *testing.T) {
	tt := auth.NewRedisSignupIPThrottle(newAbortThrottleRedis(t), auth.SignupIPThrottleOptions{
		Max:    2,
		Window: time.Hour,
	})
	const ip = "203.0.113.11"

	for i := range 2 {
		if err := tt.CheckIP(deadContext(), ip); err != nil {
			t.Fatalf("aborted attempt %d: %v", i+1, err)
		}
	}
	if err := tt.CheckIP(context.Background(), ip); !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Fatalf("attempt 3 = %v, want ErrSignupRateLimited — the two aborted attempts were never "+
			"counted, so a bulk-minter gets unlimited free attempts by aborting each one", err)
	}
}

// Blast-radius guard: detaching from the CALLER's cancellation must not
// detach from the BACKEND's failure. A genuinely broken Redis still has
// to arm the clock and fail closed past the window (F-0049 / F-0149).
func TestRedisSignupIPThrottle_RealOutageStillFailsClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tt := auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{
		Max:    5,
		Window: time.Hour,
		NowFn:  func() time.Time { return now },
	})
	const ip = "203.0.113.12"

	mr.Close() // real transport failure from here on

	err := tt.CheckIP(context.Background(), ip)
	if err == nil || errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("first outage attempt = %v, want a wrapped transport error (fail OPEN inside the "+
			"dwell window)", err)
	}

	now = now.Add(auth.DefaultSignupThrottleDwellTime + time.Second)
	if err := tt.CheckIP(context.Background(), ip); !errors.Is(err, auth.ErrThrottleUnavailable) {
		t.Fatalf("sustained-outage attempt = %v, want ErrThrottleUnavailable — a real Redis outage "+
			"past the dwell window must still fail CLOSED", err)
	}
}
