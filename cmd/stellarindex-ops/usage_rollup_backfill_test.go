package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// These tests run with `go test ./cmd/stellarindex-ops/ -run UsageRollup`
// and the worker's with `go test ./internal/usage/`; internal/ops holds
// no usage code, so `./internal/ops/... -run 'Usage|Rollup'` runs nothing.

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

// TestUsageRollupBackfill_RecoversADaySweepCannotReach — the manual
// recovery path folds exactly the requested day's counters into the
// sink, and not the day before it (it folds the days it is given, not
// a live sweep window pinned to them).
func TestUsageRollupBackfill_RecoversADaySweepCannotReach(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	lost := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	const (
		subject  = "key:kid_test_fixture_not_a_secret"
		endpoint = "/v1/assets/{asset_id}"
	)
	seedUsageDetail(t, rdb, lost, subject, endpoint, usage.ClassOK, 7)
	seedUsageDetail(t, rdb, lost, subject, endpoint, usage.ClassThrottled, 3)
	seedUsageDetail(t, rdb, lost.AddDate(0, 0, -1), subject, endpoint, usage.ClassOK, 4)

	sink := &recordingUsageSink{}
	if err := runUsageRollupBackfill(context.Background(), rdb, sink, []time.Time{lost}, false); err != nil {
		t.Fatalf("runUsageRollupBackfill: %v", err)
	}

	wantDay := lost.Format(usageRollupDateLayout)
	if len(sink.rows) != 1 {
		t.Errorf("backfill of %s upserted %d row(s), want exactly that day's 1: %+v", wantDay, len(sink.rows), sink.rows)
	}
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
// blast radius of a range before committing to it. The REAL sink is
// handed in (as the production wiring does) and fails any write, so a
// dry run that reaches it errors instead of passing silently.
func TestUsageRollupBackfill_DryRunWritesNothing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	day := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	seedUsageDetail(t, rdb, day, "key:kid_dryrun", "/v1/price", usage.ClassOK, 5)

	realSink := &recordingUsageSink{err: errors.New("the real sink must never be called under -dry-run")}
	var runErr error
	out := captureOpsStderr(t, func() {
		runErr = runUsageRollupBackfill(context.Background(), rdb, realSink, []time.Time{day}, true)
	})
	if runErr != nil {
		t.Fatalf("dry-run backfill reached the real sink: %v", runErr)
	}
	if len(realSink.rows) != 0 {
		t.Errorf("real sink received %d row(s) under -dry-run", len(realSink.rows))
	}
	const wantSummary = "would upsert 1 row(s) across 1 day(s)"
	if !strings.Contains(out, wantSummary) {
		t.Errorf("dry-run must still scan and report what would be written: want %q in output:\n%s", wantSummary, out)
	}
}

// captureOpsStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureOpsStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return string(<-done)
}

// TestUsageRollupDays covers the flag-expansion guards. Redis holds the
// counters for usage.RetentionDays ending today, so a day before that
// window has expired and a day after today has not happened: folding
// either prints "0 row(s)" and exits 0, which reads as "no traffic".
// Both are refused however narrow the range (CA2-A26-harden-3).
func TestUsageRollupDays(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC)
	cases := []struct {
		name     string
		from, to string
		wantDays int
		wantErr  bool
	}{
		{name: "single day defaults -to to -from", from: "2026-07-19", wantDays: 1},
		{name: "inclusive range ending today", from: "2026-07-19", to: "2026-07-21", wantDays: 3},
		{name: "missing -from", wantErr: true},
		{name: "unparseable -from", from: "19-07-2026", wantErr: true},
		{name: "unparseable -to", from: "2026-07-19", to: "tomorrow", wantErr: true},
		{name: "reversed range", from: "2026-07-21", to: "2026-07-19", wantErr: true},
		{name: "exactly the retention window", from: "2026-06-17", to: "2026-07-21", wantDays: 35},
		{name: "one day past the retention window", from: "2026-06-16", to: "2026-07-21", wantErr: true},
		{name: "narrow range entirely expired", from: "2026-05-01", to: "2026-05-03", wantErr: true},
		{name: "-to in the future", from: "2026-07-21", to: "2026-07-22", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			days, err := usageRollupDays(tc.from, tc.to, now)
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
