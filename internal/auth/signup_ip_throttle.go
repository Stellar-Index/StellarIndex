package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// DefaultSignupThrottleDwellTime is the F-0049 / F-0149 dwell-time
// window: how long the throttle is allowed to be failing-open on
// Redis errors before [RedisSignupIPThrottle.CheckIP] flips to
// fail-CLOSED with [ErrThrottleUnavailable].
//
// 30s is the documented tradeoff: long enough that a single Redis
// blip (MISCONF, brief network partition, fail-over) doesn't take
// signup offline, short enough that the J40 adversarial vector
// (sustained outage to disable abuse-prevention) can't pivot to
// bulk-mint accounts indefinitely. Tune via
// [SignupIPThrottleOptions.DwellTime] if the operator has a
// different Redis-availability SLO.
const DefaultSignupThrottleDwellTime = 30 * time.Second

// RedisSignupIPThrottle implements [v1.SignupIPThrottle] (the per-IP
// signup rate-limit boundary, declared in v1 to keep the v1 package
// the source of truth for its own boundaries) with a sliding-window
// Redis counter.
//
// The signup endpoint sees one request per attempted account; the
// global anonymous rate limit caps at 60/min per IP — plenty for
// browsing the public surfaces but lets a single IP bulk-mint
// 60 accounts/min × 60 min = 3,600/hr of email→key_id pairs.
// The default 5/hour cap here closes that vector while still
// letting a legitimate operator onboarding a small team through a
// single shared egress complete normally; operators tune up via
// `signup_ip_max_per_window` in the API config.
//
// F-1232 (audit-2026-05-12).
//
// # Dwell-time fail-open inversion (F-0049 / F-0149)
//
// On a Redis transport failure CheckIP starts a dwell-time clock.
// Errors observed inside the window (default 30s, see
// [DefaultSignupThrottleDwellTime]) propagate as wrapped Redis
// errors and the handler falls open as before. Once the window is
// exceeded — i.e. Redis has been failing continuously for longer
// than the dwell-time — CheckIP returns [ErrThrottleUnavailable]
// and the handler returns 503 + Retry-After instead. A single
// Redis success resets the clock so transient blips never trip the
// threshold.
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
}

// SignupIPThrottleOptions tunes a [RedisSignupIPThrottle].
type SignupIPThrottleOptions struct {
	// Max is the maximum number of signups permitted per IP within
	// Window. Default 5 — tight enough to block bulk-mint, loose
	// enough that a legitimate operator onboarding a small team
	// through a single shared egress completes normally.
	Max int
	// Window is the rolling-window length. Default 1 hour.
	Window time.Duration
	// KeyPrefix is the Redis key namespace for this throttle.
	// Default "signup-ip:". Override only in tests.
	KeyPrefix string
	// DwellTime is the F-0049 / F-0149 fail-open window. Errors
	// observed within DwellTime of the first error since the last
	// success fall open as today; errors observed past DwellTime
	// return [ErrThrottleUnavailable] so the handler emits 503 +
	// Retry-After. Default [DefaultSignupThrottleDwellTime] (30s).
	// Set to a negative value to disable the dwell-time inversion
	// (legacy fail-open-always behaviour) — operators should NOT
	// reach for this without understanding the J40 vector.
	DwellTime time.Duration
	// NowFn overrides the clock source. Tests inject a synthetic
	// clock to drive dwell-time transitions deterministically; nil
	// uses [time.Now].
	NowFn func() time.Time
}

// NewRedisSignupIPThrottle constructs the throttle. rdb MUST be
// non-nil; pass a no-op v1.SignupIPThrottle (or simply leave the
// Options.SignupIPThrottle field nil) for deployments without
// Redis.
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

// CheckIP increments the per-IP counter for the current window.
// Returns nil while under the cap, [ErrSignupRateLimited] once the
// cap is reached, [ErrThrottleUnavailable] when Redis has been
// failing for longer than the configured dwell-time (the handler
// returns 503 + Retry-After), and a wrapped Redis error on
// transient transport failure (the handler treats those as
// fail-open — better than taking signup offline because Redis
// blipped briefly).
func (t *RedisSignupIPThrottle) CheckIP(ctx context.Context, ip string) error {
	if ip == "" {
		// No usable IP — let the request through; the global
		// rate-limit middleware also failed to find one and capped
		// via its own fallback. F-1232 hardens against IP-rotators,
		// not against IP-less direct calls (which production
		// shouldn't see — Caddy + Cloudflare always populate one).
		return nil
	}
	// The shared counter uses the same window-bucket trick as
	// ratelimit.Bucket (round the current time to the window) and owns
	// the first-touch drain TTL. Sliding-window approximate; gives at
	// most 2× the cap during a window-crossing burst, which is
	// acceptable for an abuse-prevention threshold (not for a strict
	// billing meter).
	count, err := t.counter.Incr(ctx, t.keyPrefix+ip)
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

// observeRedisFailure stamps the first-error timestamp on the
// dwell-time clock if it isn't already armed, and reports whether
// the dwell-time has elapsed. Callers should map true to
// [ErrThrottleUnavailable] and false to a wrapped transport error
// (the handler falls open on the latter).
//
// Disabled (negative DwellTime) always returns false — preserves
// the pre-F-0049 fail-open-always behaviour for operators who
// explicitly opt out.
func (t *RedisSignupIPThrottle) observeRedisFailure() bool {
	if t.dwellTime < 0 {
		return false
	}
	now := t.nowFn()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.redisErrorSince.IsZero() {
		t.redisErrorSince = now
		return false
	}
	return now.Sub(t.redisErrorSince) > t.dwellTime
}

// observeRedisSuccess resets the dwell-time clock. A single
// successful Redis round-trip is sufficient to flip back to the
// fail-open window — operators inspecting the post-incident
// timeline want "first OK after outage" as the recovery marker,
// not "DwellTime of consecutive OKs."
func (t *RedisSignupIPThrottle) observeRedisSuccess() {
	t.mu.Lock()
	t.redisErrorSince = time.Time{}
	t.mu.Unlock()
}
