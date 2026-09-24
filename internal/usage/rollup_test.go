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

	// Second sweep with no new traffic: every row is exactly what the
	// sink already acknowledged, so nothing is written — a quiet tick
	// must not cost a Postgres round-trip per active subject-day.
	n, err = r.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(sink.batches) != 1 {
		t.Fatalf("quiet re-sweep upserted %d rows in %d batches, want 0 rows / 1 batch total", n, len(sink.batches))
	}

	// One more request on one key: only that row goes through, and it
	// carries the FULL cumulative value (the sink merges with GREATEST,
	// so a delta would under-count).
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	n, err = r.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(sink.batches) != 2 {
		t.Fatalf("changed re-sweep upserted %d rows in %d batches, want 1 row / 2 batches total", n, len(sink.batches))
	}
	changed := usage.RollupRow{Day: "2026-07-03", Subject: "key:k1", Endpoint: "/v1/price", OK: 2}
	if got := sink.batches[1]; len(got) != 1 || got[0] != changed {
		t.Errorf("changed batch = %+v, want exactly [%+v]", got, changed)
	}
}

// TestRollupSweep_ResendsAfterSinkFailure — rows a failed upsert did
// not land are not marked acknowledged; the next sweep sends them all.
func TestRollupSweep_ResendsAfterSinkFailure(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	_ = c.IncrementDetail(ctx, "key:k2", "/v1/ohlc", usage.ClassOK)

	sink := &fakeSink{err: errors.New("pg down")}
	r := usage.NewRollup(c, sink, time.Minute, nil)
	if _, err := r.Sweep(ctx); err == nil {
		t.Fatal("Sweep returned nil with a failing sink")
	}
	sink.err = nil
	n, err := r.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(sink.batches) != 1 || len(sink.batches[0]) != 2 {
		t.Fatalf("after sink recovery upserted %d rows, batches %+v; want both rows resent", n, sink.batches)
	}
}

// blockingSink holds every upsert until its context ends and reports
// which error ended it.
type blockingSink struct{ err error }

func (b *blockingSink) UpsertUsageDaily(ctx context.Context, _ []usage.RollupRow) error {
	<-ctx.Done()
	b.err = ctx.Err()
	return ctx.Err()
}

// TestRollupSweep_DeadlineBoundsSink — a sink that never answers is cut
// off within one interval, not held until the caller's context dies.
func TestRollupSweep_DeadlineBoundsSink(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	_ = c.IncrementDetail(context.Background(), "key:k1", "/v1/price", usage.ClassOK)

	const interval = 100 * time.Millisecond
	sink := &blockingSink{}
	r := usage.NewRollup(c, sink, interval, nil)

	// The caller's context outlives the interval by far: only the
	// sweep's own deadline can return before it does.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := r.Sweep(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sweep err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 10*interval {
		t.Fatalf("Sweep took %s against a %s interval; the sink was not bounded by the sweep deadline", elapsed, interval)
	}
	if !errors.Is(sink.err, context.DeadlineExceeded) {
		t.Errorf("sink saw %v, want context.DeadlineExceeded", sink.err)
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

// TestScanDetailFunc_StreamsOneRowAtATime — GH-1282's open remainder:
// a production sweep must not buffer a whole day's rows before
// processing them. ScanDetailFunc delivers rows to fn as they are
// decoded, so a callback that aborts after the first row sees the
// walk stop there instead of continuing to drain every remaining
// hash into memory first.
func TestScanDetailFunc_StreamsOneRowAtATime(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		subject := "key:kid_" + string(rune('a'+i))
		if err := c.IncrementDetail(ctx, subject, "/v1/price", usage.ClassOK); err != nil {
			t.Fatal(err)
		}
	}

	sentinel := errors.New("stop after first row")
	seen := 0
	err := c.ScanDetailFunc(ctx, []string{"2026-07-03"}, func(usage.DetailRow) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if seen != 1 {
		t.Fatalf("fn invoked %d times, want exactly 1 — a full day was materialized before the callback's error could stop the walk", seen)
	}
}

// TestScanDetailFunc_MatchesScanDetail — the streaming and batch
// forms must decode the identical set of rows; ScanDetail is now a
// thin wrapper over ScanDetailFunc and must not drop or reorder-merge
// anything relative to the field-by-field cross-check.
func TestScanDetailFunc_MatchesScanDetail(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		subject := "key:kid_" + string(rune('a'+i))
		if err := c.IncrementDetail(ctx, subject, "/v1/price", usage.ClassOK); err != nil {
			t.Fatal(err)
		}
	}

	want, err := c.ScanDetail(ctx, []string{"2026-07-03"})
	if err != nil {
		t.Fatal(err)
	}

	var got []usage.DetailRow
	if err := c.ScanDetailFunc(ctx, []string{"2026-07-03"}, func(row usage.DetailRow) error {
		got = append(got, row)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) || len(want) != 4 {
		t.Fatalf("ScanDetailFunc rows = %d, ScanDetail rows = %d, want 4 each", len(got), len(want))
	}
	sortDetailRows(got)
	sortDetailRows(want)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func sortDetailRows(rows []usage.DetailRow) {
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Subject < rows[j].Subject
	})
}

// foldedOK returns, per day, the OK count of the latest row the sink
// acknowledged for (subject, endpoint).
func foldedOK(sink *fakeSink, subject, endpoint string) map[string]int64 {
	out := map[string]int64{}
	for _, batch := range sink.batches {
		for _, row := range batch {
			if row.Subject == subject && row.Endpoint == endpoint {
				out[row.Day] = row.OK
			}
		}
	}
	return out
}

// TestRollupSweep_CatchesUpDaysAnOutageSkipped pins GH #798: a sink
// outage from Friday evening to Monday morning must not lose Friday's
// tail or the weekend from usage_daily. The first successful sweep
// after recovery re-folds every day no successful sweep covered since
// it ended, while the Redis counters still hold them.
func TestRollupSweep_CatchesUpDaysAnOutageSkipped(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 3, 17, 55, 0, 0, time.UTC) // Friday
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	hit := func(n int) {
		for i := 0; i < n; i++ {
			if err := c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK); err != nil {
				t.Fatal(err)
			}
		}
	}
	sink := &fakeSink{}
	r := usage.NewRollup(c, sink, time.Minute, nil)

	hit(2)
	for i := 0; i < 6; i++ { // drain the fresh process's retention walk
		if _, err := r.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	sink.err = errors.New("pg down")
	clock = time.Date(2026, 7, 3, 20, 0, 0, 0, time.UTC)
	hit(3)
	for _, at := range []time.Time{
		time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
	} {
		clock = at
		hit(at.Day())
		if _, err := r.Sweep(ctx); err == nil {
			t.Fatal("Sweep returned nil with a failing sink")
		}
	}

	sink.err = nil
	clock = time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC) // Monday
	hit(1)
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got := foldedOK(sink, "key:k1", "/v1/price")
	want := map[string]int64{"2026-07-03": 5, "2026-07-04": 4, "2026-07-05": 5, "2026-07-06": 1}
	for day, ok := range want {
		if got[day] != ok {
			t.Errorf("usage_daily %s OK = %d, want %d (all folded: %v)", day, got[day], ok, got)
		}
	}
}

// TestRollupSweep_FreshWorkerWalksRetentionWindow — a restarted API has
// no record of what was folded before it, so it re-folds every retained
// day, a bounded batch per sweep, without holding back the live days and
// without scanning past the Redis retention window.
func TestRollupSweep_FreshWorkerWalksRetentionWindow(t *testing.T) {
	_, rdb := newRedis(t)
	today := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	clock := today
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	seed := func(daysAgo, n int) {
		clock = today.AddDate(0, 0, -daysAgo)
		for i := 0; i < n; i++ {
			if err := c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(35, 9) // one day older than the retention window
	seed(34, 2) // the oldest retained day
	seed(10, 3)
	seed(0, 1)
	clock = today

	sink := &fakeSink{}
	r := usage.NewRollup(c, sink, time.Minute, nil)
	if _, err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	first := foldedOK(sink, "key:k1", "/v1/price")
	if first["2026-08-10"] != 1 || first["2026-07-07"] != 2 {
		t.Errorf("first sweep folded %v; want today (1) and the oldest retained day 2026-07-07 (2)", first)
	}
	if _, ok := first["2026-07-31"]; ok {
		t.Errorf("first sweep folded 2026-07-31, past its catch-up batch: %v", first)
	}
	for i := 0; i < 5; i++ {
		if _, err := r.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	all := foldedOK(sink, "key:k1", "/v1/price")
	if all["2026-07-31"] != 3 {
		t.Errorf("2026-07-31 OK = %d after the catch-up drained, want 3 (all: %v)", all["2026-07-31"], all)
	}
	if _, ok := all["2026-07-06"]; ok {
		t.Errorf("folded 2026-07-06, outside the 35-day retention window: %v", all)
	}
}

// TestRollupSweepDays_FoldsExactlyTheGivenDays — the ops recovery path
// folds the requested days and nothing around them.
func TestRollupSweepDays_FoldsExactlyTheGivenDays(t *testing.T) {
	_, rdb := newRedis(t)
	clock := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	clock = clock.AddDate(0, 0, 1)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)
	_ = c.IncrementDetail(ctx, "key:k1", "/v1/price", usage.ClassOK)

	sink := &fakeSink{}
	r := usage.NewRollup(c, sink, time.Minute, nil)
	n, err := r.SweepDays(ctx, []string{"2026-07-14"})
	if err != nil {
		t.Fatal(err)
	}
	got := foldedOK(sink, "key:k1", "/v1/price")
	if n != 1 || len(got) != 1 || got["2026-07-14"] != 2 {
		t.Errorf("SweepDays(2026-07-14) upserted %d row(s) %v; want exactly 2026-07-14 OK=2", n, got)
	}
}
