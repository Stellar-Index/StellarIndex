package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// RedisLoginThrottle is the magic-link send throttle: per-IP (10/hour) and per-target-email (5/hour) sliding caps, denied when EITHER is exhausted; Redis errors fall open to an in-process counter with the same caps.
type RedisLoginThrottle struct {
	counter    *ratelimit.FixedWindowCounter
	fallback   *ratelimit.LocalFixedWindowCounter
	maxPerIP   int
	maxPerMail int
	keyPrefix  string
}

// LoginThrottleOptions tunes a [RedisLoginThrottle]; zero values pick the documented defaults.
type LoginThrottleOptions struct {
	MaxPerIP    int           // sends/window/IP (default 10)
	MaxPerEmail int           // sends/window/target-email (default 5)
	Window      time.Duration // rolling window (default 1h)
	KeyPrefix   string        // Redis namespace (default "login-throttle:")
	NowFn       func() time.Time
}

// NewRedisLoginThrottle constructs the throttle; rdb MUST be non-nil.
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
		fallback:   ratelimit.NewLocalFixedWindowCounter(opts.Window, nowFn),
		maxPerIP:   opts.MaxPerIP,
		maxPerMail: opts.MaxPerEmail,
		keyPrefix:  opts.KeyPrefix,
	}
}

// Allow increments the per-IP and per-email counters and reports whether BOTH are within cap; the email is hashed so plaintext never reaches Redis, and a definitive deny returns (false, nil) rather than an error.
func (t *RedisLoginThrottle) Allow(ctx context.Context, ip, email string) (bool, error) {
	allowed := true
	var firstErr error

	// Per-target-email cap (the email-bomb dimension). An empty email
	// shouldn't reach here (handler validates first) but guard anyway.
	if email != "" {
		ok, err := t.incrUnderCap(ctx, t.keyPrefix+"mail:"+HashEmail(email), t.maxPerMail)
		allowed = allowed && ok
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Per-IP cap (the spray-many-addresses dimension); an IP-less direct call skips it, as in the signup throttle. The address is masked via ratelimit.ThrottleIPKey, else an attacker with any IPv6 /64 rotates the /128 and never shares a bucket.
	if ip != "" {
		ok, err := t.incrUnderCap(ctx, t.keyPrefix+"ip:"+ratelimit.ThrottleIPKey(ip), t.maxPerIP)
		allowed = allowed && ok
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if !allowed {
		// Definitive deny. Swallow any Redis error: this decision came
		// from a counter that answered (Redis, or the in-process
		// fallback), and surfacing the error would route the handler
		// into its fall-open branch and send the mail anyway.
		return false, nil
	}
	return true, firstErr
}

// incrUnderCap bumps BOTH the Redis fixed-window counter and the in-process fallback, then reports whether the send is within limit.
// Redis is authoritative when it answers (fleet-wide, count >= local); the fallback is incremented on every call so it is already warm when Redis drops, and on a transport error the local verdict is returned with the error.
func (t *RedisLoginThrottle) incrUnderCap(ctx context.Context, keyBase string, limit int) (bool, error) {
	localOK := t.fallback.Allow(keyBase, limit)
	count, err := t.counter.Incr(ctx, keyBase)
	if err != nil {
		return localOK, fmt.Errorf("login throttle: %w", err)
	}
	return int(count) <= limit, nil
}

// HashEmail returns a short stable digest of the canonicalised email for a Redis key, never the plaintext; it normalises here so every spelling of one inbox shares the per-email cap.
func HashEmail(email string) string {
	normalized := CanonicalEmail(email)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// CanonicalEmail reduces an address to its lower-cased addr-spec (no display name, `+tag`, or gmail local-part dots) for throttle bucketing only; unparseable input returns lower-cased, trimmed input and it never rejects.
func CanonicalEmail(email string) string {
	trimmed := strings.ToLower(strings.TrimSpace(email))
	addrSpec := trimmed
	if addr, err := mail.ParseAddress(trimmed); err == nil {
		addrSpec = strings.ToLower(strings.TrimSpace(addr.Address))
	}
	local, domain, ok := strings.Cut(addrSpec, "@")
	if !ok {
		return addrSpec
	}
	if tag := strings.IndexByte(local, '+'); tag >= 0 {
		local = local[:tag]
	}
	if domain == "googlemail.com" {
		// googlemail.com is Google's own historical alias for gmail.com
		// (same mailbox, same provider) — fold it so the two domain
		// spellings share one bucket too.
		domain = "gmail.com"
	}
	if domain == "gmail.com" {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local + "@" + domain
}
