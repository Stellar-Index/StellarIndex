// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type refreshCall struct {
	view     string
	from, to time.Time
	forced   bool
}

type fakeLateCAGGStore struct {
	mu        sync.Mutex
	calls     []refreshCall
	failFirst int // fail this many refresh calls before succeeding
}

// The production policies (migrations 0002, 0036, 0064, 0068, 0165, 0187).
func (f *fakeLateCAGGStore) CAGGRefreshWindows(context.Context) ([]timescale.CAGGRefreshWindow, error) {
	p := func(view string, start, schedule time.Duration) timescale.CAGGRefreshWindow {
		return timescale.CAGGRefreshWindow{View: view, HasPolicy: true, StartOffset: start, ScheduleInterval: schedule}
	}
	day := 24 * time.Hour
	return []timescale.CAGGRefreshWindow{
		p("prices_1m", 15*time.Minute, 30*time.Second),
		p("prices_15m", time.Hour, 5*time.Minute),
		p("prices_1h", 4*time.Hour, 15*time.Minute),
		p("prices_4h", day, time.Hour),
		p("prices_1d", 7*day, 6*time.Hour),
		p("prices_1w", 28*day, day),
		p("prices_1mo", 90*day, day),
		p("dex_volume_by_pair_1d", 7*day, time.Hour),
		p("source_volume_1h", 7*day, 5*time.Minute),
		p("pools_per_source_1h", 7*day, 5*time.Minute),
		p("twap_1h", 4*time.Hour, 15*time.Minute),
		p("twap_1d", 7*day, 6*time.Hour),
	}, nil
}

func (f *fakeLateCAGGStore) record(view string, from, to time.Time, forced bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, refreshCall{view, from, to, forced})
	if f.failFirst > 0 {
		f.failFirst--
		return errors.New("55P03: refresh already running")
	}
	return nil
}

func (f *fakeLateCAGGStore) RefreshContinuousAggregate(_ context.Context, v string, from, to time.Time) error {
	return f.record(v, from, to, false)
}

func (f *fakeLateCAGGStore) RefreshContinuousAggregateForced(_ context.Context, v string, from, to time.Time) error {
	return f.record(v, from, to, true)
}

func (f *fakeLateCAGGStore) snapshot() []refreshCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]refreshCall(nil), f.calls...)
}

var lateTestNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newTestLateRefresher(f *fakeLateCAGGStore) *LateTradeRefresher {
	r := NewLateTradeRefresher(f, LateTradeRefresherOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r.now = func() time.Time { return lateTestNow }
	r.debounce = 5 * time.Millisecond
	r.maxBackoff = 20 * time.Millisecond
	return r
}

func tradesAt(ages ...time.Duration) []canonical.Trade {
	out := make([]canonical.Trade, len(ages))
	for i, a := range ages {
		out[i] = canonical.Trade{Source: "sdex", Ledger: uint32(i + 1), Timestamp: lateTestNow.Add(-a)}
	}
	return out
}

func TestLateTradeRefresh_OnTimeBatchRefreshesNothing(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	// Before the policies load every trade is pending; the plan still
	// finds none of them late.
	r.ObserveTrades(tradesAt(10*time.Second, 2*time.Minute)...)
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// After: on-time trades are not even recorded.
	r.ObserveTrades(tradesAt(10*time.Second, 8*time.Minute)...)
	if r.pending {
		t.Fatal("an on-time batch left a pending window")
	}
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := f.snapshot(); len(c) != 0 {
		t.Fatalf("on-time trades refreshed %+v", c)
	}
}

func TestLateTradeRefresh_LateBatchRefreshesPrices1mOverItsWindow(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	// Observed before the policies load, so the 1-minute-old trade is
	// pending too and the window has to be cut where the policy takes over.
	r.ObserveTrades(tradesAt(20*time.Minute, 18*time.Minute, time.Minute)...)
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// prices_1m coverage = 15m - 30s schedule - 1m bucket - 5m slack = 8m30s,
	// so its refresh ends there; prices_15m's 35m coverage holds all three.
	want := []refreshCall{{
		view: "prices_1m",
		from: lateTestNow.Add(-21 * time.Minute),
		to:   lateTestNow.Add(-8*time.Minute - 30*time.Second),
	}}
	if got := f.snapshot(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("calls = %+v, want %+v", got, want)
	}
	if r.pending {
		t.Fatal("window still pending after a successful refresh")
	}
}

func TestLateTradeRefresh_HoursLateRefreshesEveryUncoveredViewInOrder(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	r.ObserveTrades(tradesAt(6 * time.Hour)...)
	if err := r.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var views []string
	for _, c := range f.snapshot() {
		if c.forced {
			t.Errorf("%s refreshed forced", c.view)
		}
		if !c.from.Before(lateTestNow.Add(-6*time.Hour)) || !c.to.After(lateTestNow.Add(-6*time.Hour)) {
			t.Errorf("%s window [%s, %s) misses the trade", c.view, c.from, c.to)
		}
		views = append(views, c.view)
	}
	want := []string{"prices_1m", "prices_15m", "prices_1h", "twap_1h"}
	if len(views) != len(want) {
		t.Fatalf("views = %v, want %v", views, want)
	}
	for i := range want {
		if views[i] != want[i] {
			t.Fatalf("views = %v, want %v", views, want)
		}
	}
}

func TestLateTradeRefresh_BurstCoalescesIntoOneRefresh(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.ObserveTrades(tradesAt(time.Duration(20+i%10) * time.Minute)...)
		}(i)
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitForCalls(t, f, 1)
	time.Sleep(50 * time.Millisecond)
	got := f.snapshot()
	want := refreshCall{view: "prices_1m", from: lateTestNow.Add(-30 * time.Minute), to: lateTestNow.Add(-19 * time.Minute)}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %+v, want one %+v", got, want)
	}
}

func TestLateTradeRefresh_FailureIsCountedAndRetried(t *testing.T) {
	f := &fakeLateCAGGStore{failFirst: 1}
	r := newTestLateRefresher(f)
	errBefore := testutil.ToFloat64(obs.LateTradeCAGGRefreshTotal.WithLabelValues("error"))
	okBefore := testutil.ToFloat64(obs.LateTradeCAGGRefreshTotal.WithLabelValues("ok"))
	r.ObserveTrades(tradesAt(20 * time.Minute)...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitForCalls(t, f, 2)
	got := f.snapshot()
	if got[0] != got[1] {
		t.Fatalf("retry refreshed %+v, first attempt %+v", got[1], got[0])
	}
	waitFor(t, func() bool {
		return testutil.ToFloat64(obs.LateTradeCAGGRefreshTotal.WithLabelValues("ok"))-okBefore == 1
	})
	if d := testutil.ToFloat64(obs.LateTradeCAGGRefreshTotal.WithLabelValues("error")) - errBefore; d != 1 {
		t.Fatalf("error count rose by %v, want 1", d)
	}
}

type stubTradeWriter struct{ err error }

func (s stubTradeWriter) BatchInsertTrades(context.Context, []canonical.Trade) error { return s.err }
func (s stubTradeWriter) InsertTrade(context.Context, canonical.Trade) error         { return s.err }
func (stubTradeWriter) WouldPopulateUSDVolume(context.Context, canonical.Trade) bool { return false }

func TestObservingTradeWriter_ObservesOnlyCommittedWrites(t *testing.T) {
	r := newTestLateRefresher(&fakeLateCAGGStore{})
	late := tradesAt(time.Hour)
	failing := observingTradeWriter{tradeWriter: stubTradeWriter{err: errors.New("down")}, late: r}
	_ = failing.BatchInsertTrades(context.Background(), late)
	_ = failing.InsertTrade(context.Background(), late[0])
	if r.pending {
		t.Fatal("a failed write was observed")
	}
	ok := observingTradeWriter{tradeWriter: stubTradeWriter{}, late: r}
	if err := ok.InsertTrade(context.Background(), late[0]); err != nil {
		t.Fatal(err)
	}
	if !r.pending || !r.lo.Equal(late[0].Timestamp) {
		t.Fatalf("committed write not observed: pending=%v lo=%s", r.pending, r.lo)
	}
}

func waitForCalls(t *testing.T, f *fakeLateCAGGStore, n int) {
	t.Helper()
	waitFor(t, func() bool { return len(f.snapshot()) >= n })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
