package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// Compiled defaults for [RedisSignupResendThrottle]: each accepted
// resend is one outbound transactional email, so both caps are tight.
const (
	signupResendMaxPerAddress = 3
	signupResendMaxPerIP      = 10
	signupResendWindow        = time.Hour
)

// RedisSignupResendThrottle caps verification-mail resends per address
// and per IP. Both counters are spent before the address is looked up,
// so a known and an unknown address burn the same budget and the 429
// reveals nothing about which addresses have an account.
type RedisSignupResendThrottle struct {
	counter *ratelimit.FixedWindowCounter
}

// NewRedisSignupResendThrottle constructs the throttle. rdb MUST be non-nil.
func NewRedisSignupResendThrottle(rdb redis.Cmdable) *RedisSignupResendThrottle {
	if rdb == nil {
		panic("auth: NewRedisSignupResendThrottle: rdb must not be nil")
	}
	return &RedisSignupResendThrottle{
		counter: ratelimit.NewFixedWindowCounter(rdb, signupResendWindow, nil),
	}
}

// Allow returns nil while both budgets have room and
// [ErrSignupRateLimited] once either is spent. A Redis error is returned
// as-is (not the sentinel): a resend sends mail, so the caller fails
// closed rather than lifting the cap during an outage.
func (t *RedisSignupResendThrottle) Allow(ctx context.Context, ip, emailHash string) error {
	// Both increments always run: short-circuiting would let a request
	// over one cap skip spending the other.
	byAddr, errAddr := t.counter.Incr(ctx, "signup-ip:resend-addr:"+emailHash)
	var byIP int64
	var errIP error
	if ip != "" {
		byIP, errIP = t.counter.Incr(ctx, "signup-ip:resend-ip:"+ratelimit.ThrottleIPKey(ip))
	}
	if errAddr != nil || errIP != nil {
		return fmt.Errorf("signup resend throttle: %w", firstErr(errAddr, errIP))
	}
	if byAddr > signupResendMaxPerAddress || byIP > signupResendMaxPerIP {
		return ErrSignupRateLimited
	}
	return nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
