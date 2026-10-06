//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingPrices1mStore holds its first prices_1m refresh until released.
type blockingPrices1mStore struct {
	*timescale.Store
	entered, release chan struct{}
}

func (s *blockingPrices1mStore) RefreshContinuousAggregate(ctx context.Context, v string, from, to time.Time) error {
	if v == "prices_1m" && s.entered != nil {
		close(s.entered)
		s.entered = nil
		<-s.release
	}
	return s.Store.RefreshContinuousAggregate(ctx, v, from, to)
}

// TestLateTradeRefresh_ClearIsGenGuarded: a window widened while a flush's
// refresh runs survives that flush's clear, even when the widen comes from
// another process the flush's in-flight count cannot see.
func TestLateTradeRefresh_ClearIsGenGuarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Store contract: a widen moves gen and keeps first_seen.
	const fam = timescale.CAGGLateFamilyTrades
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	views := []string{"probe_view"}
	if err := store.RecordCAGGLateRefreshWindow(ctx, fam, views, t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	first := lateWindow(t, ctx, store, "probe_view")
	if err := store.RecordCAGGLateRefreshWindow(ctx, fam, views, t0.Add(-3*time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.ClearCAGGLateRefreshWindow(ctx, fam, "probe_view", first.Gen); err != nil || deleted {
		t.Fatalf("clear at the pre-widen gen: deleted=%v err=%v, want the row kept", deleted, err)
	}
	widened := lateWindow(t, ctx, store, "probe_view")
	if !widened.From.Equal(t0.Add(-3*time.Hour)) || !widened.To.Equal(t0.Add(time.Hour)) || !widened.FirstSeen.Equal(first.FirstSeen) {
		t.Fatalf("widened row = %+v, want [%s, %s] with first_seen %s", widened, t0.Add(-3*time.Hour), t0.Add(time.Hour), first.FirstSeen)
	}
	if deleted, err := store.ClearCAGGLateRefreshWindow(ctx, fam, "probe_view", widened.Gen); err != nil || !deleted {
		t.Fatalf("clear at the current gen: deleted=%v err=%v", deleted, err)
	}

	// Refresher: A's flush blocks inside its prices_1m refresh while a
	// second refresher (another process) records and commits B.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sinkFor := func(r *pipeline.LateTradeRefresher) func(context.Context, consumer.Event) error {
		return r.ObservingSink(func(ctx context.Context, ev consumer.Event) error {
			return pipeline.HandleEvent(ctx, logger, store, ev)
		})
	}
	blocking := &blockingPrices1mStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	entered := blocking.entered
	a := pipeline.NewLateTradeRefresher(blocking, pipeline.LateTradeRefresherOptions{Logger: logger})
	pairA, pairB := lateRefreshPair(t, "GENA2720"), lateRefreshPair(t, "GENB2720")
	tsA := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	tsB := time.Now().UTC().Add(-8 * time.Hour).Truncate(time.Second)
	if err := sinkFor(a)(ctx, lateRefreshTrade(pairA, tsA, "00000000000000000000000000000000000000000000000000000000000b0004")); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan struct{})
	go func() { defer close(flushed); a.FlushOnShutdown(ctx) }()
	<-entered
	b := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger})
	if err := sinkFor(b)(ctx, lateRefreshTrade(pairB, tsB, "00000000000000000000000000000000000000000000000000000000000b0005")); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	<-flushed

	if w := lateWindow(t, ctx, store, "prices_1m"); w.From.After(tsB) {
		t.Fatalf("prices_1m window after A's flush = [%s, %s]; B at %s was dropped by a stale-gen clear", w.From, w.To, tsB)
	}

	// The window B's write left is what a restarted process refreshes.
	fresh := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger})
	fresh.FlushOnShutdown(ctx)
	if n := waitPrices1mTradeCount(t, ctx, store, pairB, tsB.Truncate(time.Minute), time.Second); n != 1 {
		t.Errorf("B trade_count = %d, want 1", n)
	}
	if got := lateWindowViews(t, ctx, store); got != "" {
		t.Errorf("windows left after the idle flush: %q", got)
	}
}

func lateWindow(t *testing.T, ctx context.Context, store *timescale.Store, view string) timescale.CAGGLateRefreshWindow {
	t.Helper()
	ws, err := store.CAGGLateRefreshWindows(ctx, timescale.CAGGLateFamilyTrades)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.View == view {
			return w
		}
	}
	t.Fatalf("no pending window for %s in %+v", view, ws)
	return timescale.CAGGLateRefreshWindow{}
}
