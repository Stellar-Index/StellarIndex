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

// RedisLoginThrottle implements [dashboardauth.LoginThrottle] — the
// magic-link send throttle. The /v1/auth/login
// endpoint fires an outbound email per accepted request, bounded only by
// the global anonymous per-IP rate-limit. That lets a single IP
// (a) bomb one victim inbox and (b) burn the deployment's email-send quota
// / sender reputation. This adds two sliding-window caps — per IP and per
// TARGET email — and denies the send when EITHER is exhausted.
//
// Defaults: 10 sends/hour/IP, 5 sends/hour/email. A legitimate user almost
// never needs >5 links/hour to their own inbox; an IP fronting a small team
// stays under 10. Operators tune via [LoginThrottleOptions].
//
// # Redis outage: degrade, don't disappear
//
// The handler falls open on a throttle error, so a Redis-only throttle
// would drop the per-target-email cap for as long as Redis is down, and
// one IP could pump unbounded magic-link mail at one inbox (victim
// harassment, plus sender-reputation and email-quota burn that outlives
// the outage). An attacker who can hold Redis down, or who merely waits
// for a fail-over, would disable the control by definition.
//
// Every increment is therefore also applied to an in-process fixed-window
// counter with the SAME caps. While Redis answers it stays authoritative
// (fleet-wide accounting); when it errors, the in-process count decides.
// A legitimate user needs ≤5 links/hour and never notices; a bomber is
// capped per process instead of not at all. That is deliberately chosen
// over the signup throttle's dwell-time fail-CLOSED inversion, which
// would take dashboard login fully offline for the duration of a
// Redis blip — a worse outcome than the nuisance it prevents, and
// unnecessary when the cap can simply keep being enforced locally.
//
// Consequently Allow returns (false, nil) — a definitive DENY, not a
// fall-open error — once the degraded cap is exhausted, because the
// handler inspects the error first and would otherwise send anyway. The
// handler's over-quota branch already skips the send while still
// returning the generic 200 {status:"sent"}, so the anti-enumeration
// contract holds.
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

// Allow increments the per-IP and per-email counters for the current window
// and reports whether BOTH are within their caps. The email is hashed
// (sha256, 16-hex prefix) before it becomes a Redis key, so plaintext
// addresses never land in Redis.
//
// Return contract:
//
//   - (true, nil)   — within quota; send.
//   - (false, nil)  — over quota on at least one dimension; skip the send.
//     Also the answer once the in-process fallback's cap is
//     exhausted during a Redis outage — the
//     Redis error is deliberately NOT returned in that case,
//     because the handler falls open on any non-nil error and
//     would send anyway.
//   - (true, err)   — Redis is unreachable but the degraded in-process
//     cap still has room; the handler logs and sends.
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

	// Per-IP cap (the spray-many-addresses dimension). An IP-less direct
	// call (production shouldn't see one — Caddy/Cloudflare populate it)
	// skips the IP dimension, same as the signup throttle. The address is
	// masked to its throttle-key identity first — see [ratelimit.ThrottleIPKey];
	// without that an attacker with any IPv6 /64 rotates the /128 and
	// never shares a bucket with themselves.
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

// incrUnderCap bumps BOTH the Redis fixed-window counter (which appends
// the window bucket + owns the drain TTL) and the in-process fallback,
// then reports whether the send is within limit.
//
// Redis is authoritative whenever it answers — it accounts fleet-wide and
// its count is always >= the local one. The fallback is incremented on
// every call regardless, so its window is already warm when Redis drops:
// otherwise the first N requests after an outage began would each get a
// fresh local budget. On a transport error the local verdict is what the
// caller gets, alongside the error.
func (t *RedisLoginThrottle) incrUnderCap(ctx context.Context, keyBase string, limit int) (bool, error) {
	localOK := t.fallback.Allow(keyBase, limit)
	count, err := t.counter.Incr(ctx, keyBase)
	if err != nil {
		return localOK, fmt.Errorf("login throttle: %w", err)
	}
	return int(count) <= limit, nil
}

// HashEmail returns a short stable digest of a canonicalised email for use as
// a Redis key fragment — never the plaintext address. Normalisation is applied
// HERE so the per-target-email cap buckets every spelling of one inbox to the
// same key regardless of whether the caller pre-normalised — the throttle
// invariant is self-enforcing rather than a promise the caller must keep.
// Exported so the Redis-less in-process fallback (cmd/stellarindex-api's
// inProcessLoginThrottle) shares this identity instead of hand-rolling its
// own un-canonicalised sha256.
//
// Canonicalisation is RFC-5322 aware, not just case+trim: the addr-spec is
// extracted so `victim@x.com`, `<victim@x.com>` and `"display" <victim@x.com>`
// share one bucket. Case+trim alone would give each spelling its OWN 5/hour
// budget while all three deliver to the same inbox — so the per-email cap,
// whose entire purpose is bounding inbox-bombing, would be bypassable by
// re-spelling the target. Unparseable input falls back to case+trim.
func HashEmail(email string) string {
	normalized := CanonicalEmail(email)
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// CanonicalEmail reduces an address to its lower-cased addr-spec
// ("user@host"), stripping any display name and angle brackets, a `+tag`
// subaddress suffix, and — for gmail.com/googlemail.com only, where it is a
// documented provider convention rather than a general RFC rule — dots in
// the local part. Returns the lower-cased, trimmed input when it does not
// parse as an address — callers use this only for throttle-bucket identity,
// so a best-effort answer is correct; it must never reject.
//
// Without this, `victim+1@gmail.com`, `victim+2@gmail.com` and
// `vic.tim@gmail.com` each mint their own per-email budget while all three
// deliver to one inbox — the same re-spelling bypass the
// RFC-5322 addr-spec extraction closes for angle-bracket/display-name
// spellings, just via subaddressing instead.
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
