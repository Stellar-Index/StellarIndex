package usage

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// DailyBillableReader is the durable side of the month-to-date meter:
// the per-day billable units (ok + 4xx) [Rollup] persisted to
// `usage_daily`. Production wiring is *timescale.Store.
type DailyBillableReader interface {
	// BillableByDay returns subject's billable units per UTC day in
	// [from, to] (both YYYY-MM-DD, inclusive), keyed by day. Days with
	// no row are absent.
	BillableByDay(ctx context.Context, subject, from, to string) (map[string]int64, error)
}

// WithDurableDays makes [Counter.MonthToDate] reconcile every day of
// the month against r, so an evicted or flushed Redis day key cannot
// read as a quiet day. A nil r leaves the counter Redis-only.
func WithDurableDays(r DailyBillableReader) Option {
	return func(c *Counter) {
		if r != nil {
			c.durable = &durableDays{reader: r, entries: make(map[string]durableEntry)}
		}
	}
}

// durableCacheTTL is how long one subject's durable read is reused.
// usage_daily moves at most once per rollup sweep, so a fresher read
// buys nothing, and MonthToDate runs on every metered request.
const durableCacheTTL = DefaultRollupInterval

// durableCacheMaxSubjects bounds the per-process cache.
const durableCacheMaxSubjects = 100_000

// durableDays caches [DailyBillableReader] reads per subject.
type durableDays struct {
	reader  DailyBillableReader
	group   singleflight.Group
	mu      sync.Mutex
	entries map[string]durableEntry
}

type durableEntry struct {
	from, to string
	fetched  time.Time
	days     map[string]int64
}

// get returns subject's durable per-day units for [from, to]. On a
// read failure it falls back to an earlier read of the same month:
// usage_daily only grows, so any earlier snapshot is still a lower
// bound. With no such snapshot the error is returned, so the caller's
// fail-closed dwell sees an unreadable meter rather than a low one.
func (d *durableDays) get(ctx context.Context, subject, from, to string, now time.Time) (map[string]int64, error) {
	d.mu.Lock()
	cached, ok := d.entries[subject]
	d.mu.Unlock()
	if ok && cached.from == from && cached.to == to && now.Sub(cached.fetched) < durableCacheTTL {
		return cached.days, nil
	}
	v, err, _ := d.group.Do(subject+"\x00"+from+"\x00"+to, func() (any, error) {
		return d.reader.BillableByDay(ctx, subject, from, to)
	})
	if err != nil {
		if ok && cached.from == from {
			// Re-stamped so a Postgres outage costs one retry per TTL,
			// not one per request.
			d.put(subject, durableEntry{from: from, to: to, fetched: now, days: cached.days}, now)
			return cached.days, nil
		}
		return nil, err
	}
	days, _ := v.(map[string]int64)
	d.put(subject, durableEntry{from: from, to: to, fetched: now, days: days}, now)
	return days, nil
}

func (d *durableDays) put(subject string, e durableEntry, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) >= durableCacheMaxSubjects {
		for k, old := range d.entries {
			if now.Sub(old.fetched) >= durableCacheTTL {
				delete(d.entries, k)
			}
		}
		if len(d.entries) >= durableCacheMaxSubjects {
			clear(d.entries)
		}
	}
	d.entries[subject] = e
}
