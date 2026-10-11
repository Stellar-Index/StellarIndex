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

// DefaultDwellTime is how long Take may fail open on Redis errors before flipping to fail-CLOSED
// via [ErrThrottleUnavailable]. 30s matches [auth.DefaultSignupThrottleDwellTime] so operators
// reason about one threshold. It is long enough to ride out a Redis blip, short enough that a
// sustained attacker-induced outage cannot pivot to unbounded request volume.
const DefaultDwellTime = 30 * time.Second

// ErrThrottleUnavailable signals the limiter has failed open on Redis errors for longer than the
// dwell-time and the caller should fail CLOSED (HTTP 503 + Retry-After). [Bucket.Take] and
// [Bucket.Charge] return it in place of the wrapped Redis error; errors within the window keep
// the wrapped error so handlers retain their fail-open branch for blips.
var ErrThrottleUnavailable = errors.New("ratelimit: throttle layer unavailable (sustained backend errors)")

// Bucket is a per-(client × window) counter.
//
// Safe for concurrent use. Construct one instance at startup and share it across handlers.
//
// # Dwell-time fail-open inversion
//
// Take returns a wrapped Redis error on transport failure while the dwell-time clock is inside
// its window (see [DefaultDwellTime]); callers fall open. Once Redis has failed continuously for
// longer than the dwell-time, Take returns [ErrThrottleUnavailable] and callers should fail
// CLOSED. Only sustained recovery (a full dwell-time of unbroken successes, or a success after a
// dwell-time with no failure) resets the clock; a stray success under a flapping Redis does not
// (see healthySince).
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
	// inversion, so every Redis error fails open.
	dwellTime time.Duration

	// local is the in-process fixed-window fallback, non-nil iff rdb was nil at construction.
	// Take/Charge then bypass Redis and enforce from memory, keeping the anon/key tiers throttled
	// when Redis is absent at boot. See [localStore].
	local *localStore

	mu              sync.Mutex
	redisErrorSince time.Time
	// healthySince marks the start of the CURRENT unbroken run of Redis successes; any failure
	// resets it to zero. redisErrorSince (the fail-closed clock) is cleared only once this streak
	// has lasted dwellTime. A stray success under a flapping Redis must NOT reset the clock, or
	// the limiter and the auth/signup throttles sharing this type would stay fail-open while
	// Redis is effectively down.
	healthySince time.Time
	// lastFailure lets a success that follows a failure-free dwellTime clear
	// the clock, so a long-ago outage cannot make the next blip fail closed.
	lastFailure time.Time
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

// WithDwellTime overrides how long Redis may keep failing before Take stops failing open and
// returns [ErrThrottleUnavailable]. Operators tune it via api.rate_limit_dwell
// (config.APIConfig.RateLimitDwell); a negative value disables the inversion, so every Redis
// error fails open. Default [DefaultDwellTime].
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
// rdb may be nil: the Bucket then uses an in-process fixed-window limiter (see [localStore]).
// This is the deliberate degraded mode for a single-instance deployment with no Redis; limits
// are still ENFORCED (fail-closed), just per-process rather than fleet-wide.
//
// Panics on invalid arguments (zero or negative limit, sub-second window).
func New(rdb redis.Cmdable, limit int, window time.Duration, opts ...Option) *Bucket {
	if limit <= 0 {
		panic("ratelimit: max must be positive")
	}
	// Sub-second windows would divide by zero in Take()'s bucket-key derivation
	// (`Unix() / int64(window.Seconds())`); reject them here so the failure is at construction,
	// not the first request.
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
	b.healthySince = time.Time{} // any failure breaks the recovery streak
	b.lastFailure = now
	if b.redisErrorSince.IsZero() {
		b.redisErrorSince = now
		return false
	}
	return now.Sub(b.redisErrorSince) > b.dwellTime
}

// observeRedisSuccess advances the recovery streak. The fail-closed clock (redisErrorSince) is
// cleared only after dwellTime of UNBROKEN successes, not on a single success: under a flapping
// Redis the interspersed failures keep resetting healthySince, so the clock stays armed and
// observeRedisFailure trips fail-CLOSED instead of failing open forever. A success after
// dwellTime with no observed failure also clears it, since the flap evidence has aged out and
// an old sparse-traffic outage would otherwise make an unrelated blip fail closed.
func (b *Bucket) observeRedisSuccess() {
	now := b.nowFn()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.redisErrorSince.IsZero() {
		return // not in the error state; nothing to recover
	}
	if b.healthySince.IsZero() {
		b.healthySince = now
	}
	if now.Sub(b.healthySince) >= b.dwellTime || now.Sub(b.lastFailure) > b.dwellTime {
		b.redisErrorSince = time.Time{}
		b.healthySince = time.Time{}
	}
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
// ARGV[3]  — cost (tokens this call spends; >= 1)
//
// Returns  — two-element array [count, retry_after_seconds].
//
//	retry_after is 0 when allowed.
//
// The expiry is armed when the post-increment count EQUALS the cost, i.e. this call created
// the key. Keying it on 1 would leave a key first written by a cost-N charge with no TTL.
const lua = `
local cost = tonumber(ARGV[3])
local current = redis.call('INCRBY', KEYS[1], cost)
if current == cost then
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

// Take increments the counter for key in the current window and reports whether the request is
// allowed, in one Redis round-trip.
//
// On error the caller must branch on [ErrThrottleUnavailable]: a wrapped Redis error (inside the
// dwell-time) fails OPEN, ErrThrottleUnavailable fails CLOSED with 503 + Retry-After. Treating
// every error as fail-open re-opens the sustained-outage bypass (package doc, "Failure mode").
func (b *Bucket) Take(ctx context.Context, key string) (Result, error) {
	return b.Charge(ctx, key, 1, 0)
}

// Charge spends cost tokens against key's counter for the current window in ONE atomic step and
// reports whether the request still fits. `Take(ctx, key)` is `Charge(ctx, key, 1, 0)`. A
// request's price must track the work it buys, or a 1000-id batch is a 1000x amplifier.
//
// cost and limit are different axes. limit > 0 replaces the bucket's max for this call only (a
// per-subject ceiling such as an API key's `rate_limit_per_min`); <= 0 uses the bucket's max.
// Callers MUST pass a stable limit per key, or the shared counter races inconsistent thresholds.
//
// One round-trip whatever the cost (INCRBY). Never weight a request by looping Take: that is not
// atomic against concurrent callers.
//
// Cost is clamped to [1, effective limit]: below 1 charges 1 (a negative INCRBY would REFUND
// budget); above the limit charges the whole limit (else a big batch could never succeed).
// A denied charge is still counted; its Retry-After names the window end.
func (b *Bucket) Charge(ctx context.Context, key string, cost, limit int) (Result, error) {
	effectiveMax := b.max
	if limit > 0 {
		effectiveMax = limit
	}
	cost = clampCost(cost, effectiveMax)
	minute := b.nowFn().Unix() / int64(b.window.Seconds())

	// In-process fallback (rdb was nil at construction). No Redis
	// round-trip, no error path, no fail-open — the limit is enforced
	// from an in-memory fixed-window map.
	if b.local != nil {
		return b.localCharge(key, cost, effectiveMax, minute), nil
	}

	// Escape the caller-supplied key before appending ":<minute>". Keys such as IPv6 addresses
	// contain `:`, which would let two distinct keys collide on one Redis slot and smear limits
	// across clients. Escaping also blocks control-byte injection and stays readable for ops.
	rlKey := b.keyPrefix + url.QueryEscape(key) + ":" + strconv.FormatInt(minute, 10)
	// Double-window TTL so keys drain naturally. The floor of 1 guards sub-second windows:
	// Redis treats EXPIRE 0 as "no expiry", which would leak keys forever.
	ttlSeconds := int(b.window.Seconds() * 2)
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	resRaw, err := luaScript.Run(ctx, b.rdb, []string{rlKey},
		ttlSeconds, effectiveMax, cost,
	).Result()
	if err != nil {
		// A caller abort says nothing about Redis health, so it neither arms the
		// dwell clock nor breaks a recovery streak. A deadline still counts: a
		// Redis too slow to answer in time is a failing Redis.
		if errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, fmt.Errorf("ratelimit: eval: %w", err)
		}
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

	// Use `count` as the authoritative allow-signal, not retryTTL. The Lua clamps a negative TTL
	// to 0 on the deny path, so reading "retryTTL == 0" as allowed would flip a deny into an
	// allow when the key is lazily evicted between INCR and TTL. Comparing count to the limit is
	// the same check the Lua makes.
	allowed := int(count) <= effectiveMax
	remaining := effectiveMax - int(count)
	if remaining < 0 {
		remaining = 0
	}
	// RetryAfter is the REMAINING SECONDS IN THE CURRENT WINDOW, not the Redis key TTL (2x window
	// for draining), which would overstate the reset. Floor at 1s so the header is never zero
	// on a denial.
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

// clampCost normalises a caller-computed cost into [1, effectiveMax].
// See [Bucket.Charge] for why both ends are clamped rather than
// rejected.
func clampCost(cost, effectiveMax int) int {
	if cost < 1 {
		return 1
	}
	if cost > effectiveMax {
		return effectiveMax
	}
	return cost
}

// localCharge is the in-process fixed-window path taken when rdb was nil at construction. It
// mirrors the Redis path's Result derivation (Remaining clamped at zero; RetryAfter = seconds
// left in the window, floored at 1s on a denial) so callers cannot tell the backends apart.
// It never errors: the fallback exists to stay enforced when Redis is unavailable.
//
// The weight is spent as cost single-token takes. A loop is right here: each take is a map write
// under a mutex, cost is bounded by the effective limit, and the store's capacity/overflow
// routing and sweep stay in one implementation. The verdict is read off the LAST take, so a
// concurrent caller interleaving can only make it stricter, never looser.
func (b *Bucket) localCharge(key string, cost, effectiveMax int, minute int64) Result {
	now := b.nowFn()
	var (
		count   int
		allowed bool
	)
	for i := 0; i < cost; i++ {
		count, allowed = b.local.take(key, minute, effectiveMax, now, b.window)
	}

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
