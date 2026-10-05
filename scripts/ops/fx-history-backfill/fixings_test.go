package main

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external/forex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeBars serves a fixed series per (ticker, grain), clipped to each
// request's [from, to]; rateLimited answers that many 429s per ticker first.
type fakeBars struct {
	series      map[string][]forex.FXBar // key ticker+"/"+grain
	rateLimited map[string]int
	calls       int
}

func (f *fakeBars) ListAggBars(_ context.Context, ticker, grain string, from, to time.Time) ([]forex.FXBar, error) {
	f.calls++
	if f.rateLimited[ticker] > 0 {
		f.rateLimited[ticker]--
		return nil, &forex.StatusError{Code: 429}
	}
	var out []forex.FXBar
	for _, b := range f.series[ticker+"/"+grain] {
		if !b.BarStart.Before(from) && !b.BarStart.After(to) {
			out = append(out, b)
		}
	}
	return out, nil
}

type recordingFixingStore struct{ rows []timescale.FXFixing }

func (s *recordingFixingStore) InsertFXFixingBatch(_ context.Context, rows []timescale.FXFixing) (int64, error) {
	s.rows = append(s.rows, rows...)
	return int64(len(rows)), nil
}

func bars(ticker, grain string, start time.Time, width time.Duration, n int) []forex.FXBar {
	out := make([]forex.FXBar, n)
	for i := range out {
		s := start.Add(time.Duration(i) * width)
		out[i] = forex.FXBar{Ticker: ticker, Grain: grain, BarStart: s, BarEnd: s.Add(width), CloseText: "0.9", Source: "massive"}
	}
	return out
}

func TestRunFixings_HourlyFromH0DailyBelow(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h0 := from.Add(100 * 24 * time.Hour)
	now := h0.Add(72*time.Hour + 30*time.Minute)
	// Daily bars start at 22:00 (the vendor's day boundary) and run past H0.
	dailyStart := from.Add(-2 * time.Hour)
	lister := &fakeBars{series: map[string][]forex.FXBar{
		"EUR/1d": bars("EUR", forex.GrainDay, dailyStart, 24*time.Hour, 104),
		"EUR/1h": bars("EUR", forex.GrainHour, h0, time.Hour, 73),
	}}
	store := &recordingFixingStore{}
	var sleeps []time.Duration
	run := fixingsRun{
		logger: discardLogger(), client: lister, store: store, from: from, now: now,
		sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	}
	got, err := run.backfillTicker(context.Background(), "EUR")
	if err != nil {
		t.Fatalf("backfillTicker: %v", err)
	}
	if !got.h0.Equal(h0) {
		t.Errorf("h0 = %s, want %s", got.h0, h0)
	}
	if got.dailyOffset != 22*time.Hour {
		t.Errorf("daily t offset = %s, want 22h", got.dailyOffset)
	}
	var daily, hourly int
	for _, r := range store.rows {
		switch r.Grain {
		case forex.GrainDay:
			daily++
			if r.BarEnd.After(h0) {
				t.Errorf("daily bar %s ends after H0", r.BarStart)
			}
		case forex.GrainHour:
			hourly++
			if r.BarEnd.After(now.Add(-forex.FixingSettle)) {
				t.Errorf("open hourly bar %s written", r.BarStart)
			}
		}
		if r.RateUSD != "0.9" {
			t.Errorf("rate text = %q", r.RateUSD)
		}
	}
	// Daily bars fetched from `from` (the 22:00 bar before it is outside the
	// request) that end by H0: 100 − 1 = 99. Hourly: 72 closed of 73.
	if daily != 99 || hourly != 72 {
		t.Errorf("wrote daily=%d hourly=%d, want 99/72", daily, hourly)
	}
	if int(got.rows) != len(store.rows) {
		t.Errorf("reported rows %d, wrote %d", got.rows, len(store.rows))
	}
	for _, d := range sleeps {
		if d < fixingCallSpacing {
			t.Fatalf("call spaced %s, want ≥ %s", d, fixingCallSpacing)
		}
	}
	if len(sleeps) != lister.calls {
		t.Errorf("%d sleeps for %d calls: every call must be spaced", len(sleeps), lister.calls)
	}
}

func TestRunFixings_RateLimitRetriesThenFailsTheTicker(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := from.Add(10 * 24 * time.Hour)
	lister := &fakeBars{
		series: map[string][]forex.FXBar{
			"EUR/1h": bars("EUR", forex.GrainHour, from, time.Hour, 24),
			"GBP/1h": bars("GBP", forex.GrainHour, from, time.Hour, 24),
		},
		rateLimited: map[string]int{"EUR": 1, "GBP": fixingMaxRetries + 1},
	}
	var waits int
	run := fixingsRun{
		logger: discardLogger(), client: lister, store: &recordingFixingStore{}, from: from, now: now,
		sleep: func(_ context.Context, d time.Duration) error {
			if d == fixingRetryWait {
				waits++
			}
			return nil
		},
	}
	res := run.runFixings(context.Background(), []string{"EUR", "GBP"})
	if res.failedChunks != 1 || res.exitCode() != 1 {
		t.Fatalf("failed=%d exit=%d, want GBP failed and exit 1", res.failedChunks, res.exitCode())
	}
	if res.totalRows != 24 {
		t.Errorf("rows = %d, want EUR's 24 after one retried 429", res.totalRows)
	}
	if waits != 1+fixingMaxRetries {
		t.Errorf("retry waits = %d, want %d", waits, 1+fixingMaxRetries)
	}
}
