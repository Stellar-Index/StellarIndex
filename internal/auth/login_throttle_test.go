package auth_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// redisOutage is a go-redis hook that fails every command with a
// transport error, on demand. Cheaper and far more deterministic than
// killing the miniredis server (go-redis spends ~0.8s per call
// rediscovering that a dead address is dead), and it reproduces the
// exact thing the throttle sees during an outage: Incr returns an error.
type redisOutage struct{ down atomic.Bool }

func (h *redisOutage) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *redisOutage) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.down.Load() {
			err := errors.New("simulated redis outage")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (h *redisOutage) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// newLoginThrottle wires a throttle against a fresh miniredis and hands
// back the outage switch so a test can take Redis down mid-flight.
func newLoginThrottle(t *testing.T, opts auth.LoginThrottleOptions) (*auth.RedisLoginThrottle, *redisOutage) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	outage := &redisOutage{}
	rdb.AddHook(outage)
	return auth.NewRedisLoginThrottle(rdb, opts), outage
}

// TestLoginThrottle_AllowsUpToCapThenDenies pins the baseline contract
// the outage tests below build on: the (Max+1)th magic-link send for one
// target inbox is denied, and a denial is reported as (false, nil) so the
// handler skips the send rather than falling open.
func TestLoginThrottle_AllowsUpToCapThenDenies(t *testing.T) {
	tt, _ := newLoginThrottle(t, auth.LoginThrottleOptions{
		MaxPerIP:    100,
		MaxPerEmail: 3,
		Window:      time.Hour,
	})
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		ok, err := tt.Allow(ctx, "203.0.113.7", "victim@example.com")
		if err != nil {
			t.Fatalf("send %d: unexpected error: %v", i, err)
		}
		if !ok {
			t.Fatalf("send %d is within the 3/window cap and must be allowed", i)
		}
	}
	ok, err := tt.Allow(ctx, "203.0.113.7", "victim@example.com")
	if err != nil {
		t.Fatalf("over-cap send: unexpected error: %v", err)
	}
	if ok {
		t.Fatal("the 4th send for a 3/window email cap must be denied")
	}
}

// TestLoginThrottle_EmailBombIsCappedDuringRedisOutage is the SEC-15 /
// REL-06 regression, expressed as the attack.
//
// Attack: knock Redis over (or simply wait for a fail-over — an attacker
// does not need to cause the outage, only to notice it) and then POST
// /v1/auth/login for one victim's address in a loop. Pre-fix every call
// returned (false, <redis error>), the handler's fail-open branch fired
// on the non-nil error, and a magic-link email went out EVERY time: the
// per-target-email cap this type exists to enforce was absent for the
// whole outage. The blast radius is the victim's inbox plus the
// deployment's sender reputation and email quota, both of which outlive
// the outage.
//
// Post-fix the in-process fallback keeps counting, so the send stops at
// the same cap. The assertion is on the exact contract the handler
// reads: (false, nil) — a definitive DENY. (false, err) would NOT do,
// because the handler inspects the error first and would send anyway.
func TestLoginThrottle_EmailBombIsCappedDuringRedisOutage(t *testing.T) {
	tt, redisDown := newLoginThrottle(t, auth.LoginThrottleOptions{
		MaxPerIP:    100, // isolate the per-EMAIL dimension
		MaxPerEmail: 3,
		Window:      time.Hour,
	})
	ctx := context.Background()

	// The outage starts before any send: nothing is warm in Redis.
	redisDown.down.Store(true)

	const victim = "victim@example.com"
	sends := 0
	for i := 0; i < 50; i++ {
		ok, err := tt.Allow(ctx, "203.0.113.7", victim)
		// This is precisely what dashboardauth.HandleLogin does with the
		// pair: a non-nil error falls OPEN (sends); otherwise ok decides.
		if err != nil || ok {
			sends++
		}
	}
	if sends > 3 {
		t.Fatalf("Redis-outage email bomb delivered %d magic-link sends to %s; the 3/window "+
			"cap must still bound outbound mail when the backing store is down (SEC-15/REL-06)",
			sends, victim)
	}
	if sends != 3 {
		t.Fatalf("sends = %d, want exactly 3 — the degraded cap must match the configured "+
			"cap, not be tighter", sends)
	}
}

// TestLoginThrottle_IPSprayIsCappedDuringRedisOutage is the same attack
// on the other dimension: one IP spraying links at MANY addresses (the
// email-quota / sender-reputation burn, which no per-email counter
// bounds).
func TestLoginThrottle_IPSprayIsCappedDuringRedisOutage(t *testing.T) {
	tt, redisDown := newLoginThrottle(t, auth.LoginThrottleOptions{
		MaxPerIP:    4,
		MaxPerEmail: 100, // isolate the per-IP dimension
		Window:      time.Hour,
	})
	ctx := context.Background()
	redisDown.down.Store(true)

	sends := 0
	for i := 0; i < 50; i++ {
		// A fresh victim every time — only the IP cap can stop this.
		ok, err := tt.Allow(ctx, "203.0.113.7", "target"+string(rune('a'+i%26))+"@example.com")
		if err != nil || ok {
			sends++
		}
	}
	if sends != 4 {
		t.Fatalf("Redis-outage spray delivered %d sends from one IP, want the 4/window cap "+
			"to hold (SEC-15/REL-06)", sends)
	}
}

// TestLoginThrottle_RedisBlipStillSendsWithinCap guards the other side of
// the trade-off: the fallback must not turn a Redis outage into a login
// outage. Within the degraded cap a send is still permitted, and the
// Redis error is surfaced so the handler logs it.
func TestLoginThrottle_RedisBlipStillSendsWithinCap(t *testing.T) {
	tt, redisDown := newLoginThrottle(t, auth.LoginThrottleOptions{
		MaxPerIP:    10,
		MaxPerEmail: 5,
		Window:      time.Hour,
	})
	ctx := context.Background()
	redisDown.down.Store(true)

	ok, err := tt.Allow(ctx, "203.0.113.7", "legit@example.com")
	if err == nil {
		t.Error("a Redis transport failure must still be surfaced to the caller for logging")
	}
	if !ok {
		t.Error("the first send of the window must survive a Redis outage — the fallback " +
			"enforces the cap, it does not take login offline")
	}
}

// TestLoginThrottle_IPv6RotationWithinSlash64CannotEvadeCap is the
// IPv6-keying regression (audit-2026-07-23; the same class the API
// middleware fixed as SEC-15).
//
// Attack: an attacker with ANY delegated IPv6 /64 — the standard
// residential/VPS allocation — sources each request from a different
// /128 inside it. Pre-fix the throttle keyed on the full address, so
// every request landed on a pristine bucket and the per-IP magic-link
// cap cost nothing to bypass: 2^64 free buckets. Post-fix the key is the
// /64, so the whole allocation shares one budget.
func TestLoginThrottle_IPv6RotationWithinSlash64CannotEvadeCap(t *testing.T) {
	tt, _ := newLoginThrottle(t, auth.LoginThrottleOptions{
		MaxPerIP:    2,
		MaxPerEmail: 100, // isolate the per-IP dimension
		Window:      time.Hour,
	})
	ctx := context.Background()

	// Every address below is inside 2001:db8:1234:5678::/64.
	rotated := []string{
		"2001:db8:1234:5678::1",
		"2001:db8:1234:5678::2",
		"2001:db8:1234:5678:aaaa:bbbb:cccc:dddd",
	}
	sends := 0
	for i, ip := range rotated {
		ok, err := tt.Allow(ctx, ip, "target@example.com")
		if err != nil {
			t.Fatalf("send %d: unexpected error: %v", i+1, err)
		}
		if ok {
			sends++
		}
	}
	if sends != 2 {
		t.Fatalf("%d sends got through from 3 addresses inside ONE /64 against a 2/window "+
			"per-IP cap; rotating the /128 must not mint fresh buckets", sends)
	}

	// A genuinely different /64 is a different customer and keeps its
	// own budget — the aggregation must not over-collapse.
	if ok, err := tt.Allow(ctx, "2001:db8:1234:9999::1", "target@example.com"); err != nil || !ok {
		t.Errorf("a different /64 must have its own budget: ok=%v err=%v", ok, err)
	}
}
