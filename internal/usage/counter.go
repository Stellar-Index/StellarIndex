// Package usage provides per-subject request counters backed by
// Redis, plus the rollup worker that persists them into the
// `usage_daily` Timescale hypertable. Increments fire from
// middleware.UsageTracker on every authenticated request; reads
// back the wire shapes /v1/account/usage emits.
//
// `subject` is the OWNER-ACCOUNT key middleware.UsageKeyForSubject
// derives ("id:acct:<slug>"; "key:<KeyID>" only for a credential whose
// store stamped no owner reference), so every credential an account
// holds shares one counter — the monthly ceiling is a plan budget, and
// keying it per credential let a customer multiply it by minting keys
// and reset it by rotating one (RLT-404).
//
// Storage shape — two key families per (subject, day):
//
//	usage:<sub>:<YYYY-MM-DD>     → INCRBY-counted request-unit total
//	                               (BILLABLE traffic only; the
//	                               MonthlyQuota input — excludes
//	                               429 and 5xx other than a timed-
//	                               out read, see ClassThrottled)
//	usage:ep:<sub>:<YYYY-MM-DD>  → HASH of per-endpoint outcome
//	                               counters; fields are
//	                               "<route-pattern>|<class>" with
//	                               class ∈ {ok, 4xx, 429, 5xx}
//
// Each key carries a 35-day TTL so the 30d-window read always
// has the floor data without needing manual cleanup; [Rollup]
// folds the detail hashes into Timescale for durable per-endpoint
// history beyond the TTL. Subject strings are url-encoded before
// they reach Redis — same convention as the rate-limit Bucket so
// the keys never collide on a `:` byte inside an IPv6 address or
// similar (and so the literal "ep:" infix can't be spoofed by a
// subject).
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

// retentionDays bounds how far back Read can look. 35 covers the
// 30d billing window with a 5-day buffer for late writes / clock
// skew between regions.
const retentionDays = 35

// Outcome classes for the per-endpoint detail counters. Bounded by
// construction — the middleware maps every response status onto
// exactly one of these four, so the Redis-hash field cardinality is
// (#route patterns × 4) per subject per day.
const (
	// ClassOK — any status < 400 (2xx success, 3xx redirect/304).
	ClassOK = "ok"
	// ClassClientError — 4xx except 429.
	ClassClientError = "4xx"
	// ClassThrottled — 429 rejections (rate-limit and monthly
	// quota). Kept out of the legacy per-day total, along with
	// ClassServerError, so `requests` keeps meaning BILLABLE
	// traffic — outcomes the CALLER caused (MonthlyQuota reads the
	// legacy keys). A 429 is our throttle firing and a 5xx is our
	// failure; charging either against a paid monthly cap means an
	// outage or a rate-limit storm eats the customer's allowance
	// (audit COR-05). The one billable 5xx is a read that timed out
	// on its server-side budget, which the request itself spent (see
	// middleware.billableClass). Both are still counted in full in the
	// per-endpoint detail hash, so neither becomes invisible — only
	// unbillable. Caveat on the ENDPOINT dimension: both 429
	// producers reject before the router resolves a route pattern,
	// so throttled counts land under the "unmatched" endpoint
	// rather than the caller's target route (cold audit
	// 2026-08-03; documented on /v1/account/usage in the spec).
	ClassThrottled = "429"
	// ClassServerError — 5xx.
	ClassServerError = "5xx"
)

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
	// caller in cmd/stellarindex-api/main.go now only constructs a
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

// Increment bumps the counter for (subject, today) by one unit. Errors
// are returned but callers should treat usage tracking as best-effort
// — failing to increment must NEVER block a request. Hot-path
// callers typically `_ = counter.Increment(...)` and let the
// metric tell them if Redis is misbehaving.
func (c *Counter) Increment(ctx context.Context, subject string) error {
	return c.IncrementBy(ctx, subject, 1)
}

// IncrementBy adds n request units to the counter for (subject, today).
// A request's unit count is its product cost — one per priced asset on
// /v1/price/batch — so the monthly quota meters lookups, not HTTP calls.
// n <= 0 records nothing.
func (c *Counter) IncrementBy(ctx context.Context, subject string, n int64) error {
	if c == nil || subject == "" || n <= 0 {
		return nil
	}
	day := c.nowFn().UTC().Format("2006-01-02")
	key := c.keyPrefix + url.QueryEscape(subject) + ":" + day
	pipe := c.rdb.TxPipeline()
	pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, retentionDays*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage: incr %s: %w", key, err)
	}
	return nil
}

// detailKeyInfix separates the legacy per-day totals ("usage:<sub>:
// <day>") from the per-endpoint detail hashes ("usage:ep:<sub>:
// <day>"). Subjects are QueryEscape'd before keying (':' → "%3A"),
// so no subject can collide with the literal "ep" segment.
const detailKeyInfix = "ep:"

// detailKey builds the Redis key for the per-(subject, day) detail
// hash. One HASH per (subject, day); fields are "<endpoint>|<class>".
func (c *Counter) detailKey(subject, day string) string {
	return c.keyPrefix + detailKeyInfix + url.QueryEscape(subject) + ":" + day
}

// IncrementDetail bumps the per-endpoint outcome counter for
// (subject, today). endpoint MUST be a bounded-cardinality route
// pattern (e.g. "/v1/assets/{asset_id}"), never a raw URL path;
// class is one of the Class* constants. Best-effort like
// [Counter.Increment] — callers drop the error after logging.
func (c *Counter) IncrementDetail(ctx context.Context, subject, endpoint, class string) error {
	return c.IncrementDetailBy(ctx, subject, endpoint, class, 1)
}

// IncrementDetailBy adds n request units to the per-endpoint outcome
// counter, weighted exactly as [Counter.IncrementBy] weights the
// billable total so the rollup's billable sum equals the quota counter.
func (c *Counter) IncrementDetailBy(ctx context.Context, subject, endpoint, class string, n int64) error {
	if c == nil || subject == "" || endpoint == "" || class == "" || n <= 0 {
		return nil
	}
	day := c.nowFn().UTC().Format("2006-01-02")
	key := c.detailKey(subject, day)
	pipe := c.rdb.TxPipeline()
	pipe.HIncrBy(ctx, key, endpoint+"|"+class, n)
	pipe.Expire(ctx, key, retentionDays*24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("usage: hincrby %s: %w", key, err)
	}
	return nil
}

// DetailRow is one (subject, day, endpoint, class) counter read back
// from a detail hash. The rollup worker groups these into
// [RollupRow]s before handing them to the Timescale sink.
type DetailRow struct {
	Date     string // YYYY-MM-DD UTC
	Subject  string // decoded subject ("id:<ident>" / "key:<id>")
	Endpoint string // route pattern
	Class    string // one of the Class* constants
	Count    int64
}

// ScanDetail walks every per-endpoint detail hash for the given
// dates (YYYY-MM-DD) and returns the decoded counters, materialized
// into one slice. SCAN-based so it never blocks Redis; the key
// population is bounded by (#active subjects × len(dates)). Kept for
// callers that want the whole batch (tests, one-off ops reads); a
// production sweep over the live subject population should use
// [Counter.ScanDetailFunc] instead so it never buffers a whole day's
// rows in memory (GH-1282).
//
// Keys are de-duplicated per date. Redis SCAN guarantees only that
// every key present for the whole iteration is returned AT LEAST once
// — a rehash mid-cursor can return the same key again, and this Redis
// is shared with the rate limiter's high-churn per-minute keys, so
// rehashes are routine. Without the seen-set, one duplicate re-read
// the same hash into a second DetailRow, the rollup's groupDetails
// SUMMED both, and usage_daily's GREATEST() merge made the inflated
// count PERMANENT for a closed day (no later sweep can produce a
// larger true value to correct it) — a customer's usage history
// doubled by a Redis implementation detail (cold audit 2026-08-03).
// A repeat key is always a re-read of the same (subject, day) hash,
// never distinct data, so keeping the first read is exact.
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

// ScanDetailFunc walks every per-endpoint detail hash for the given
// dates and invokes fn once per decoded row, in the same
// deduplicated order [ScanDetail] would return them. A caller that
// aggregates as it goes (e.g. [Rollup.Sweep]'s grouping) never holds
// a whole day's rows in memory at once — only the current SCAN page
// (<=200 keys) and the hash currently being decoded. fn's error
// aborts the walk.
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

// scanDetailDateFunc walks one date's detail hashes. Split out of
// ScanDetailFunc so the cursor walk, the de-duplication and the
// per-date loop each stay individually readable (and under the
// gocognit ceiling).
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

// readDetailHash HGETALLs one detail hash and decodes its fields
// into DetailRows. Malformed fields / values are skipped — the hash
// is single-writer ([Counter.IncrementDetail]) so damage here means
// operator surgery, not a code path worth failing the sweep over.
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

// subjectFromDetailKey recovers the url-decoded subject from a
// detail hash key of the form "<prefix>ep:<escaped-subject>:<date>".
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

// MonthToDate returns the sum of `subject`'s per-day counters
// from the 1st of the current UTC month through (and including)
// today. Used by [middleware.MonthlyQuota] to enforce monthly
// request ceilings; `subject` is the owner-account key (see the
// package doc), so the sum spans every credential the account
// holds. F-1226 (codex audit-2026-05-12).
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
