package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// DefaultSignupThrottleDwellTime (30s) is how long the throttle may fail open on Redis errors before CheckIP fails CLOSED with ErrThrottleUnavailable.
const DefaultSignupThrottleDwellTime = 30 * time.Second

// signupThrottleIncrTimeout bounds the throttle's own Redis round-trip: the increment deliberately ignores the CALLER's cancellation (see CheckIP),
// so a wedged Redis would otherwise pin the handler goroutine forever. 5s is a backstop above go-redis's 3s default.
const signupThrottleIncrTimeout = 5 * time.Second

// RedisSignupIPThrottle is a sliding-window Redis counter, default 5/hour per IP; Redis errors fall open until they persist for the dwell time (default 30s), after which CheckIP returns ErrThrottleUnavailable (503 + Retry-After).
type RedisSignupIPThrottle struct {
	counter   *ratelimit.FixedWindowCounter
	max       int
	keyPrefix string
	dwellTime time.Duration

	// nowFn is the clock source. time.Now in production; tests
	// override via SignupIPThrottleOptions.NowFn to drive the
	// dwell-time logic deterministically without time.Sleep.
	nowFn func() time.Time

	mu              sync.Mutex
	redisErrorSince time.Time
	// healthySince: start of the current unbroken Redis-success run; any failure
	// resets it. redisErrorSince clears only after dwellTime of unbroken success —
	// a single stray success must NOT reopen the throttle (identical to
	// ratelimit.Bucket, which this deliberately mirrors).
	healthySince time.Time
	// lastFailure lets a success that follows a failure-free dwellTime clear
	// the clock, so a long-ago outage cannot make the next blip fail closed.
	lastFailure time.Time
}

// SignupIPThrottleOptions tunes a [RedisSignupIPThrottle].
type SignupIPThrottleOptions struct {
	// Max is the signups permitted per IP within Window. Default 5: tight enough to block bulk-mint, loose enough for a team onboarding
	// through one shared egress.
	Max int
	// Window is the rolling-window length. Default 1 hour.
	Window time.Duration
	// KeyPrefix is the Redis key namespace for this throttle.
	// Default "signup-ip:". Override only in tests.
	KeyPrefix string
	// DwellTime is the fail-open window: errors within it of the first error since the last success fall open, later ones return ErrThrottleUnavailable (503 + Retry-After). Default 30s;
	// negative disables the inversion (fail-open-always), which exposes the sustained-outage bulk-mint vector.
	DwellTime time.Duration
	// NowFn overrides the clock source. Tests inject a synthetic
	// clock to drive dwell-time transitions deterministically; nil
	// uses [time.Now].
	NowFn func() time.Time
}

// NewRedisSignupIPThrottle constructs the throttle; rdb MUST be non-nil.
func NewRedisSignupIPThrottle(rdb redis.UniversalClient, opts SignupIPThrottleOptions) *RedisSignupIPThrottle {
	if rdb == nil {
		panic("auth: NewRedisSignupIPThrottle: rdb must not be nil")
	}
	if opts.Max <= 0 {
		opts.Max = 5
	}
	if opts.Window <= 0 {
		opts.Window = time.Hour
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "signup-ip:"
	}
	// Zero (the Go default) picks the documented default. Operators
	// who explicitly want fail-open-always pass a negative sentinel —
	// see [SignupIPThrottleOptions.DwellTime].
	if opts.DwellTime == 0 {
		opts.DwellTime = DefaultSignupThrottleDwellTime
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &RedisSignupIPThrottle{
		counter:   ratelimit.NewFixedWindowCounter(rdb, opts.Window, nowFn),
		max:       opts.Max,
		keyPrefix: opts.KeyPrefix,
		dwellTime: opts.DwellTime,
		nowFn:     nowFn,
	}
}

// CheckIP increments the per-IP counter: nil under the cap, ErrSignupRateLimited at the cap, ErrThrottleUnavailable after the dwell time of Redis failure, else a wrapped Redis error (the handler falls open).
func (t *RedisSignupIPThrottle) CheckIP(ctx context.Context, ip string) error {
	if ip == "" {
		// No usable IP: let the request through, since the global rate limit also found none; this throttle hardens against IP-rotators, not IP-less direct calls.
		return nil
	}
	// The counter shares ratelimit.Bucket's window trick and first-touch drain TTL (sliding-window approximate, at most 2x the cap at a window crossing: fine for abuse prevention, not billing).
	// The address is masked via ratelimit.ThrottleIPKey first; keying on an IPv6 /128 would be free to bypass, as one /64 holds 2^64 addresses.
	// The increment runs on the caller's VALUES without its cancellation (bounded by signupThrottleIncrTimeout): the counter cannot tell a client RST from a Redis outage, so RSTs would arm the dwell clock and take signup offline for everyone, and an aborted attempt would go uncounted (bulk-mint).
	incrCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signupThrottleIncrTimeout)
	defer cancel()
	count, err := t.counter.Incr(incrCtx, t.keyPrefix+ratelimit.ThrottleIPKey(ip))
	if err != nil {
		if t.observeRedisFailure() {
			return ErrThrottleUnavailable
		}
		return fmt.Errorf("signup throttle: %w", err)
	}
	t.observeRedisSuccess()
	if int(count) > t.max {
		return ErrSignupRateLimited
	}
	return nil
}

// observeRedisFailure arms the dwell clock on the first error if not armed and reports whether the dwell time has elapsed (true maps to ErrThrottleUnavailable, false to a wrapped transport error). A negative DwellTime always returns false.
func (t *RedisSignupIPThrottle) observeRedisFailure() bool {
	if t.dwellTime < 0 {
		return false
	}
	now := t.nowFn()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.healthySince = time.Time{} // any failure breaks the recovery streak
	t.lastFailure = now
	if t.redisErrorSince.IsZero() {
		t.redisErrorSince = now
		return false
	}
	return now.Sub(t.redisErrorSince) > t.dwellTime
}

// observeRedisSuccess clears the fail-closed clock only after dwellTime of UNBROKEN successes (or a success after dwellTime with no failure), not on one success: a flapping Redis would otherwise keep the throttle fail-open indefinitely.
// The recovery signal operators want comes from the success metric. Mirrors ratelimit.Bucket.observeRedisSuccess.
func (t *RedisSignupIPThrottle) observeRedisSuccess() {
	now := t.nowFn()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.redisErrorSince.IsZero() {
		return
	}
	if t.healthySince.IsZero() {
		t.healthySince = now
	}
	if now.Sub(t.healthySince) >= t.dwellTime || now.Sub(t.lastFailure) > t.dwellTime {
		t.redisErrorSince = time.Time{}
		t.healthySince = time.Time{}
	}
}
