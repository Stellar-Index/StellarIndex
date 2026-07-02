package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/StellarIndex/stellar-index/internal/ratelimit"
)

// RedisLoginThrottle implements [dashboardauth.LoginThrottle] — the
// magic-link send throttle (audit-2026-06-14 A12). The /v1/auth/login
// endpoint fires an outbound email per accepted request, bounded only by
// the global anonymous per-IP rate-limit (60/min). That lets a single IP
// (a) bomb one victim inbox and (b) burn the deployment's email-send quota
// / sender reputation. This adds two sliding-window caps — per IP and per
// TARGET email — and denies the send when EITHER is exhausted.
//
// Defaults: 10 sends/hour/IP, 5 sends/hour/email. A legitimate user almost
// never needs >5 links/hour to their own inbox; an IP fronting a small team
// stays under 10. Operators tune via [LoginThrottleOptions].
//
// Fail-open: on a Redis transport error Allow returns (false, err); the
// handler checks the error first and falls open (sends), preferring login
// availability over a brief abuse window — the global rate-limit still caps
// per-IP volume. (Unlike the signup throttle there is no dwell-time
// fail-CLOSED inversion: a magic-link flood is a nuisance, not the
// bulk-account-mint vector signup guards.)
type RedisLoginThrottle struct {
	counter    *ratelimit.FixedWindowCounter
	maxPerIP   int
	maxPerMail int
	keyPrefix  string
}

// LoginThrottleOptions tunes a [RedisLoginThrottle]. Zero values pick the
// documented defaults.
type LoginThrottleOptions struct {
	MaxPerIP    int           // sends/window/IP (default 10)
	MaxPerEmail int           // sends/window/target-email (default 5)
	Window      time.Duration // rolling window (default 1h)
	KeyPrefix   string        // Redis namespace (default "login-throttle:")
	NowFn       func() time.Time
}

// NewRedisLoginThrottle constructs the throttle. rdb MUST be non-nil; leave
// the dashboardauth Config.LoginThrottle field nil for Redis-less deploys.
func NewRedisLoginThrottle(rdb redis.UniversalClient, opts LoginThrottleOptions) *RedisLoginThrottle {
	if rdb == nil {
		panic("auth: NewRedisLoginThrottle: rdb must not be nil")
	}
	if opts.MaxPerIP <= 0 {
		opts.MaxPerIP = 10
	}
	if opts.MaxPerEmail <= 0 {
		opts.MaxPerEmail = 5
	}
	if opts.Window <= 0 {
		opts.Window = time.Hour
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "login-throttle:"
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &RedisLoginThrottle{
		counter:    ratelimit.NewFixedWindowCounter(rdb, opts.Window, nowFn),
		maxPerIP:   opts.MaxPerIP,
		maxPerMail: opts.MaxPerEmail,
		keyPrefix:  opts.KeyPrefix,
	}
}

// Allow increments the per-IP and per-email counters for the current window
// and reports whether BOTH are within their caps. Returns (false, err) on a
// Redis transport failure — the handler falls open on a non-nil error. The
// email is hashed (sha256, 16-hex prefix) before it becomes a Redis key, so
// plaintext addresses never land in Redis.
func (t *RedisLoginThrottle) Allow(ctx context.Context, ip, email string) (bool, error) {
	allowed := true

	// Per-target-email cap (the email-bomb dimension). An empty email
	// shouldn't reach here (handler validates first) but guard anyway.
	if email != "" {
		ok, err := t.incrUnderCap(ctx, t.keyPrefix+"mail:"+hashEmail(email), t.maxPerMail)
		if err != nil {
			return false, err
		}
		allowed = allowed && ok
	}

	// Per-IP cap (the spray-many-addresses dimension). An IP-less direct
	// call (production shouldn't see one — Caddy/Cloudflare populate it)
	// skips the IP dimension, same as the signup throttle.
	if ip != "" {
		ok, err := t.incrUnderCap(ctx, t.keyPrefix+"ip:"+ip, t.maxPerIP)
		if err != nil {
			return false, err
		}
		allowed = allowed && ok
	}

	return allowed, nil
}

// incrUnderCap bumps the shared fixed-window counter (which appends the
// window bucket + owns the drain TTL) and reports whether the
// post-increment count is within limit.
func (t *RedisLoginThrottle) incrUnderCap(ctx context.Context, keyBase string, limit int) (bool, error) {
	count, err := t.counter.Incr(ctx, keyBase)
	if err != nil {
		return false, fmt.Errorf("login throttle: %w", err)
	}
	return int(count) <= limit, nil
}

// hashEmail returns a short stable digest of a lowercased email for use as a
// Redis key fragment — never the plaintext address.
func hashEmail(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:8])
}
