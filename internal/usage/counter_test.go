package usage_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// TestIncrementAndRead_RoundTrip — Increment + Read recover the
// counts they wrote. The happy path.
// TestMonthToDate_SumsCurrentMonthOnly — only counters dated
// within the current UTC calendar month are summed. F-1226
// (codex audit-2026-05-12).
func TestMonthToDate_SumsCurrentMonthOnly(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) // 1st of month
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	// April 30 — previous month, must NOT be counted.
	clock = time.Date(2026, 4, 30, 23, 0, 0, 0, time.UTC)
	_ = c.Increment(ctx, "subj-mtd")
	_ = c.Increment(ctx, "subj-mtd")

	// May 1.
	clock = time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	_ = c.Increment(ctx, "subj-mtd")
	_ = c.Increment(ctx, "subj-mtd")
	_ = c.Increment(ctx, "subj-mtd")

	// May 3.
	clock = time.Date(2026, 5, 3, 8, 0, 0, 0, time.UTC)
	_ = c.Increment(ctx, "subj-mtd")
	_ = c.Increment(ctx, "subj-mtd")

	// Read MTD as-of May 3.
	got, err := c.MonthToDate(ctx, "subj-mtd")
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 5 {
		t.Errorf("MonthToDate = %d, want 5 (3 from May 1 + 2 from May 3; April's 2 must be excluded)", got)
	}
}

// TestMonthToDate_EmptySubjectIsZero — empty subject returns 0
// without touching Redis (matches Increment's no-op behaviour).
func TestMonthToDate_EmptySubjectIsZero(t *testing.T) {
	_, rdb := newRedis(t)
	c := usage.New(rdb)
	got, err := c.MonthToDate(context.Background(), "")
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 0 {
		t.Errorf("MonthToDate = %d, want 0", got)
	}
}

// TestMonthToDate_NoActivity — a subject with zero days
// written returns 0, no error.
func TestMonthToDate_NoActivity(t *testing.T) {
	_, rdb := newRedis(t)
	c := usage.New(rdb)
	got, err := c.MonthToDate(context.Background(), "subj-quiet")
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 0 {
		t.Errorf("MonthToDate = %d, want 0 (no activity)", got)
	}
}

func TestIncrementAndRead_RoundTrip(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := c.Increment(ctx, "subj-1"); err != nil {
			t.Fatalf("Increment %d: %v", i, err)
		}
	}

	days, err := c.Read(ctx, "subj-1", 7)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("Read returned %d rows, want 1 (only today has counts)", len(days))
	}
	if days[0].Date != "2026-05-12" {
		t.Errorf("Date = %q, want 2026-05-12", days[0].Date)
	}
	if days[0].Requests != 3 {
		t.Errorf("Requests = %d, want 3", days[0].Requests)
	}
}

// TestRead_DayBoundary — counts for different days appear in
// distinct rows, oldest first. Advances the clock between
// increments to exercise the day-key derivation.
func TestRead_DayBoundary(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))

	ctx := context.Background()
	_ = c.Increment(ctx, "subj-2") // 2026-05-10
	clock = clock.AddDate(0, 0, 1)
	_ = c.Increment(ctx, "subj-2") // 2026-05-11
	_ = c.Increment(ctx, "subj-2") // 2026-05-11
	clock = clock.AddDate(0, 0, 1)
	_ = c.Increment(ctx, "subj-2") // 2026-05-12

	days, err := c.Read(ctx, "subj-2", 7)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(days) != 3 {
		t.Fatalf("expected 3 rows, got %d: %+v", len(days), days)
	}
	want := []usage.Day{
		{Date: "2026-05-10", Requests: 1},
		{Date: "2026-05-11", Requests: 2},
		{Date: "2026-05-12", Requests: 1},
	}
	for i, w := range want {
		if days[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, days[i], w)
		}
	}
}

// TestIncrement_EmptySubjectIsNoOp — passing an empty subject is
// a no-op. The handler short-circuits before calling Increment
// when the subject is anonymous; this guard catches a regression
// where the URL-encoded empty string makes it past the check.
func TestIncrement_EmptySubjectIsNoOp(t *testing.T) {
	mr, rdb := newRedis(t)
	c := usage.New(rdb)
	if err := c.Increment(context.Background(), ""); err != nil {
		t.Fatalf("Increment(empty subject): %v", err)
	}
	if got := len(mr.Keys()); got != 0 {
		t.Errorf("empty-subject increment wrote %d keys; want 0", got)
	}
}

// TestRead_EmptySubjectReturnsNil — symmetric guard for Read.
func TestRead_EmptySubjectReturnsNil(t *testing.T) {
	_, rdb := newRedis(t)
	c := usage.New(rdb)
	rows, err := c.Read(context.Background(), "", 7)
	if err != nil {
		t.Errorf("Read(empty subject): %v", err)
	}
	if rows != nil {
		t.Errorf("rows = %v, want nil", rows)
	}
}

// TestRead_DaysClampedToRetention — asking for more days than the
// retention window returns at most RetentionDays rows. Clamp is
// load-bearing because the caller's `days` parameter is wire-
// derived (from the API `?days=` query) and we don't want a
// caller demanding 365 days to issue 365 MGETs.
func TestRead_DaysClampedToRetention(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))

	// Populate one count for today so we see SOMETHING in the
	// result; the clamp is what we're testing.
	_ = c.Increment(context.Background(), "subj-3")

	rows, err := c.Read(context.Background(), "subj-3", 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// At most 1 day has data; result should be 1 row regardless
	// of the requested days value (clamp affects the iteration
	// upper bound, not the row count).
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}
}

// TestKeyPrefix_Isolation — two Counters with different prefixes
// don't see each other's keys. Lets the test suite share one
// miniredis without collisions, AND lets a future operator run
// two usage counters against one Redis without collision.
func TestKeyPrefix_Isolation(t *testing.T) {
	_, rdb := newRedis(t)
	a := usage.New(rdb, usage.WithKeyPrefix("usage-a:"))
	b := usage.New(rdb, usage.WithKeyPrefix("usage-b:"))

	ctx := context.Background()
	_ = a.Increment(ctx, "same-subject")
	_ = a.Increment(ctx, "same-subject")
	_ = b.Increment(ctx, "same-subject")

	rowsA, _ := a.Read(ctx, "same-subject", 1)
	rowsB, _ := b.Read(ctx, "same-subject", 1)
	if len(rowsA) != 1 || rowsA[0].Requests != 2 {
		t.Errorf("a sees %+v, want 2 requests", rowsA)
	}
	if len(rowsB) != 1 || rowsB[0].Requests != 1 {
		t.Errorf("b sees %+v, want 1 request", rowsB)
	}
}

// TestSubject_URLEncoded — subjects with colon bytes (e.g. IPv6
// addresses, "subject:bucket" composites) don't collide on the
// day-suffix separator. The package URL-encodes the subject
// before joining; verify the key shape directly.
func TestSubject_URLEncoded(t *testing.T) {
	mr, rdb := newRedis(t)
	clock := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	subj := "fe80::1234:5678"
	_ = c.Increment(context.Background(), subj)

	wantKey := "usage:" + url.QueryEscape(subj) + ":2026-05-12"
	keys := mr.Keys()
	found := false
	for _, k := range keys {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("subject not URL-encoded into key; got keys = %v, want one matching %q",
			keys, wantKey)
	}
}

// TestRead_RetentionTTLApplied — the Increment path sets a 35-day
// TTL on every key so old data drops off without manual cleanup.
// This is the load-bearing reason for the retention cap on Read.
func TestRead_RetentionTTLApplied(t *testing.T) {
	mr, rdb := newRedis(t)
	clock := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	_ = c.Increment(context.Background(), "subj-ttl")

	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("no key created")
	}
	ttl := mr.TTL(keys[0])
	if ttl == 0 {
		t.Errorf("TTL = 0, want > 0 (retention cap)")
	}
	if ttl > 36*24*time.Hour {
		t.Errorf("TTL = %v, want ≤ 36 days", ttl)
	}
}

// fakeUsageDaily is an in-memory usage_daily: GREATEST-merged rollup
// rows in, billable (ok + 4xx) units per day out, as the SQL does.
type fakeUsageDaily struct {
	mu    sync.Mutex
	rows  map[string]usage.RollupRow
	err   error
	reads int
}

func (f *fakeUsageDaily) UpsertUsageDaily(_ context.Context, rows []usage.RollupRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = make(map[string]usage.RollupRow)
	}
	for _, r := range rows {
		k := r.Day + "|" + r.Subject + "|" + r.Endpoint
		cur := f.rows[k]
		r.OK = max(r.OK, cur.OK)
		r.ClientErrors = max(r.ClientErrors, cur.ClientErrors)
		r.ServerErrors = max(r.ServerErrors, cur.ServerErrors)
		r.Throttled = max(r.Throttled, cur.Throttled)
		f.rows[k] = r
	}
	return nil
}

func (f *fakeUsageDaily) BillableByDay(_ context.Context, subject, from, to string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int64)
	for _, r := range f.rows {
		if r.Subject == subject && r.Day >= from && r.Day <= to {
			out[r.Day] += r.OK + r.ClientErrors
		}
	}
	return out, nil
}

func dayKey(subject, day string) string {
	return "usage:" + url.QueryEscape(subject) + ":" + day
}

// TestMonthToDate_EvictedDayKeysReconcileFromUsageDaily pins GH-1274:
// the day keys live on an evicting Redis and a missing key read as a
// quiet day, so evicting the month's cold early days silently cut the
// billed month from 950 to 250. The rollup has already persisted those
// days, so the meter must still read 950.
func TestMonthToDate_EvictedDayKeysReconcileFromUsageDaily(t *testing.T) {
	mr, rdb := newRedis(t)
	clock := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	durable := &fakeUsageDaily{}
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }), usage.WithDurableDays(durable))
	ctx := context.Background()
	const subj = "id:acct:acme"

	// Billable traffic as middleware.UsageTracker records it: the quota
	// total and the detail hash, weighted identically.
	for day, n := range map[int]int64{1: 400, 2: 300, 20: 250} {
		clock = time.Date(2026, 5, day, 9, 0, 0, 0, time.UTC)
		if err := c.IncrementBy(ctx, subj, n); err != nil {
			t.Fatal(err)
		}
		if err := c.IncrementDetailBy(ctx, subj, "/v1/price", usage.ClassOK, n); err != nil {
			t.Fatal(err)
		}
	}
	r := usage.NewRollup(c, durable, time.Minute, nil)
	if _, err := r.SweepDays(ctx, []string{"2026-05-01", "2026-05-02", "2026-05-20"}); err != nil {
		t.Fatalf("SweepDays: %v", err)
	}

	mr.Del(dayKey(subj, "2026-05-01"))
	mr.Del(dayKey(subj, "2026-05-02"))

	clock = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	got, err := c.MonthToDate(ctx, subj)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 950 {
		t.Errorf("MonthToDate after evicting May 1-2 = %d, want 950 (Redis alone reads 250)", got)
	}
}

// TestMonthToDate_TakesMaxPerDayNeverSum: both sources are lower bounds
// of the true count, so a lagging rollup must not add to Redis and a
// recreated day key must not hide the rollup's larger value.
func TestMonthToDate_TakesMaxPerDayNeverSum(t *testing.T) {
	mr, rdb := newRedis(t)
	clock := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	durable := &fakeUsageDaily{}
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }), usage.WithDurableDays(durable))
	ctx := context.Background()
	const subj = "id:acct:acme"

	_ = durable.UpsertUsageDaily(ctx, []usage.RollupRow{
		{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/price", OK: 7},
		{Day: "2026-05-02", Subject: subj, Endpoint: "/v1/price", OK: 45, ClientErrors: 5, Throttled: 99},
	})
	_ = mr.Set(dayKey(subj, "2026-05-01"), "10") // rollup lags Redis
	_ = mr.Set(dayKey(subj, "2026-05-02"), "2")  // today's key evicted and recreated

	got, err := c.MonthToDate(ctx, subj)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 60 {
		t.Errorf("MonthToDate = %d, want 60 (max(10,7) + max(2,45+5); throttled is never billable)", got)
	}
}

// TestMonthToDate_DurableReadFailure: with no earlier read of the month
// a durable failure is an error (the gate's dwell must see it); after
// one, the earlier snapshot is still a lower bound and is served.
func TestMonthToDate_DurableReadFailure(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	durable := &fakeUsageDaily{err: errors.New("postgres down")}
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }), usage.WithDurableDays(durable))
	ctx := context.Background()
	const subj = "id:acct:acme"

	if _, err := c.MonthToDate(ctx, subj); err == nil {
		t.Fatal("MonthToDate with an unreadable usage_daily and no snapshot returned nil error")
	}

	durable.err = nil
	_ = durable.UpsertUsageDaily(ctx, []usage.RollupRow{{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/price", OK: 30}})
	if got, err := c.MonthToDate(ctx, subj); err != nil || got != 30 {
		t.Fatalf("MonthToDate = %d, %v; want 30, nil", got, err)
	}

	durable.err = errors.New("postgres down")
	clock = clock.Add(time.Hour)
	if got, err := c.MonthToDate(ctx, subj); err != nil || got != 30 {
		t.Errorf("MonthToDate on a failed refresh = %d, %v; want the snapshot's 30, nil", got, err)
	}
}

// TestMonthToDate_DurableReadsAreCached: MonthToDate runs on every
// metered request, so usage_daily is read once per subject per rollup
// interval, not once per request.
func TestMonthToDate_DurableReadsAreCached(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	durable := &fakeUsageDaily{}
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }), usage.WithDurableDays(durable))
	ctx := context.Background()

	for range 50 {
		if _, err := c.MonthToDate(ctx, "id:acct:acme"); err != nil {
			t.Fatal(err)
		}
	}
	if durable.reads != 1 {
		t.Errorf("usage_daily reads for 50 calls inside one interval = %d, want 1", durable.reads)
	}
	clock = clock.Add(usage.DefaultRollupInterval)
	if _, err := c.MonthToDate(ctx, "id:acct:acme"); err != nil {
		t.Fatal(err)
	}
	if durable.reads != 2 {
		t.Errorf("usage_daily reads after one interval = %d, want 2", durable.reads)
	}
}
