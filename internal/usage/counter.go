// Package usage provides per-subject daily request counters
// backed by Redis. Increments fire alongside the rate-limit
// check on every authenticated request; reads aggregate the
// per-day INCRs into the wire shape /v1/account/usage emits.
//
// Storage shape: one Redis key per (subject, day):
//
//	usage:<sub>:<YYYY-MM-DD>  → INCR-counted request count
//
// Each key carries a 35-day TTL so the 30d-window read always
// has the floor data without needing manual cleanup. Subject
// strings are url-encoded by the caller before they reach this
// package — same convention as the rate-limit Bucket so the
// keys never collide on a `:` byte inside an IPv6 address or
// similar.
package usage

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// retentionDays bounds how far back Read can look. 35 covers the
// 30d billing window with a 5-day buffer for late writes / clock
// skew between regions.
const retentionDays = 35

// Counter atomically increments per-(subject, day) Redis counters
// and reads them back in date-bucketed order. Safe for concurrent
// use across goroutines; one process-level instance.
type Counter struct {
	rdb       redis.Cmdable
	keyPrefix string
	nowFn     func() time.Time
}

// Option configures a Counter at construction.
type Option func(*Counter)

// WithClock overrides the time source. Tests pass a fake clock to
// drive the day boundary deterministically.
func WithClock(now func() time.Time) Option {
	return func(c *Counter) { c.nowFn = now }
}

// WithKeyPrefix overrides the "usage:" key prefix. Tests use this
// to isolate against shared miniredis state.
func WithKeyPrefix(prefix string) Option {
	return func(c *Counter) { c.keyPrefix = prefix }
}

// New constructs a usage Counter. Pass the same redis.Cmdable
// the rate-limit Bucket uses — usage shares the bucket's Redis
// host since the keys never collide (different prefix).
func New(rdb redis.Cmdable, opts ...Option) *Counter {
	// F-1258 (codex audit-2026-05-12) — defence-in-depth. The
	// caller in cmd/stellaratlas-api/main.go now only constructs a
	// counter when Redis is wired, but if a future call site
	// passes nil here, return nil so [middleware.UsageTracker]'s
	// `counter == nil` short-circuit fires before any Redis op.
	if rdb == nil {
		return nil
	}
	c := &Counter{
		rdb:       rdb,
		keyPrefix: "usage:",
		nowFn:     time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Increment bumps the counter for (subject, today). Errors are
// returned but callers should treat usage tracking as best-effort
// — failing to increment must NEVER block a request. Hot-path
// callers typically `_ = counter.Increment(...)` and let the
// metric tell them if Redis is misbehaving.
func (c *Counter) Increment(ctx context.Context, subject string) error {
	if subject == "" {
		return nil
	}
	day := c.nowFn().UTC().Format("2006-01-02")
	key := c.keyPrefix + url.QueryEscape(subject) + ":" + day
	pipe := c.rdb.TxPipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, retentionDays*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage: incr %s: %w", key, err)
	}
	return nil
}

// Day is one row of the per-day usage rollup.
type Day struct {
	Date     string // YYYY-MM-DD UTC
	Requests int64
}

// MonthToDate returns the sum of `subject`'s per-day counters
// from the 1st of the current UTC month through (and including)
// today. Used by [middleware.MonthlyQuota] to enforce per-key
// monthly request ceilings. F-1226 (codex audit-2026-05-12).
//
// Empty subject or a Redis-side failure returns (0, err); the
// caller treats both as "fail open" — usage caps must never be
// the reason a request 500s. Returns (0, nil) for the first day
// of the month before any counter has been written.
func (c *Counter) MonthToDate(ctx context.Context, subject string) (int64, error) {
	if c == nil || subject == "" {
		return 0, nil
	}
	now := c.nowFn().UTC()
	year, month, today := now.Date()
	keys := make([]string, 0, today)
	for d := 1; d <= today; d++ {
		date := time.Date(year, month, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		keys = append(keys, c.keyPrefix+url.QueryEscape(subject)+":"+date)
	}
	raw, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("usage: month-to-date mget: %w", err)
	}
	var total int64
	for _, v := range raw {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		total += n
	}
	return total, nil
}

// Read returns up to `days` daily counts for the subject, oldest
// first. Days with no requests are omitted (the caller fills
// gaps with zero buckets if the wire contract requires it). days
// is clamped to retentionDays — beyond that the data has expired.
func (c *Counter) Read(ctx context.Context, subject string, days int) ([]Day, error) {
	if subject == "" || days <= 0 {
		return nil, nil
	}
	if days > retentionDays {
		days = retentionDays
	}
	now := c.nowFn().UTC()
	keys := make([]string, days)
	dates := make([]string, days)
	for i := 0; i < days; i++ {
		date := now.AddDate(0, 0, -(days - 1 - i))
		dateStr := date.Format("2006-01-02")
		dates[i] = dateStr
		keys[i] = c.keyPrefix + url.QueryEscape(subject) + ":" + dateStr
	}
	raw, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("usage: mget: %w", err)
	}
	out := make([]Day, 0, days)
	for i, v := range raw {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, Day{Date: dates[i], Requests: n})
	}
	return out, nil
}
