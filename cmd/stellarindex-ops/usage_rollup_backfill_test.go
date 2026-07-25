package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// recordingUsageSink stands in for *timescale.Store at the exact
// usage.RollupSink seam the production wiring uses.
type recordingUsageSink struct {
	rows []usage.RollupRow
	err  error
}

func (s *recordingUsageSink) UpsertUsageDaily(_ context.Context, rows []usage.RollupRow) error {
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, rows...)
	return nil
}

// seedUsageDetail writes per-endpoint counters for `day` through the
// REAL usage.Counter (clock pinned to that day), so the Redis keys and
// hash fields are exactly what middleware.UsageTracker writes in
// production rather than a hand-rolled approximation.
func seedUsageDetail(t *testing.T, rdb redis.Cmdable, day time.Time, subject, endpoint, class string, times int) {
	t.Helper()
	pinned := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, time.UTC)
	c := usage.New(rdb, usage.WithClock(func() time.Time { return pinned }))
	if c == nil {
		t.Fatal("usage.New returned nil — test setup invariant broken")
	}
	for i := 0; i < times; i++ {
		if err := c.IncrementDetail(context.Background(), subject, endpoint, class); err != nil {
			t.Fatalf("seed IncrementDetail: %v", err)
		}
	}
}

// TestUsageRollupBackfill_RecoversADaySweepCannotReach pins COR-10
// (audit-2026-07-23). The API's in-process rollup worker only ever
// folds TODAY + YESTERDAY, so a day whose sweep never succeeded — sink
// down, or the API process down, across a day boundary — is skipped
// permanently even though Redis holds its counters for 35 days.
//
// This test asserts BOTH halves of that claim against the same seeded
// Redis:
//
//	(a) the live worker's Sweep, run "today", recovers nothing for a
//	    10-day-old day — the gap is real, not hypothetical; and
//	(b) runUsageRollupBackfill folds that day's exact counters into
//	    the sink — the gap is closed.
//
// Half (b) is what the fix adds; half (a) keeps this test honest by
// proving the recovered rows genuinely lie outside the live window.
func TestUsageRollupBackfill_RecoversADaySweepCannotReach(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	today := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	lost := today.AddDate(0, 0, -10)
	const (
		subject  = "key:kid_515c8d94191f4e93"
		endpoint = "/v1/assets/{asset_id}"
	)
	seedUsageDetail(t, rdb, lost, subject, endpoint, usage.ClassOK, 7)
	seedUsageDetail(t, rdb, lost, subject, endpoint, usage.ClassThrottled, 3)

	// (a) The live worker, sweeping "today", cannot see the lost day.
	liveSink := &recordingUsageSink{}
	liveCounter := usage.New(rdb, usage.WithClock(func() time.Time { return today }))
	liveRollup := usage.NewRollup(liveCounter, liveSink, usage.DefaultRollupInterval, discardOpsLogger())
	if _, err := liveRollup.Sweep(context.Background()); err != nil {
		t.Fatalf("live Sweep: %v", err)
	}
	if len(liveSink.rows) != 0 {
		t.Fatalf("live worker recovered %d row(s) for a 10-day-old day: %+v — "+
			"the two-day window premise this backfill exists for no longer holds",
			len(liveSink.rows), liveSink.rows)
	}

	// (b) The backfill recovers it.
	sink := &recordingUsageSink{}
	if err := runUsageRollupBackfill(context.Background(), rdb, sink, []time.Time{lost}, false); err != nil {
		t.Fatalf("runUsageRollupBackfill: %v", err)
	}

	wantDay := lost.Format(usageRollupDateLayout)
	var got *usage.RollupRow
	for i := range sink.rows {
		if sink.rows[i].Day == wantDay && sink.rows[i].Endpoint == endpoint {
			got = &sink.rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no row for day %s endpoint %s; got %+v", wantDay, endpoint, sink.rows)
	}
	if got.Subject != subject {
		t.Errorf("Subject = %q, want %q", got.Subject, subject)
	}
	if got.OK != 7 {
		t.Errorf("OK = %d, want 7 (the exact count seeded into Redis for the lost day)", got.OK)
	}
	if got.Throttled != 3 {
		t.Errorf("Throttled = %d, want 3", got.Throttled)
	}
	if got.ClientErrors != 0 || got.ServerErrors != 0 {
		t.Errorf("ClientErrors/ServerErrors = %d/%d, want 0/0", got.ClientErrors, got.ServerErrors)
	}
}

// TestUsageRollupBackfill_DryRunWritesNothing — -dry-run must size the
// recovery without touching usage_daily, so an operator can check the
// blast radius of a range before committing to it.
func TestUsageRollupBackfill_DryRunWritesNothing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	day := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	seedUsageDetail(t, rdb, day, "key:kid_dryrun", "/v1/price", usage.ClassOK, 5)

	realSink := &recordingUsageSink{err: errors.New("the real sink must never be called under -dry-run")}
	counting := &countingUsageSink{}
	if err := runUsageRollupBackfill(context.Background(), rdb, counting, []time.Time{day}, true); err != nil {
		t.Fatalf("dry-run backfill: %v", err)
	}
	if counting.rows == 0 {
		t.Error("dry-run counted 0 rows; it must still scan and report what would be written")
	}
	if len(realSink.rows) != 0 {
		t.Errorf("real sink received %d row(s) under -dry-run", len(realSink.rows))
	}
}

// TestUsageRollupDays covers the flag-expansion guards: a range wider
// than the Redis retention window can only scan expired days while
// paying a full keyspace SCAN each, so it's refused rather than run.
func TestUsageRollupDays(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		wantDays int
		wantErr  bool
	}{
		{name: "single day defaults -to to -from", from: "2026-07-19", wantDays: 1},
		{name: "inclusive range", from: "2026-07-19", to: "2026-07-21", wantDays: 3},
		{name: "missing -from", wantErr: true},
		{name: "unparseable -from", from: "19-07-2026", wantErr: true},
		{name: "unparseable -to", from: "2026-07-19", to: "tomorrow", wantErr: true},
		{name: "reversed range", from: "2026-07-21", to: "2026-07-19", wantErr: true},
		{name: "exactly the retention window", from: "2026-06-16", to: "2026-07-20", wantDays: 35},
		{name: "one day past the retention window", from: "2026-06-15", to: "2026-07-20", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			days, err := usageRollupDays(tc.from, tc.to)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("usageRollupDays(%q, %q) = %d days, want error", tc.from, tc.to, len(days))
				}
				return
			}
			if err != nil {
				t.Fatalf("usageRollupDays(%q, %q): %v", tc.from, tc.to, err)
			}
			if len(days) != tc.wantDays {
				t.Fatalf("len(days) = %d, want %d", len(days), tc.wantDays)
			}
			if got := days[0].Format(usageRollupDateLayout); got != tc.from {
				t.Errorf("days[0] = %s, want %s", got, tc.from)
			}
		})
	}
}

func discardOpsLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
