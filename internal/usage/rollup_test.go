package usage_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/obstest"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// fakeSink records every UpsertUsageDaily batch. err, when set,
// fails the call (sink_error path).
type fakeSink struct {
	batches [][]usage.RollupRow
	err     error
}

func (f *fakeSink) UpsertUsageDaily(_ context.Context, rows []usage.RollupRow) error {
	if f.err != nil {
		return f.err
	}
	cp := make([]usage.RollupRow, len(rows))
	copy(cp, rows)
	f.batches = append(f.batches, cp)
	return nil
}

func sortRows(rows []usage.RollupRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Endpoint < b.Endpoint
	})
}

// TestIncrementDetailAndScan_RoundTrip — detail HINCRBYs come back
// out of ScanDetail with subject / endpoint / class intact,
// including a subject containing ':' (the url-escape contract).
func TestIncrementDetailAndScan_RoundTrip(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := c.IncrementDetail(ctx, "key:kid_1", "/v1/price", usage.ClassOK); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.IncrementDetail(ctx, "key:kid_1", "/v1/price", usage.ClassServerError); err != nil {
		t.Fatal(err)
	}
	if err := c.IncrementDetail(ctx, "id:owner:42", "/v1/assets/{asset_id}", usage.ClassThrottled); err != nil {
		t.Fatal(err)
	}

	rows, err := c.ScanDetail(ctx, []string{"2026-07-03"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (%+v)", len(rows), rows)
	}
	byField := map[[3]string]usage.DetailRow{}
	for _, r := range rows {
		if r.Date != "2026-07-03" {
			t.Errorf("Date = %q", r.Date)
		}
		byField[[3]string{r.Subject, r.Endpoint, r.Class}] = r
	}
	if got := byField[[3]string{"key:kid_1", "/v1/price", usage.ClassOK}].Count; got != 3 {
		t.Errorf("ok count = %d, want 3", got)
	}
	if got := byField[[3]string{"key:kid_1", "/v1/price", usage.ClassServerError}].Count; got != 1 {
		t.Errorf("5xx count = %d, want 1", got)
	}
	// Subject with ':' bytes survives the escape/unescape round trip.
	if got := byField[[3]string{"id:owner:42", "/v1/assets/{asset_id}", usage.ClassThrottled}].Count; got != 1 {
		t.Errorf("throttled count = %d, want 1", got)
	}
}

// TestScanDetail_OtherDatesExcluded — a scan for one date must not
// pick up hashes for neighbouring days.
func TestScanDetail_OtherDatesExcluded(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 2, 23, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	_ = c.IncrementDetail(ctx, "key:a", "/v1/price", usage.ClassOK)
	clock = time.Date(2026, 7, 3, 1, 0, 0, 0, time.UTC)
	_ = c.IncrementDetail(ctx, "key:a", "/v1/price", usage.ClassOK)

	rows, err := c.ScanDetail(ctx, []string{"2026-07-03"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Date != "2026-07-03" || rows[0].Count != 1 {
		t.Fatalf("rows = %+v, want single 2026-07-03 row with count 1", rows)
	}
}

// TestRollupSweep_GroupsAndUpserts — one sweep folds today's +
// yesterday's per-class counters into per-(day, subject, endpoint)
// rows, and repeating the sweep hands the sink the SAME cumulative
// batch (idempotence is then the sink's GREATEST()-merge contract).
func TestRollupSweep_GroupsAndUpserts(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	// Yesterday (relative to the sweep clock below).
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassClientError)

	// Today.
	clock = time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/ohlc", usage.ClassThrottled)
	_ = c.IncrementDetail(ctx, "id:acct-2", "/v1/price", usage.ClassServerError)

	sink := &fakeSink{}
	r := usage.NewRollup(c, sink, time.Minute, nil)
	if r == nil {
		t.Fatal("NewRollup returned nil with live deps")
	}

	n, err := r.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("Sweep rows = %d, want 4", n)
	}
	want := []usage.RollupRow{
		{Day: "2026-07-02", Subject: "key:k1", Endpoint: "/v1/price", OK: 2, ClientErrors: 1},
		{Day: "2026-07-03", Subject: "id:acct-2", Endpoint: "/v1/price", ServerErrors: 1},
		{Day: "2026-07-03", Subject: "key:k1", Endpoint: "/v1/ohlc", Throttled: 1},
		{Day: "2026-07-03", Subject: "key:k1", Endpoint: "/v1/price", OK: 1},
	}
	got := sink.batches[0]
	sortRows(got)
	if len(got) != len(want) {
		t.Fatalf("batch = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Second sweep with no new traffic: same cumulative batch again
	// (counters are cumulative; the sink merges with GREATEST so a
	// replay is a no-op server-side).
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	second := sink.batches[1]
	sortRows(second)
	for i := range want {
		if second[i] != want[i] {
			t.Errorf("replay row[%d] = %+v, want %+v (sweep must be idempotent)", i, second[i], want[i])
		}
	}
}

// TestRollupSweep_Metrics — the paired outcome counter + duration
// histogram advance on both the ok and sink_error paths (wave-100
// obstest convention).
func TestRollupSweep_Metrics(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)

	okBefore := obstest.HistogramSampleCount(t,
		obs.UsageRollupSweepDurationSeconds, "outcome", "ok")
	r := usage.NewRollup(c, &fakeSink{}, time.Minute, nil)
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	okAfter := obstest.HistogramSampleCount(t,
		obs.UsageRollupSweepDurationSeconds, "outcome", "ok")
	if okAfter != okBefore+1 {
		t.Errorf("ok histogram count = %d, want %d", okAfter, okBefore+1)
	}

	errBefore := obstest.HistogramSampleCount(t,
		obs.UsageRollupSweepDurationSeconds, "outcome", "sink_error")
	rErr := usage.NewRollup(c, &fakeSink{err: errors.New("pg down")}, time.Minute, nil)
	if _, err := rErr.Sweep(ctx); err == nil {
		t.Fatal("Sweep should surface the sink error")
	}
	errAfter := obstest.HistogramSampleCount(t,
		obs.UsageRollupSweepDurationSeconds, "outcome", "sink_error")
	if errAfter != errBefore+1 {
		t.Errorf("sink_error histogram count = %d, want %d", errAfter, errBefore+1)
	}
}

// TestNewRollup_NilDeps — missing counter or sink yields a nil
// worker so main.go can gate with a plain nil check.
func TestNewRollup_NilDeps(t *testing.T) {
	_, rdb := newRedis(t)
	c := usage.New(rdb)
	if usage.NewRollup(nil, &fakeSink{}, time.Minute, nil) != nil {
		t.Error("nil counter should yield nil Rollup")
	}
	if usage.NewRollup(c, nil, time.Minute, nil) != nil {
		t.Error("nil sink should yield nil Rollup")
	}
}

// dupScanRedis wraps a real client and makes SCAN return every key
// TWICE — the documented Redis behaviour when the keyspace rehashes
// mid-cursor ("an element may be returned multiple times"). miniredis
// never does this, which is why the cold-audit 2026-08-03 defect
// survived: a duplicate re-read the same hash, groupDetails SUMMED
// both, and usage_daily's GREATEST() merge froze the inflated count
// into a CLOSED day permanently — a customer's usage history doubled
// by a Redis implementation detail.
type dupScanRedis struct {
	redis.Cmdable
}

func (d dupScanRedis) Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd {
	keys, next, err := d.Cmdable.Scan(ctx, cursor, match, count).Result()
	if err != nil {
		return redis.NewScanCmdResult(nil, 0, err)
	}
	doubled := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		doubled = append(doubled, k, k)
	}
	return redis.NewScanCmdResult(doubled, next, nil)
}

// TestScanDetail_DuplicateScanKeysCountOnce — a key SCAN returns more
// than once must contribute its counts exactly once.
func TestScanDetail_DuplicateScanKeysCountOnce(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	c := usage.New(dupScanRedis{Cmdable: rdb}, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := c.IncrementDetail(ctx, "key:kid_1", "/v1/price", usage.ClassOK); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := c.ScanDetail(ctx, []string{"2026-07-03"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly 1 (the duplicate SCAN hit must not produce a second row)", rows)
	}
	if rows[0].Count != 5 {
		t.Errorf("count = %d, want 5 — a duplicated SCAN key double-counted the day", rows[0].Count)
	}
}
