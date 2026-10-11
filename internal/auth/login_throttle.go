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

// RedisLoginThrottle implements dashboardauth.LoginThrottle, the magic-link send throttle: per-IP (10/hour) and per-TARGET-email (5/hour) sliding-window caps, send denied when EITHER is exhausted, since /v1/auth/login otherwise lets one IP bomb a victim inbox and burn email quota and sender reputation.
// Redis outage: the handler falls open on a throttle error, so every increment is also applied to an in-process fixed-window counter with the SAME caps; Redis is authoritative while it answers, the local count decides on error.
// This beats the signup throttle's fail-closed inversion, which would take login offline for a blip. Once the degraded cap is exhausted Allow returns (false, nil), a definitive DENY; the over-quota branch still returns the generic 200, so anti-enumeration holds.
type RedisLoginThrottle struct {
	counter    *ratelimit.FixedWindowCounter
	fallback   *ratelimit.LocalFixedWindowCounter
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
		fallback:   ratelimit.NewLocalFixedWindowCounter(opts.Window, nowFn),
		maxPerIP:   opts.MaxPerIP,
		maxPerMail: opts.MaxPerEmail,
		keyPrefix:  opts.KeyPrefix,
	}
}

// Allow increments the per-IP and per-email counters and reports whether BOTH are within cap; the email is hashed (sha256, 16-hex prefix) so plaintext never reaches Redis.
// (true, nil) send; (false, nil) over quota, also once the in-process cap is exhausted during an outage, where the Redis error is deliberately NOT returned since the handler falls open on any non-nil error; (true, err) Redis down but local cap has room.
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

// HashEmail returns a short stable digest of a canonicalised email for use as a Redis key fragment, never the plaintext. Normalisation happens HERE so the per-email cap buckets every spelling of one inbox together whether or not the caller normalised;
// it is exported so the in-process fallback (cmd/stellarindex-api inProcessLoginThrottle) shares the identity. Canonicalisation is RFC-5322 aware (addr-spec extracted), since case+trim alone gives each spelling its own 5/hour budget, bypassing the inbox-bombing cap; unparseable input falls back to case+trim.
func HashEmail(email string) string {
	normalized := CanonicalEmail(email)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// CanonicalEmail reduces an address to its lower-cased addr-spec, stripping display name, angle brackets, a `+tag` suffix and, for gmail.com/googlemail.com only, local-part dots; unparseable input returns lower-cased, trimmed input (throttle-bucket identity only, so it must never reject).
// Without it `victim+1@gmail.com`, `victim+2@gmail.com` and `vic.tim@gmail.com` each get their own budget for one inbox.
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
