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

// DefaultDwellTime is how long Take may fail open on Redis errors before flipping to fail-CLOSED via [ErrThrottleUnavailable], so a blip is ridden out but a sustained outage is not an unbounded-volume bypass; matches [auth.DefaultSignupThrottleDwellTime].
const DefaultDwellTime = 30 * time.Second

// ErrThrottleUnavailable signals Redis has failed longer than the dwell-time and the caller should fail CLOSED (503 + Retry-After); [Bucket.Take] and [Bucket.Charge] return it in place of the wrapped error, which within the window keeps the fail-open branch.
var ErrThrottleUnavailable = errors.New("ratelimit: throttle layer unavailable (sustained backend errors)")

// Bucket is a per-(client x window) counter, safe for concurrent use (one shared instance); Take returns a wrapped Redis error inside the dwell window (callers fail open), then [ErrThrottleUnavailable] (fail CLOSED), and only sustained recovery resets the clock, not a stray success (see healthySince).
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
	// healthySince starts the CURRENT unbroken run of successes; any failure resets it.
	// redisErrorSince clears only after a full dwellTime streak, or a flapping Redis would leave
	// the throttles fail-open while effectively down.
	healthySince time.Time
	// lastFailure lets a success that follows a failure-free dwellTime clear
	// the clock, so a long-ago outage cannot make the next blip fail closed.
	lastFailure time.Time
}

// Option configures a Bucket at construction.
type Option func(*Bucket)

// WithClock overrides the window-bucket time source, for tests using miniredis's FastForward.
func WithClock(now func() time.Time) Option {
	return func(b *Bucket) { b.nowFn = now }
}

// WithKeyPrefix overrides the "rl:" key prefix, for test isolation or a Redis shared with a non-stellarindex caller.
func WithKeyPrefix(prefix string) Option {
	return func(b *Bucket) { b.keyPrefix = prefix }
}

// WithDwellTime overrides how long Redis may keep failing before Take stops failing open and returns [ErrThrottleUnavailable] (api.rate_limit_dwell); a negative value disables the inversion so every Redis error fails open. Default [DefaultDwellTime].
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

// New constructs a limiter allowing limit requests per fixed window; a nil rdb selects the in-process limiter (see [localStore]), still ENFORCED but per-process, and it panics on a non-positive limit or sub-second window.
func New(rdb redis.Cmdable, limit int, window time.Duration, opts ...Option) *Bucket {
	if limit <= 0 {
		panic("ratelimit: max must be positive")
	}
	// Sub-second windows would divide by zero in the bucket-key derivation; fail at construction,
	// not on the first request.
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

// InProcess reports whether the Bucket runs on the in-process fallback (rdb was nil) rather than Redis, so the binary can log the enforcing backend.
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

// observeRedisSuccess advances the recovery streak. The fail-closed clock clears only after
// dwellTime of UNBROKEN successes, so a flapping Redis trips fail-CLOSED rather than failing open
// forever. A success after dwellTime with no failure also clears it, so an old outage cannot make
// an unrelated blip fail closed.
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

// Max returns the configured per-window limit, for surfacing in response headers.
func (b *Bucket) Max() int { return b.max }

// Window returns the configured window size.
func (b *Bucket) Window() time.Duration { return b.window }

// lua is the atomic rate-limit check. KEYS[1] is the key; ARGV is TTL seconds (window * 2), max,
// and cost (>= 1). It returns [count, retry_after_seconds], with retry_after 0 when allowed.
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

// luaScript is compiled once: go-redis's Script caches the SHA inside the value, so re-creating it
// per call would waste a SHA-1 and allocation on every check.
var luaScript = redis.NewScript(lua)

// Take increments key's counter in the current window and reports whether the request is allowed, in one Redis round-trip; callers MUST branch on [ErrThrottleUnavailable] (fail CLOSED, 503 + Retry-After) versus a wrapped Redis error (fail OPEN), since treating all errors as fail-open re-opens the sustained-outage bypass.
func (b *Bucket) Take(ctx context.Context, key string) (Result, error) {
	return b.Charge(ctx, key, 1, 0)
}

// Charge spends cost tokens against key in ONE atomic step (never weight by looping Take; price must track work bought); limit > 0 replaces max for this call and MUST be stable per key; cost is clamped to [1, limit] (a negative INCRBY would REFUND); a denied charge is still counted.
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

	// Escape the key before appending ":<minute>": IPv6 addresses contain `:`, which would let
	// distinct keys collide and smear limits across clients. It also blocks control-byte injection.
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
		// A caller abort says nothing about Redis health, so it neither arms the dwell clock nor
		// breaks a recovery streak. A deadline still counts: too slow to answer is failing.
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

	// `count`, not retryTTL, is the allow-signal: the Lua clamps a negative TTL to 0 on the deny
	// path, so a key lazily evicted between INCR and TTL would flip a deny into an allow.
	allowed := int(count) <= effectiveMax
	remaining := effectiveMax - int(count)
	if remaining < 0 {
		remaining = 0
	}
	// RetryAfter is the remaining seconds in the current window, not the key TTL (2x window),
	// which would overstate the reset; floored at 1s so a denial never carries zero.
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

// localCharge is the in-process fixed-window path taken when rdb was nil. It mirrors the Redis
// path's Result derivation so callers cannot tell the backends apart, and never errors: the
// fallback exists to stay enforced when Redis is unavailable.
//
// The weight is spent as cost single-token takes: each is a map write under a mutex, cost is
// bounded by the effective limit, and capacity/overflow routing stays in one implementation. The
// verdict is read off the LAST take, so interleaving can only make it stricter, never looser.
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
