// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
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
	failFirst int    // fail this many refresh calls before succeeding
	failView  string // every refresh of this view fails
	block     bool   // every refresh waits for its ctx to end
	onRefresh func(view string)

	durable   map[string]timescale.CAGGLateRefreshWindow // by view
	gen       int64
	records   int
	recordErr error
}

func (f *fakeLateCAGGStore) RecordCAGGLateRefreshWindow(_ context.Context, _ string, views []string, from, to time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.records++
	if f.durable == nil {
		f.durable = map[string]timescale.CAGGLateRefreshWindow{}
	}
	for _, v := range views {
		f.gen++
		w, ok := f.durable[v]
		if !ok {
			w = timescale.CAGGLateRefreshWindow{View: v, From: from, To: to, FirstSeen: lateTestNow}
		}
		if from.Before(w.From) {
			w.From = from
		}
		if to.After(w.To) {
			w.To = to
		}
		w.Gen = f.gen
		f.durable[v] = w
	}
	return nil
}

func (f *fakeLateCAGGStore) CAGGLateRefreshWindows(context.Context, string) ([]timescale.CAGGLateRefreshWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []timescale.CAGGLateRefreshWindow
	for _, w := range f.durable {
		out = append(out, w)
	}
	return out, nil
}

func (f *fakeLateCAGGStore) ClearCAGGLateRefreshWindow(_ context.Context, _, view string, gen int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.durable[view]; ok && w.Gen == gen {
		delete(f.durable, view)
		return true, nil
	}
	return false, nil
}

func (f *fakeLateCAGGStore) durableViews() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for v := range f.durable {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
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

func (f *fakeLateCAGGStore) record(ctx context.Context, view string, from, to time.Time, forced bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, refreshCall{view, from, to, forced})
	fail, block, hook := f.failFirst > 0, f.block, f.onRefresh
	if fail {
		f.failFirst--
	}
	fail = fail || view == f.failView
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail {
		return errors.New("55P03: refresh already running")
	}
	if hook != nil {
		hook(view)
	}
	return nil
}

func (f *fakeLateCAGGStore) RefreshContinuousAggregate(ctx context.Context, v string, from, to time.Time) error {
	return f.record(ctx, v, from, to, false)
}

func (f *fakeLateCAGGStore) RefreshContinuousAggregateForced(ctx context.Context, v string, from, to time.Time) error {
	return f.record(ctx, v, from, to, true)
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
	flushOK(t, r)
	// After: on-time trades are not even recorded.
	r.ObserveTrades(tradesAt(10*time.Second, 8*time.Minute)...)
	if r.pending {
		t.Fatal("an on-time batch left a pending window")
	}
	flushOK(t, r)
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
	flushOK(t, r)
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
	if r.anyPendingLocked() {
		t.Fatal("window still pending after a successful refresh")
	}
}

func TestLateTradeRefresh_HoursLateRefreshesEveryUncoveredViewInOrder(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	r.ObserveTrades(tradesAt(6 * time.Hour)...)
	flushOK(t, r)
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

func flushOK(t *testing.T, r *LateTradeRefresher) time.Duration {
	t.Helper()
	next, err := r.flush(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

// lateClock is a settable r.now.
type lateClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lateClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func viewsOf(calls []refreshCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.view)
	}
	return out
}

func TestLateTradeRefresh_FlushOnShutdownRefreshesWhatRunLeft(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); r.Run(ctx) }()
	cancel()
	<-runDone
	// Written by the sink's shutdown drain, after Run stopped.
	r.ObserveTrades(tradesAt(20 * time.Minute)...)
	r.FlushOnShutdown(context.Background())
	if got := viewsOf(f.snapshot()); len(got) != 1 || got[0] != "prices_1m" {
		t.Fatalf("shutdown flush refreshed %v, want [prices_1m]", got)
	}
	if r.anyPendingLocked() {
		t.Fatal("window still pending after the shutdown flush")
	}
}

func TestLateTradeRefresh_ShutdownTimeoutIsLogged(t *testing.T) {
	f := &fakeLateCAGGStore{block: true}
	r := newTestLateRefresher(f)
	var logs bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	r.ObserveTrades(tradesAt(20 * time.Minute)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.FlushOnShutdown(ctx)
	out := logs.String()
	wantFrom := lateTestNow.Add(-20 * time.Minute).Format(time.RFC3339)
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "view=prices_1m") || !strings.Contains(out, "trades_from="+wantFrom) {
		t.Fatalf("abandoned window not logged at ERROR with its view and range:\n%s", out)
	}
}

func TestLateTradeRefresh_ShutdownWithNothingPendingIsNotAbandoned(t *testing.T) {
	r := newTestLateRefresher(&fakeLateCAGGStore{block: true})
	var logs bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.FlushOnShutdown(ctx)
	if out := logs.String(); strings.Contains(out, "level=ERROR") {
		t.Fatalf("nothing pending, but logged:\n%s", out)
	}
}

func TestLateTradeRefresh_FailedViewDoesNotStopTheOthers(t *testing.T) {
	f := &fakeLateCAGGStore{failView: "prices_15m"}
	r := newTestLateRefresher(f)
	r.ObserveTrades(tradesAt(6 * time.Hour)...)
	if _, err := r.flush(context.Background(), true); err == nil || !strings.Contains(err.Error(), "prices_15m") {
		t.Fatalf("err = %v, want prices_15m's failure", err)
	}
	got := viewsOf(f.snapshot())
	for _, v := range []string{"prices_1m", "prices_15m", "prices_1h", "twap_1h"} {
		if !slices.Contains(got, v) {
			t.Fatalf("refreshed %v; %s missing after prices_15m failed", got, v)
		}
	}
	if !r.views["prices_15m"].pending || r.views["prices_1h"].pending {
		t.Fatal("want only the failed view kept pending")
	}
}

func TestLateTradeRefresh_Prices1mFailureHoldsOnlyTheTwaps(t *testing.T) {
	f := &fakeLateCAGGStore{failView: "prices_1m"}
	r := newTestLateRefresher(f)
	r.ObserveTrades(tradesAt(6 * time.Hour)...)
	if _, err := r.flush(context.Background(), true); err == nil {
		t.Fatal("prices_1m's failure was not returned")
	}
	got := viewsOf(f.snapshot())
	if slices.Contains(got, "twap_1h") || !slices.Contains(got, "prices_1h") {
		t.Fatalf("refreshed %v; want twap_1h held and prices_1h refreshed", got)
	}
	if !r.views["twap_1h"].pending {
		t.Fatal("twap_1h dropped its window while prices_1m failed")
	}
}

func overdueSince(r *LateTradeRefresher) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.overdueSince
}

func TestLateTradeRefresh_OverdueAgeGrowsThroughFailuresAndClearsOnSuccess(t *testing.T) {
	clock := &lateClock{t: lateTestNow}
	f := &fakeLateCAGGStore{failFirst: 2}
	r := newTestLateRefresher(f)
	r.now = clock.now
	r.ObserveTrades(tradesAt(20 * time.Minute)...)
	if got := overdueSince(r); !got.Equal(lateTestNow) {
		t.Fatalf("overdue since %s on observe, want %s", got, lateTestNow)
	}
	for range 2 {
		clock.advance(11 * time.Minute)
		if _, err := r.flush(context.Background(), true); err == nil {
			t.Fatal("want the injected failure")
		}
		if got := overdueSince(r); !got.Equal(lateTestNow) {
			t.Fatalf("a failed refresh moved overdue since to %s, want %s", got, lateTestNow)
		}
	}
	flushOK(t, r)
	if got := overdueSince(r); !got.IsZero() {
		t.Fatalf("overdue since %s after success, want cleared", got)
	}
}

func TestLateTradeRefresh_RateLimitedViewIsNotOverdue(t *testing.T) {
	clock := &lateClock{t: lateTestNow}
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	r.now = clock.now
	r.ObserveTrades(canonical.Trade{Timestamp: lateTestNow.Add(-6 * 24 * time.Hour)})
	flushOK(t, r)
	clock.advance(time.Minute)
	r.ObserveTrades(canonical.Trade{Timestamp: clock.now().Add(-6 * 24 * time.Hour)})
	flushOK(t, r)
	// Held: prices_15m (5m schedule) is the first to fall due, 4m from now.
	if got, want := overdueSince(r), lateTestNow.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("overdue since %s, want %s", got, want)
	}
}

func TestLateTradeRefresh_TwapCutoffIsPlannedAfterPrices1mReturns(t *testing.T) {
	clock := &lateClock{t: lateTestNow}
	f := &fakeLateCAGGStore{onRefresh: func(view string) {
		if view == "prices_1m" {
			clock.advance(10 * time.Minute)
		}
	}}
	r := newTestLateRefresher(f)
	r.now = clock.now
	r.ObserveTrades(tradesAt(6*time.Hour, 2*time.Hour)...)
	flushOK(t, r)
	// twap_1h coverage = 4h - 15m - 1h - 5m = 2h40m, from the clock after
	// prices_1m's refresh, not before it.
	wantTo := lateTestNow.Add(10*time.Minute - 2*time.Hour - 40*time.Minute)
	for _, c := range f.snapshot() {
		if c.view == "twap_1h" {
			if !c.to.Equal(wantTo) {
				t.Fatalf("twap_1h refreshed to %s, want %s", c.to, wantTo)
			}
			return
		}
	}
	t.Fatalf("twap_1h not refreshed: %v", viewsOf(f.snapshot()))
}

func TestLateTradeRefresh_ViewIsRateLimitedToItsSchedule(t *testing.T) {
	clock := &lateClock{t: lateTestNow}
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	r.now = clock.now
	day := 24 * time.Hour
	// 6 days back is past prices_1d's 7d - 6h - 1d - 5m coverage.
	r.ObserveTrades(canonical.Trade{Timestamp: lateTestNow.Add(-6 * day)})
	flushOK(t, r)
	if !slices.Contains(viewsOf(f.snapshot()), "prices_1d") {
		t.Fatalf("first flush skipped prices_1d: %v", viewsOf(f.snapshot()))
	}

	clock.advance(time.Minute)
	r.ObserveTrades(canonical.Trade{Timestamp: clock.now().Add(-6 * day)})
	n := len(f.snapshot())
	next := flushOK(t, r)
	second := viewsOf(f.snapshot()[n:])
	if slices.Contains(second, "prices_1d") || slices.Contains(second, "prices_1w") {
		t.Fatalf("a minute later prices_1d/1w were refreshed again: %v", second)
	}
	if !slices.Contains(second, "prices_1m") {
		t.Fatalf("prices_1m (30s schedule) was held: %v", second)
	}
	if next != 4*time.Minute {
		// prices_15m (5m schedule, refreshed 1m ago) falls due first.
		t.Fatalf("next = %s, want 4m", next)
	}

	clock.advance(6 * time.Hour)
	n = len(f.snapshot())
	flushOK(t, r)
	var got *refreshCall
	for _, c := range f.snapshot()[n:] {
		if c.view == "prices_1d" {
			got = &c
		}
	}
	if got == nil {
		t.Fatalf("prices_1d still held after its schedule: %v", viewsOf(f.snapshot()[n:]))
	}
	if second := lateTestNow.Add(time.Minute - 6*day); !got.to.After(second) {
		t.Fatalf("held prices_1d window [%s, %s) dropped the second trade at %s", got.from, got.to, second)
	}
}

func TestLateTradeRefresh_TwapsWaitWhilePrices1mIsHeld(t *testing.T) {
	clock := &lateClock{t: lateTestNow}
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	r.now = clock.now
	r.ObserveTrades(tradesAt(20 * time.Minute)...)
	flushOK(t, r) // prices_1m only

	clock.advance(10 * time.Second)
	r.ObserveTrades(tradesAt(6 * time.Hour)...)
	n := len(f.snapshot())
	flushOK(t, r)
	second := viewsOf(f.snapshot()[n:])
	if slices.Contains(second, "prices_1m") || slices.Contains(second, "twap_1h") {
		t.Fatalf("10s after prices_1m's refresh: refreshed %v; want prices_1m held and twap_1h waiting on it", second)
	}
	if !slices.Contains(second, "prices_15m") {
		t.Fatalf("prices_15m does not read prices_1m and must not wait: %v", second)
	}

	clock.advance(30 * time.Second)
	n = len(f.snapshot())
	flushOK(t, r)
	if got := viewsOf(f.snapshot()[n:]); len(got) != 2 || got[0] != "prices_1m" || got[1] != "twap_1h" {
		t.Fatalf("after prices_1m fell due: %v, want [prices_1m twap_1h]", got)
	}
}

func TestObservingSink_ObservesOnlyHandledEvents(t *testing.T) {
	r := newTestLateRefresher(&fakeLateCAGGStore{})
	ev := sdex.TradeEvent{Trade: tradesAt(time.Hour)[0]}
	failing := r.ObservingSink(func(context.Context, consumer.Event) error { return errors.New("down") })
	if err := failing(context.Background(), ev); err == nil {
		t.Fatal("sink error swallowed")
	}
	if r.anyPendingLocked() {
		t.Fatal("an event the sink failed was observed")
	}
	ok := r.ObservingSink(func(context.Context, consumer.Event) error { return nil })
	if err := ok(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if !r.pending || !r.lo.Equal(ev.Trade.Timestamp) {
		t.Fatalf("handled trade not observed: pending=%v lo=%s", r.pending, r.lo)
	}
	var nilR *LateTradeRefresher
	if nilR.ObservingSink(failing) == nil {
		t.Fatal("nil refresher dropped the sink")
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

// recordingStub checks, at insert time, that the late window was already recorded.
type recordingStub struct {
	stubTradeWriter
	f       *fakeLateCAGGStore
	sawRows int
}

func (s *recordingStub) InsertTrade(context.Context, canonical.Trade) error {
	s.sawRows = len(s.f.durableViews())
	return nil
}

func TestObservingTradeWriter_RecordsLateWindowBeforeTheInsert(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	inner := &recordingStub{f: f}
	w := observingTradeWriter{tradeWriter: inner, late: r}
	if err := w.InsertTrade(context.Background(), tradesAt(90 * time.Minute)[0]); err != nil {
		t.Fatal(err)
	}
	if inner.sawRows != len(timescale.TradesCAGGs) {
		t.Fatalf("insert ran with %d durable rows recorded, want one per trades cagg (%d)", inner.sawRows, len(timescale.TradesCAGGs))
	}
	got := f.durable["prices_1m"]
	if from, to := lateTestNow.Add(-2*time.Hour), lateTestNow.Add(-time.Hour); !got.From.Equal(from) || !got.To.Equal(to) {
		t.Errorf("recorded [%s, %s], want the trade's hour [%s, %s]", got.From, got.To, from, to)
	}

	f.recordErr = errors.New("down")
	inner.sawRows = -1
	if err := w.InsertTrade(context.Background(), tradesAt(5 * time.Hour)[0]); err == nil {
		t.Fatal("a failed record did not fail the write")
	}
	if inner.sawRows != -1 {
		t.Fatal("the insert ran without its window recorded")
	}
}

func TestObservingTradeWriter_SameHourSkipsTheUpsertUntilAFlush(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	w := observingTradeWriter{tradeWriter: stubTradeWriter{}, late: r}
	for _, age := range []time.Duration{90 * time.Minute, 100 * time.Minute, 0} {
		if err := w.InsertTrade(context.Background(), tradesAt(age)[0]); err != nil {
			t.Fatal(err)
		}
	}
	if f.records != 1 {
		t.Fatalf("%d upserts for late trades in one hour (and one on time), want 1", f.records)
	}
	flushOK(t, r)
	if err := w.InsertTrade(context.Background(), tradesAt(90 * time.Minute)[0]); err != nil {
		t.Fatal(err)
	}
	if f.records != 2 {
		t.Fatalf("%d upserts, want a second after the flush reset the cover", f.records)
	}
}

// A flush that refreshes and would clear while a recorded write is still
// uncommitted must keep the row: that write lands after the refresh ran.
func TestLateTradeRefresh_InFlightWriteKeepsItsDurableRow(t *testing.T) {
	f := &fakeLateCAGGStore{}
	r := newTestLateRefresher(f)
	end, err := r.beginWrite(context.Background(), tradesAt(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	flushOK(t, r)
	if len(f.snapshot()) == 0 {
		t.Fatal("the flush refreshed nothing over the recorded window")
	}
	end()
	if !slices.Contains(f.durableViews(), "prices_1m") {
		t.Fatal("the flush cleared prices_1m's row while the write it covers was in flight")
	}

	// Idle again: the next flush clears every refreshed or covered view.
	if _, err := r.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := f.durableViews(); len(got) != 0 {
		t.Fatalf("rows left after an idle flush: %v", got)
	}
}

func TestLateTradeRefresh_FailingViewKeepsOnlyItsDurableRow(t *testing.T) {
	f := &fakeLateCAGGStore{failView: "prices_15m"}
	r := newTestLateRefresher(f)
	w := observingTradeWriter{tradeWriter: stubTradeWriter{}, late: r}
	if err := w.InsertTrade(context.Background(), tradesAt(2 * time.Hour)[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.flush(context.Background(), true); err == nil {
		t.Fatal("prices_15m's failure was not reported")
	}
	if got := f.durableViews(); !slices.Equal(got, []string{"prices_15m"}) {
		t.Fatalf("durable rows after one view failed = %v, want only [prices_15m]", got)
	}
}

// A new process unions the rows its predecessor recorded and refreshes them.
func TestLateTradeRefresh_FlushRefreshesRowsRecordedBeforeARestart(t *testing.T) {
	f := &fakeLateCAGGStore{}
	w := observingTradeWriter{tradeWriter: stubTradeWriter{}, late: newTestLateRefresher(f)}
	if err := w.InsertTrade(context.Background(), tradesAt(2 * time.Hour)[0]); err != nil {
		t.Fatal(err)
	}
	fresh := newTestLateRefresher(f)
	flushOK(t, fresh)
	if calls := f.snapshot(); len(calls) == 0 || calls[0].view != "prices_1m" {
		t.Fatalf("restarted refresher refreshed %v, want prices_1m first", viewsOf(calls))
	}
	if got := f.durableViews(); len(got) != 0 {
		t.Fatalf("rows left after the restarted flush: %v", got)
	}
}
