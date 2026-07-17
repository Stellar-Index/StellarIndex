package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultDwellTime is the F-0050 / F-0150 fail-open dwell-time
// window: how long Take is allowed to be failing-open on Redis
// errors before flipping to fail-CLOSED via [ErrThrottleUnavailable].
//
// 30s matches [auth.DefaultSignupThrottleDwellTime] (kept in step
// across the two abuse-prevention seams so operators reason about
// one threshold, not two). Long enough for a Redis blip not to
// take rate-limiting offline; short enough that a sustained
// attacker-induced outage can't pivot to unbounded request volume.
const DefaultDwellTime = 30 * time.Second

// ErrThrottleUnavailable signals the rate-limiter has been failing
// open on Redis errors for longer than the configured dwell-time
// and the caller should switch to fail-CLOSED (HTTP 503 +
// Retry-After). Returned by [Bucket.Take] / [Bucket.TakeN] in place
// of the wrapped Redis error once the dwell-time threshold is
// crossed; transient errors within the window keep returning the
// wrapped Redis error so handlers retain their existing fail-open
// branch for blip-class outages. F-0050 / F-0150 (audit-2026-05-27).
var ErrThrottleUnavailable = errors.New("ratelimit: throttle layer unavailable (sustained backend errors)")

// Bucket is a per-(client × window) counter.
//
// Safe for concurrent use. One Bucket serves many callers —
// construct a single instance at binary startup and share it across
// handlers.
//
// # Dwell-time fail-open inversion (F-0050 / F-0150)
//
// Take returns a wrapped Redis error on transport failure while
// the dwell-time clock is still inside its window (default 30s,
// see [DefaultDwellTime]); callers fall open as before. Once Redis
// has been failing continuously for longer than the dwell-time,
// Take returns [ErrThrottleUnavailable] and callers should switch
// to fail-CLOSED (HTTP 503 + Retry-After). A single Redis success
// resets the clock so transient blips never trip the threshold.
type Bucket struct {
	rdb    redis.Cmdable
	max    int
	window time.Duration

	// keyPrefix is "rl:" for production; overridable in tests to
	// avoid collisions with other test files using the same miniredis.
	keyPrefix string

	// nowFn is the clock source. time.Now in production; tests
	// override via WithClock to deterministically advance windows.
	nowFn func() time.Time

	// dwellTime is the fail-open window. Negative disables the
	// inversion (legacy fail-open-always behaviour).
	dwellTime time.Duration

	// local is the in-process fixed-window fallback, non-nil iff rdb
	// was nil at construction. When set, Take/TakeN bypass Redis
	// entirely and enforce the limit from an in-memory map — the
	// fail-CLOSED fallback that keeps the anon/key tiers throttled when
	// Redis is absent at boot (C3-13 / C3-22). See [localStore].
	local *localStore

	mu              sync.Mutex
	redisErrorSince time.Time
}

// Option configures a Bucket at construction.
type Option func(*Bucket)

// WithClock overrides the time source used to derive window buckets.
// Primarily for tests that use miniredis's FastForward and need the
// bucket key to advance with it.
func WithClock(now func() time.Time) Option {
	return func(b *Bucket) { b.nowFn = now }
}

// WithKeyPrefix overrides the "rl:" key prefix — useful only when
// sharing a Redis with another non-stellarindex caller, which we don't.
// Exposed for completeness + test isolation.
func WithKeyPrefix(prefix string) Option {
	return func(b *Bucket) { b.keyPrefix = prefix }
}

// WithDwellTime overrides the F-0050 / F-0150 fail-open dwell-time
// window. Operators with a stricter or looser Redis-availability
// SLO tune this; a negative value disables the dwell-time
// inversion (legacy fail-open-always). Default
// [DefaultDwellTime] (30s).
func WithDwellTime(d time.Duration) Option {
	return func(b *Bucket) { b.dwellTime = d }
}

// Result carries the outcome of [Bucket.Take].
type Result struct {
	// Allowed is true iff the request fits within the current
	// window's budget.
	Allowed bool

	// Remaining is max - current-count after this increment.
	// Clamped to zero when over-limit.
	Remaining int

	// RetryAfter is the time until the window resets. Non-zero
	// only when Allowed is false.
	RetryAfter time.Duration

	// Count is the incremented request count in this window.
	// Useful for debug logging.
	Count int
}

// New constructs a rate-limiter.
//
//   - max    — requests per window.
//   - window — size of the fixed window.
//
// rdb may be nil: the Bucket then falls back to an in-process
// fixed-window limiter (see [localStore]) instead of Redis. This is the
// deliberate degraded mode for a single-instance deployment with no
// Redis — the limits are still ENFORCED (fail-closed), just accounted
// per-process rather than fleet-wide. Every other caller passes a real
// client and gets the Redis path unchanged. C3-13 / C3-22.
//
// Panics on invalid arguments (zero or negative limit / sub-second
// window).
func New(rdb redis.Cmdable, limit int, window time.Duration, opts ...Option) *Bucket {
	if limit <= 0 {
		panic("ratelimit: max must be positive")
	}
	// Windows below 1s would integer-divide to zero in Take()'s
	// bucket-key derivation (`Unix() / int64(window.Seconds())` →
	// divide by zero). The bucket scheme is inherently seconds-
	// based; rejecting sub-second windows fails-fast instead of
	// surfacing the panic at first request.
	if window < time.Second {
		panic("ratelimit: window must be >= 1s")
	}
	b := &Bucket{
		rdb:       rdb,
		max:       limit,
		window:    window,
		keyPrefix: "rl:",
		nowFn:     time.Now,
		dwellTime: DefaultDwellTime,
	}
	for _, opt := range opts {
		opt(b)
	}
	// A nil Redis client selects the in-process fixed-window fallback.
	// Constructed after opts so it inherits any WithClock override
	// through b.nowFn at Take time.
	if rdb == nil {
		b.local = newLocalStore()
	}
	return b
}

// InProcess reports whether this Bucket is running on the in-process
// fixed-window fallback (rdb was nil at construction) rather than
// Redis. Exposed so the binary can log which backend is enforcing the
// limit at startup.
func (b *Bucket) InProcess() bool { return b.local != nil }

// observeRedisFailure stamps the dwell-time clock + reports whether
// it's elapsed. True → caller maps to [ErrThrottleUnavailable];
// false → caller surfaces the wrapped Redis error and the handler
// falls open. Disabled (negative dwellTime) always returns false.
func (b *Bucket) observeRedisFailure() bool {
	if b.dwellTime < 0 {
		return false
	}
	now := b.nowFn()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.redisErrorSince.IsZero() {
		b.redisErrorSince = now
		return false
	}
	return now.Sub(b.redisErrorSince) > b.dwellTime
}

// observeRedisSuccess clears the dwell-time clock. One successful
// round-trip is sufficient to flip back to the fail-open window —
// the post-incident timeline marker operators want is "first OK
// after outage" not "DwellTime of consecutive OKs."
func (b *Bucket) observeRedisSuccess() {
	b.mu.Lock()
	b.redisErrorSince = time.Time{}
	b.mu.Unlock()
}

// Max returns the configured per-window limit. Useful for surfacing
// it in HTTP response headers without threading another value.
func (b *Bucket) Max() int { return b.max }

// Window returns the configured window size.
func (b *Bucket) Window() time.Duration { return b.window }

// lua is the atomic rate-limit check.
//
// KEYS[1]  — the rate-limit key (e.g. "rl:rek_abc:12345")
// ARGV[1]  — TTL in seconds (window * 2 so keys drain)
// ARGV[2]  — max (the limit)
//
// Returns  — two-element array [count, retry_after_seconds].
//
//	retry_after is 0 when allowed.
const lua = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
local max_count = tonumber(ARGV[2])
if current > max_count then
  local ttl = redis.call('TTL', KEYS[1])
  if ttl < 0 then ttl = 0 end
  return {current, ttl}
end
return {current, 0}
`

// luaScript is compiled once at package init. go-redis's Script
// caches the SHA inside the value; re-creating it per Take() call
// wasted a SHA-1 + string-alloc on every rate-limit check. One
// compile per process is enough — the Lua source is immutable.
var luaScript = redis.NewScript(lua)

// Take increments the counter for key in the current window and
// returns whether the request is allowed. One Redis round-trip.
//
// Callers should fail open on error — a Redis outage must not take
// the whole API offline.
func (b *Bucket) Take(ctx context.Context, key string) (Result, error) {
	return b.TakeN(ctx, key, 0)
}

// TakeN is [Bucket.Take] with a per-call limit override. When limit
// is > 0 it replaces the bucket's configured max for this single
// increment; when ≤ 0 the bucket's max is used (so `TakeN(ctx, key, 0)`
// is identical to `Take(ctx, key)`).
//
// Use this for per-subject limits that vary at runtime — e.g. a paid
// customer with a `rate_limit_per_min` override on their API-key
// record. The Redis key is shared across calls (so concurrent
// requests still race against the same counter); the bucket itself
// is stateless w.r.t. limit — the value is only consulted at Take
// time, passed through to the Lua script as ARGV[2].
//
// Callers MUST pass a stable limit for any given Redis key —
// flipping the limit between calls would race the in-flight counter
// against an inconsistent threshold and produce undefined allow/deny
// outcomes for callers near the boundary. In practice this means the
// per-key override must be sticky for the key's lifetime, which is
// the design today (RateLimitPerMin lives on the APIKeyRecord, not
// on the request).
func (b *Bucket) TakeN(ctx context.Context, key string, limit int) (Result, error) {
	effectiveMax := b.max
	if limit > 0 {
		effectiveMax = limit
	}
	minute := b.nowFn().Unix() / int64(b.window.Seconds())

	// In-process fallback (rdb was nil at construction). No Redis
	// round-trip, no error path, no fail-open — the limit is enforced
	// from an in-memory fixed-window map. C3-13 / C3-22.
	if b.local != nil {
		return b.localTakeN(key, effectiveMax, minute), nil
	}

	// url.QueryEscape the caller-supplied key before concatenating
	// with ":<minute>". Callers pass things like IPv6 addresses (which
	// contain `:`), future API-key strings, SEP-10 account IDs; any
	// `:` in the key would make two distinct keys collide on the same
	// Redis slot — a silent cross-client rate-limit smear. Escaping
	// all RFC 3986 reserved chars also hardens against injection of
	// exotic control bytes. Still human-readable in Redis for ops.
	rlKey := b.keyPrefix + url.QueryEscape(key) + ":" + strconv.FormatInt(minute, 10)
	// Double-window TTL so keys drain naturally. Floor-at-1 guards
	// sub-second windows — Redis treats EXPIRE 0 as "no expiry",
	// which would leak keys forever for any window < 500ms. 1s
	// floor is still finite; the exact value doesn't matter because
	// those windows would roll over on the next Take() anyway.
	ttlSeconds := int(b.window.Seconds() * 2)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	resRaw, err := luaScript.Run(ctx, b.rdb, []string{rlKey},
		ttlSeconds, effectiveMax,
	).Result()
	if err != nil {
		if b.observeRedisFailure() {
			return Result{}, ErrThrottleUnavailable
		}
		return Result{}, fmt.Errorf("ratelimit: eval: %w", err)
	}
	b.observeRedisSuccess()

	arr, ok := resRaw.([]any)
	if !ok || len(arr) != 2 {
		return Result{}, fmt.Errorf("ratelimit: unexpected result shape: %T", resRaw)
	}

	count, ok := arr[0].(int64)
	if !ok {
		return Result{}, fmt.Errorf("ratelimit: count not int64: %T", arr[0])
	}
	retryTTL, ok := arr[1].(int64)
	if !ok {
		return Result{}, fmt.Errorf("ratelimit: retry_after not int64: %T", arr[1])
	}

	// Use `count` as the authoritative allow-signal, not retryTTL.
	// The Lua clamps a negative TTL to 0 on the deny path, and Go
	// must not read "retryTTL == 0" as "allowed" — a TTL<0 race
	// (key lazy-evicted between INCR and TTL) would otherwise flip
	// a deny into an allow. Pair-comparing against max_count is the
	// same check the Lua makes; keeping them symmetric eliminates
	// the decoupling bug entirely.
	allowed := int(count) <= effectiveMax
	remaining := effectiveMax - int(count)
	if remaining < 0 {
		remaining = 0
	}
	// RetryAfter is "time until caller can succeed". That's the
	// REMAINING SECONDS IN THE CURRENT WINDOW, not the Redis-side
	// key TTL (which is 2× window for drain purposes). Computing
	// on the Go side instead of reading ttl from Lua means the
	// client's Retry-After header doesn't sit at a drain-padded
	// value that's up to 60× longer than the actual reset. Floor
	// at 1s so the HTTP header is never zero on a denial.
	var retryAfter time.Duration
	if !allowed {
		windowSec := int64(b.window.Seconds())
		elapsed := b.nowFn().Unix() - minute*windowSec
		remainingSec := windowSec - elapsed
		if remainingSec < 1 {
			remainingSec = 1
		}
		retryAfter = time.Duration(remainingSec) * time.Second
	}
	_ = retryTTL // intentionally unused — kept on the wire for debug
	return Result{
		Allowed:    allowed,
		Remaining:  remaining,
		RetryAfter: retryAfter,
		Count:      int(count),
	}, nil
}

// localTakeN is the in-process fixed-window path taken when rdb was nil
// at construction. Mirrors the Redis path's Result derivation
// (Remaining clamped at zero; RetryAfter = seconds left in the current
// window, floored at 1s on a denial) so callers can't tell the two
// backends apart from the Result shape. Never errors — the whole point
// of the fallback is that it stays enforced when Redis is unavailable.
func (b *Bucket) localTakeN(key string, effectiveMax int, minute int64) Result {
	now := b.nowFn()
	count, allowed := b.local.take(key, minute, effectiveMax, now, b.window)

	remaining := effectiveMax - count
	if remaining < 0 {
		remaining = 0
	}

	var retryAfter time.Duration
	if !allowed {
		windowSec := int64(b.window.Seconds())
		elapsed := now.Unix() - minute*windowSec
		remainingSec := windowSec - elapsed
		if remainingSec < 1 {
			remainingSec = 1
		}
		retryAfter = time.Duration(remainingSec) * time.Second
	}

	return Result{
		Allowed:    allowed,
		Remaining:  remaining,
		RetryAfter: retryAfter,
		Count:      count,
	}
}
