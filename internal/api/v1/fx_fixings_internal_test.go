package v1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func fixingRow(ticker, grain string, barEnd time.Time, rate string, gen int64) timescale.FXFixing {
	width := time.Hour
	if grain == timescale.FXGrainDay {
		width = 24 * time.Hour
	}
	return timescale.FXFixing{Ticker: ticker, Grain: grain, BarStart: barEnd.Add(-width), BarEnd: barEnd, RateUSD: rate, Source: "massive", Generation: gen}
}

// countingFixings serves a fixed window and counts every read.
type countingFixings struct {
	mu        sync.Mutex
	rows      []timescale.FXFixing
	loadAt    time.Time
	dbReads   int
	loads     []time.Time // ingestedAfter of each load
	dbBinding map[string]timescale.FXFixingBinding
}

func (f *countingFixings) FXFixingAtOrBefore(_ context.Context, tickers []string, _ time.Time, _ time.Duration) (map[string]timescale.FXFixingBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbReads++
	out := map[string]timescale.FXFixingBinding{}
	for _, t := range tickers {
		if b, ok := f.dbBinding[t]; ok {
			out[t] = b
		}
	}
	return out, nil
}

func (f *countingFixings) LoadFXFixingWindow(_ context.Context, _ time.Duration, after time.Time) ([]timescale.FXFixing, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, after)
	return f.rows, f.loadAt, nil
}

func (f *countingFixings) reads() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dbReads, len(f.loads)
}

func waitLoaded(t *testing.T, c *fxFixingCache) {
	t.Helper()
	c.mu.Lock()
	done := c.flight
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func TestFXFixingCutoff(t *testing.T) {
	bar := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) // a Wednesday
	rows := []timescale.FXFixing{
		fixingRow("EUR", timescale.FXGrainHour, bar, "0.90", 0),
		fixingRow("EUR", timescale.FXGrainDay, bar, "0.80", 0),
		fixingRow("EUR", timescale.FXGrainHour, bar.Add(-time.Hour), "0.70", 0),
		fixingRow("EUR", timescale.FXGrainHour, bar.Add(-time.Hour), "0.71", 1),
	}
	const maxAge = 76 * time.Hour
	cases := []struct {
		name     string
		e        time.Time
		wantRate string
	}{
		{"bar invisible one minute before E − lag reaches it", bar.Add(timescale.FXFixingLag - time.Minute), "0.71"},
		{"bar visible at E − lag == bar_end; 1h beats 1d", bar.Add(timescale.FXFixingLag), "0.90"},
		{"highest generation wins", bar.Add(timescale.FXFixingLag - 30*time.Minute), "0.71"},
		{"lookback bound is exclusive", bar.Add(timescale.FXFixingLag + maxAge), ""},
		{"inside the lookback", bar.Add(timescale.FXFixingLag + maxAge - time.Second), "0.90"},
	}
	for _, tc := range cases {
		got, ok := selectFixing(rows, tc.e.Add(-timescale.FXFixingLag), maxAge)
		if tc.wantRate == "" {
			if ok {
				t.Errorf("%s: bound %+v, want a miss", tc.name, got)
			}
			continue
		}
		if !ok || got.RateUSD != tc.wantRate {
			t.Errorf("%s: bound %q (ok=%v), want %q", tc.name, got.RateUSD, ok, tc.wantRate)
		}
	}
}

func TestFXFixingCacheHorizon(t *testing.T) {
	const maxAge = 76 * time.Hour
	loadAt := time.Date(2026, 9, 30, 12, 0, 30, 0, time.UTC)
	reader := &countingFixings{
		rows:      []timescale.FXFixing{fixingRow("BRL", timescale.FXGrainHour, time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC), "5.1", 0)},
		loadAt:    loadAt,
		dbBinding: map[string]timescale.FXFixingBinding{"BRL": {FXFixing: fixingRow("BRL", timescale.FXGrainHour, time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC), "5.1", 0), Resolution: timescale.FXResolutionHourly}},
	}
	c := newFXFixingCache(reader, slog.New(slog.NewTextHandler(io.Discard, nil)), maxAge)
	ctx := context.Background()
	e := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	// Cold: the store answers and one full load starts.
	if _, err := c.bind(ctx, []string{"BRL"}, e); err != nil {
		t.Fatal(err)
	}
	waitLoaded(t, c)
	if db, loads := reader.reads(); db != 1 || loads != 1 || !reader.loads[0].IsZero() {
		t.Fatalf("cold: db=%d loads=%d first-after=%s, want 1/1/zero", db, loads, reader.loads[0])
	}

	// E ≤ L inside the horizon: memory answers, equal to the store's.
	got, err := c.bind(ctx, []string{"BRL"}, e)
	if err != nil || got["BRL"].RateUSD != "5.1" || got["BRL"].Resolution != timescale.FXResolutionHourly {
		t.Fatalf("warm: %+v %v", got, err)
	}
	if db, _ := reader.reads(); db != 1 {
		t.Errorf("warm bind read the store (%d reads)", db)
	}

	// E > L: the store answers and one delta load re-reads from L − slack.
	if _, err := c.bind(ctx, []string{"BRL"}, loadAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitLoaded(t, c)
	db, loads := reader.reads()
	if db != 2 || loads != 2 || !reader.loads[1].Equal(loadAt.Add(-timescale.FXFixingIngestSlack)) {
		t.Errorf("E > L: db=%d loads=%d after=%v, want 2/2/L−slack", db, loads, reader.loads)
	}

	// E whose lookback reaches below the held floor: the store answers.
	old := loadAt.Add(-c.horizon()).Add(maxAge + timescale.FXFixingLag - time.Second)
	if _, err := c.bind(ctx, []string{"BRL"}, old); err != nil {
		t.Fatal(err)
	}
	if db, _ := reader.reads(); db != 3 {
		t.Errorf("below-floor bind answered from memory (%d store reads)", db)
	}

	// A ticker the window does not hold reads the store, which may miss.
	if _, err := c.bind(ctx, []string{"BRL", "JPY"}, e); !errors.Is(err, errFXLegMissing) {
		t.Errorf("unheld ticker: err = %v, want errFXLegMissing", err)
	}
}

func TestFXFixingStale(t *testing.T) {
	hourly := func(barEnd time.Time) timescale.FXFixingBinding {
		return timescale.FXFixingBinding{FXFixing: fixingRow("EUR", timescale.FXGrainHour, barEnd, "0.9", 0), Resolution: timescale.FXResolutionHourly}
	}
	wed := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	sat := time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		e    time.Time
		b    timescale.FXFixingBinding
		want bool
	}{
		{"weekday, 4h past E − lag", wed, hourly(wed.Add(-timescale.FXFixingLag - 4*time.Hour)), false},
		{"weekday, 30h past E − lag", wed, hourly(wed.Add(-timescale.FXFixingLag - 30*time.Hour)), true},
		{"Saturday, 60h past E − lag", sat, hourly(sat.Add(-timescale.FXFixingLag - 60*time.Hour)), false},
		{"Monday 01:59 cutoff is still the close", time.Date(2026, 10, 5, 4, 59, 0, 0, time.UTC), hourly(time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC)), false},
		{"Monday 02:00 cutoff reopens", time.Date(2026, 10, 5, 5, 0, 0, 0, time.UTC), hourly(time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC)), true},
		{"daily carries no FX staleness", wed, timescale.FXFixingBinding{FXFixing: fixingRow("EUR", timescale.FXGrainDay, wed.Add(-50*time.Hour), "0.9", 0), Resolution: timescale.FXResolutionDaily}, false},
	}
	for _, tc := range cases {
		if got := fxFixingStale(tc.e, tc.b); got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
}
