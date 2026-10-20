// Package usage provides per-subject request counters backed by Redis, plus the rollup worker that
// persists them into `usage_daily`. Increments come from middleware.UsageTracker.
//
// `subject` is the OWNER-ACCOUNT key from middleware.UsageKeyForSubject ("id:acct:<slug>";
// "key:<KeyID>" only when the store stamped no owner), so an account's credentials share one
// counter. Per-credential keying would let a customer multiply the monthly ceiling by minting keys.
//
// Two key families per (subject, day), with a 35-day TTL:
//
//	usage:<sub>:<YYYY-MM-DD>     → INCRBY billable request-unit total (the MonthlyQuota input)
//	usage:ep:<sub>:<YYYY-MM-DD>  → HASH of "<route-pattern>|<class>" (ok, 4xx, 429, 5xx) counters
//
// Subjects are url-encoded so a `:` in an IPv6 address can't collide or spoof "ep:".
package usage

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RetentionDays is the TTL of every Redis usage key and so how far back Read and the rollup can see; 35 covers the 30d billing window plus 5 days for late writes and clock skew.
const RetentionDays = 35

// Outcome classes for the per-endpoint detail counters. The middleware maps every status onto
// exactly one of four, so hash field cardinality is (#route patterns × 4) per subject per day.
const (
	// ClassOK — any status < 400 (2xx success, 3xx redirect/304).
	ClassOK = "ok"
	// ClassClientError — 4xx except 429.
	ClassClientError = "4xx"
	// ClassThrottled is a 429 rejection (rate-limit or monthly quota). With ClassServerError it
	// is kept out of the per-day total so `requests` stays billable traffic the caller caused:
	// an outage or rate-limit storm must not eat a paid allowance. The one billable 5xx is a
	// read that timed out on its own server-side budget (middleware.billableClass). Both still
	// count in full in the detail hash, and 429s land under "unmatched" because they reject
	// before route resolution.
	ClassThrottled = "429"
	// ClassServerError — 5xx.
	ClassServerError = "5xx"
)

// Counter atomically increments per-(subject, day) Redis counters and reads them back date-bucketed; safe for concurrent use, one process-level instance.
type Counter struct {
	rdb       redis.Cmdable
	keyPrefix string
	nowFn     func() time.Time
	durable   *durableDays
}

// Option configures a Counter at construction.
type Option func(*Counter)

// WithClock overrides the time source so tests can drive the day boundary.
func WithClock(now func() time.Time) Option {
	return func(c *Counter) { c.nowFn = now }
}

// WithKeyPrefix overrides the "usage:" key prefix to isolate tests on shared miniredis.
func WithKeyPrefix(prefix string) Option {
	return func(c *Counter) { c.keyPrefix = prefix }
}

// New constructs a usage Counter on the same redis.Cmdable as the rate-limit Bucket; the key prefixes never collide.
func New(rdb redis.Cmdable, opts ...Option) *Counter {
	// A nil rdb returns nil so [middleware.UsageTracker]'s `counter == nil` short-circuit fires
	// before any Redis op.
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

// Increment bumps (subject, today) by one unit; best-effort, a failed increment must NEVER block a request, so hot-path callers drop the error.
func (c *Counter) Increment(ctx context.Context, subject string) error {
	return c.IncrementBy(ctx, subject, 1)
}

// IncrementBy adds n product-cost units for (subject, today), e.g. one per priced asset on /v1/price/batch, so the quota meters lookups not HTTP calls; n <= 0 is a no-op.
func (c *Counter) IncrementBy(ctx context.Context, subject string, n int64) error {
	if c == nil || subject == "" || n <= 0 {
		return nil
	}
	day := c.nowFn().UTC().Format("2006-01-02")
	key := c.keyPrefix + url.QueryEscape(subject) + ":" + day
	pipe := c.rdb.TxPipeline()
	pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, RetentionDays*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage: incr %s: %w", key, err)
	}
	return nil
}

// detailKeyInfix separates per-day totals from per-endpoint detail hashes. Subjects are
// QueryEscape'd (':' → "%3A"), so none can collide with the literal "ep" segment.
const detailKeyInfix = "ep:"

// detailKey is the per-(subject, day) detail HASH key; fields are "<endpoint>|<class>".
func (c *Counter) detailKey(subject, day string) string {
	return c.keyPrefix + detailKeyInfix + url.QueryEscape(subject) + ":" + day
}

// IncrementDetail bumps the per-endpoint outcome counter; endpoint MUST be a bounded-cardinality route pattern, never a raw URL path, and class one of the Class* constants; best-effort like [Counter.Increment].
func (c *Counter) IncrementDetail(ctx context.Context, subject, endpoint, class string) error {
	return c.IncrementDetailBy(ctx, subject, endpoint, class, 1)
}

// IncrementDetailBy adds n units weighted as [Counter.IncrementBy] weights the billable total, so the rollup's billable sum equals the quota counter.
func (c *Counter) IncrementDetailBy(ctx context.Context, subject, endpoint, class string, n int64) error {
	if c == nil || subject == "" || endpoint == "" || class == "" || n <= 0 {
		return nil
	}
	day := c.nowFn().UTC().Format("2006-01-02")
	key := c.detailKey(subject, day)
	pipe := c.rdb.TxPipeline()
	pipe.HIncrBy(ctx, key, endpoint+"|"+class, n)
	pipe.Expire(ctx, key, RetentionDays*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage: hincrby %s: %w", key, err)
	}
	return nil
}

// DetailRow is one (subject, day, endpoint, class) counter read from a detail hash; the rollup groups these into [RollupRow]s for the Timescale sink.
type DetailRow struct {
	Date     string // YYYY-MM-DD UTC
	Subject  string // decoded subject ("id:<ident>" / "key:<id>")
	Endpoint string // route pattern
	Class    string // one of the Class* constants
	Count    int64
}

// ScanDetail SCANs every detail hash for dates (YYYY-MM-DD) into one slice, de-duplicated per date because SCAN is at-least-once and a duplicate row would be summed and made permanent by usage_daily's GREATEST() merge; production sweeps should use [Counter.ScanDetailFunc].
func (c *Counter) ScanDetail(ctx context.Context, dates []string) ([]DetailRow, error) {
	if c == nil {
		return nil, nil
	}
	var out []DetailRow
	err := c.ScanDetailFunc(ctx, dates, func(row DetailRow) error {
		out = append(out, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ScanDetailFunc calls fn once per decoded, deduplicated (as in [ScanDetail]) row without buffering a whole day; fn's error aborts the walk.
func (c *Counter) ScanDetailFunc(ctx context.Context, dates []string, fn func(DetailRow) error) error {
	if c == nil {
		return nil
	}
	for _, date := range dates {
		if err := c.scanDetailDateFunc(ctx, date, fn); err != nil {
			return err
		}
	}
	return nil
}

// scanDetailDateFunc walks one date's detail hashes, de-duplicating SCAN repeats.
func (c *Counter) scanDetailDateFunc(ctx context.Context, date string, fn func(DetailRow) error) error {
	match := c.keyPrefix + detailKeyInfix + "*:" + date
	seen := make(map[string]struct{})
	var cursor uint64
	for {
		keys, next, err := c.rdb.Scan(ctx, cursor, match, 200).Result()
		if err != nil {
			return fmt.Errorf("usage: scan %s: %w", match, err)
		}
		for _, key := range keys {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			rows, err := c.readDetailHash(ctx, key, date)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if err := fn(row); err != nil {
					return err
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// readDetailHash decodes one detail hash into DetailRows. Malformed fields are skipped: the hash
// is single-writer ([Counter.IncrementDetail]), so damage means operator surgery, not a reason
// to fail the sweep.
func (c *Counter) readDetailHash(ctx context.Context, key, date string) ([]DetailRow, error) {
	subject, ok := c.subjectFromDetailKey(key, date)
	if !ok {
		return nil, nil
	}
	fields, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("usage: hgetall %s: %w", key, err)
	}
	rows := make([]DetailRow, 0, len(fields))
	for field, raw := range fields {
		sep := strings.LastIndexByte(field, '|')
		if sep <= 0 || sep == len(field)-1 {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		rows = append(rows, DetailRow{
			Date:     date,
			Subject:  subject,
			Endpoint: field[:sep],
			Class:    field[sep+1:],
			Count:    n,
		})
	}
	return rows, nil
}

// subjectFromDetailKey recovers the subject from "<prefix>ep:<escaped-subject>:<date>".
func (c *Counter) subjectFromDetailKey(key, date string) (string, bool) {
	prefix := c.keyPrefix + detailKeyInfix
	suffix := ":" + date
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return "", false
	}
	escaped := key[len(prefix) : len(key)-len(suffix)]
	subject, err := url.QueryUnescape(escaped)
	if err != nil || subject == "" {
		return "", false
	}
	return subject, true
}

// Day is one row of the per-day usage rollup.
type Day struct {
	Date     string // YYYY-MM-DD UTC
	Requests int64
}

// MonthToDate sums subject's (owner-account key) per-day counters from the 1st of the UTC month; with [WithDurableDays] each day counts max(Redis, usage_daily), both lower bounds so it never over-bills; a read failure returns (0, err) and the caller fails open in its dwell window, closed after.
func (c *Counter) MonthToDate(ctx context.Context, subject string) (int64, error) {
	if c == nil || subject == "" {
		return 0, nil
	}
	now := c.nowFn().UTC()
	year, month, today := now.Date()
	dates := make([]string, 0, today)
	keys := make([]string, 0, today)
	for d := 1; d <= today; d++ {
		date := time.Date(year, month, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		dates = append(dates, date)
		keys = append(keys, c.keyPrefix+url.QueryEscape(subject)+":"+date)
	}
	raw, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("usage: month-to-date mget: %w", err)
	}
	var durable map[string]int64
	if c.durable != nil {
		durable, err = c.durable.get(ctx, subject, dates[0], dates[len(dates)-1], now)
		if err != nil {
			return 0, fmt.Errorf("usage: month-to-date durable read: %w", err)
		}
	}
	var total int64
	for i, v := range raw {
		total += max(parseCount(v), durable[dates[i]])
	}
	return total, nil
}

// parseCount decodes one MGET slot; a missing or malformed value is 0.
func parseCount(v any) int64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Read returns up to days daily counts, oldest first, omitting empty days; days is clamped to RetentionDays, past which keys have expired.
func (c *Counter) Read(ctx context.Context, subject string, days int) ([]Day, error) {
	if c == nil || subject == "" || days <= 0 {
		return nil, nil
	}
	if days > RetentionDays {
		days = RetentionDays
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

// DeleteSubject deletes subject's total and detail keys for every day in the retention window and returns how many existed; used for account erasure, keys are derived not scanned.
func (c *Counter) DeleteSubject(ctx context.Context, subject string) (int64, error) {
	if c == nil || subject == "" {
		return 0, nil
	}
	today := c.nowFn().UTC()
	keys := make([]string, 0, 2*(RetentionDays+1))
	for i := 0; i <= RetentionDays; i++ {
		day := today.AddDate(0, 0, -i).Format("2006-01-02")
		keys = append(keys, c.keyPrefix+url.QueryEscape(subject)+":"+day, c.detailKey(subject, day))
	}
	n, err := c.rdb.Del(ctx, keys...).Result()
	if err != nil {
		return 0, fmt.Errorf("usage: delete subject: %w", err)
	}
	return n, nil
}
