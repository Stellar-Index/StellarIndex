//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The crash tests re-exec this test binary as a child indexer that writes
// a late trade, prints "committed" once the row is durable, and blocks.
// The parent SIGKILLs it, so no deferred flush runs, then starts a fresh
// refresher the way a restarted indexer does.
const lateCrashChildEnv = "LATE_REFRESH_CRASH_CHILD"

// TestLateTradeRefresh_SurvivesSIGKILL: a late trade committed through
// PersistEvents by a process that is then SIGKILLed before any refresh
// still reaches prices_1m once a new refresher starts.
func TestLateTradeRefresh_SurvivesSIGKILL(t *testing.T) {
	testLateTradeCrash(t, "persist", "KILL2720", "00000000000000000000000000000000000000000000000000000000000b0001")
}

// TestLateTradeRefresh_FlushDuringUncommittedWriteSurvivesSIGKILL: the
// window is recorded, a full flush (refresh, then clear) runs before the
// insert commits, the insert commits, and the process is SIGKILLed. The
// flush's refresh could not see the row, so its window must outlive it.
func TestLateTradeRefresh_FlushDuringUncommittedWriteSurvivesSIGKILL(t *testing.T) {
	testLateTradeCrash(t, "inflight", "RACE2720", "00000000000000000000000000000000000000000000000000000000000b0002")
}

func testLateTradeCrash(t *testing.T, mode, code, hash string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	lateTS := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLateTradeRefreshCrashChild$")
	cmd.Env = append(os.Environ(), lateCrashChildEnv+"="+mode, "LATE_REFRESH_DSN="+dsn,
		"LATE_REFRESH_CODE="+code, "LATE_REFRESH_HASH="+hash, "LATE_REFRESH_TS="+lateTS.Format(time.RFC3339))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	committed := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "committed" {
				committed <- true
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		committed <- false
	}()
	select {
	case ok := <-committed:
		if !ok {
			_ = cmd.Wait()
			t.Fatalf("child exited before committing; stderr:\n%s", stderr.String())
		}
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatalf("child never committed; stderr:\n%s", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL child: %v", err)
	}
	_ = cmd.Wait()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	late := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger, Debounce: time.Second})
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); late.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-runDone })

	pair := lateRefreshPair(t, code)
	if n := waitPrices1mTradeCount(t, ctx, store, pair, lateTS.Truncate(time.Minute), 30*time.Second); n != 1 {
		t.Errorf("trade_count = %d, want 1", n)
	}
	waitFor(t, 30*time.Second, func() bool { return lateWindowViews(t, ctx, store) == "" },
		"durable windows still pending after the restarted refresher's flush")
}

// TestLateTradeRefreshCrashChild is the child of testLateTradeCrash;
// skipped unless re-executed by it.
func TestLateTradeRefreshCrashChild(t *testing.T) {
	mode := os.Getenv(lateCrashChildEnv)
	if mode == "" {
		t.Skip("child process for testLateTradeCrash")
	}
	ctx := context.Background()
	store, err := timescale.Open(ctx, os.Getenv("LATE_REFRESH_DSN"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	ts, err := time.Parse(time.RFC3339, os.Getenv("LATE_REFRESH_TS"))
	if err != nil {
		t.Fatal(err)
	}
	hash := os.Getenv("LATE_REFRESH_HASH")
	ev := lateRefreshTrade(lateRefreshPair(t, os.Getenv("LATE_REFRESH_CODE")), ts, hash)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// An hour's debounce: Run never flushes before the kill.
	late := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger, Debounce: time.Hour})
	go late.Run(ctx)

	switch mode {
	case "persist":
		in := make(chan consumer.Event, 1)
		in <- ev
		go pipeline.PersistEvents(ctx, logger, store, in, pipeline.SinkModeAll, late)
	case "inflight":
		entered, proceed := make(chan struct{}), make(chan struct{})
		sink := late.ObservingSink(func(ctx context.Context, ev consumer.Event) error {
			close(entered)
			<-proceed
			return pipeline.HandleEvent(ctx, logger, store, ev)
		})
		errc := make(chan error, 1)
		go func() { errc <- sink(ctx, ev) }()
		<-entered
		late.FlushOnShutdown(ctx) // the whole flush, clear included, before the insert
		close(proceed)
		if err := <-errc; err != nil {
			t.Fatalf("sink: %v", err)
		}
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	waitFor(t, time.Minute, func() bool {
		var n int
		err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM trades WHERE tx_hash = $1`, hash).Scan(&n)
		return err == nil && n == 1
	}, "the child's trade never committed")
	fmt.Println("committed")
	select {}
}

// failingViewStore fails every refresh of one view.
type failingViewStore struct {
	*timescale.Store
	view string
}

func (s failingViewStore) RefreshContinuousAggregate(ctx context.Context, v string, from, to time.Time) error {
	if v == s.view {
		return errors.New("55P03: refresh already running")
	}
	return s.Store.RefreshContinuousAggregate(ctx, v, from, to)
}

// TestLateTradeRefresh_FailingViewDoesNotPinTheOthers: one view's failed
// refresh keeps only that view's durable window; every other view's is
// cleared once refreshed.
func TestLateTradeRefresh_FailingViewDoesNotPinTheOthers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	late := pipeline.NewLateTradeRefresher(failingViewStore{Store: store, view: "prices_15m"},
		pipeline.LateTradeRefresherOptions{Logger: logger})
	pair := lateRefreshPair(t, "PIN2720")
	lateTS := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	sink := late.ObservingSink(func(ctx context.Context, ev consumer.Event) error {
		return pipeline.HandleEvent(ctx, logger, store, ev)
	})
	if err := sink(ctx, lateRefreshTrade(pair, lateTS, "00000000000000000000000000000000000000000000000000000000000b0003")); err != nil {
		t.Fatalf("sink: %v", err)
	}
	late.FlushOnShutdown(ctx)

	if got := lateWindowViews(t, ctx, store); got != "prices_15m" {
		t.Errorf("pending durable windows = %q, want only the failing view prices_15m", got)
	}
	if n := waitPrices1mTradeCount(t, ctx, store, pair, lateTS.Truncate(time.Minute), time.Second); n != 1 {
		t.Errorf("trade_count = %d, want 1", n)
	}
}

// lateWindowViews lists the pending trades windows' views, comma-separated.
func lateWindowViews(t *testing.T, ctx context.Context, store *timescale.Store) string {
	t.Helper()
	var views string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(string_agg(view, ',' ORDER BY view), '') FROM cagg_late_refresh_windows WHERE family = 'trades'`,
	).Scan(&views); err != nil {
		t.Fatalf("read cagg_late_refresh_windows: %v", err)
	}
	return views
}

func waitPrices1mTradeCount(t *testing.T, ctx context.Context, store *timescale.Store, pair c.Pair, bucket time.Time, within time.Duration) int {
	t.Helper()
	var n int
	var err error
	waitFor(t, within, func() bool {
		err = store.DB().QueryRowContext(ctx,
			`SELECT trade_count FROM prices_1m WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
			pair.Base.String(), pair.Quote.String(), bucket).Scan(&n)
		return err == nil
	}, "no prices_1m row for the late trade")
	return n
}

func waitFor(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s after %s", msg, within)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

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

// TestLateTradeRefresh_MaterialisesTradeOlderThanPolicyLookback proves the
// live write path (PersistEvents + LateTradeRefresher, as the indexer wires
// them) puts a trade written ~2h late into prices_1m with no manual
// refresh_continuous_aggregate. prices_1m's policy only reaches back 15
// minutes, so before the refresher such a row was never materialised.
func TestLateTradeRefresh_MaterialisesTradeOlderThanPolicyLookback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn) // policy jobs stay defined but unscheduled

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	latePair, onTimePair := lateRefreshPair(t, "LATE2492"), lateRefreshPair(t, "ONTIME2492")

	lateTS := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	bucket := lateTS.Truncate(time.Minute)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	late := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger, Debounce: time.Second})
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); late.Run(runCtx) }()

	in := make(chan consumer.Event, 2)
	in <- lateRefreshTrade(onTimePair, time.Now().UTC().Truncate(time.Second), "00000000000000000000000000000000000000000000000000000000000a0001")
	in <- lateRefreshTrade(latePair, lateTS, "00000000000000000000000000000000000000000000000000000000000a0002")
	sinkDone := make(chan struct{})
	go func() {
		defer close(sinkDone)
		pipeline.PersistEvents(ctx, logger, store, in, pipeline.SinkModeAll, late)
	}()
	t.Cleanup(func() {
		close(in)
		<-sinkDone
		stop()
		<-runDone
	})

	// Run's first flush waits out the one-second debounce.
	const q = `SELECT vwap::text, volume::text, trade_count FROM prices_1m
	            WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`
	var vwap, volume string
	var count int
	deadline := time.Now().Add(90 * time.Second)
	for {
		err := store.DB().QueryRowContext(ctx, q, latePair.Base.String(), latePair.Quote.String(), bucket).Scan(&vwap, &volume, &count)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no prices_1m row for the late trade after 90s (last err: %v)", err)
		}
		time.Sleep(2 * time.Second)
	}

	wantVWAP := new(big.Rat).SetFrac64(12_000_000, 1_000_000_000)
	gotVWAP, ok := new(big.Rat).SetString(vwap)
	if !ok || gotVWAP.Cmp(wantVWAP) != 0 {
		t.Errorf("vwap = %s, want exactly 0.012", vwap)
	}
	if volume != "1000000000" {
		t.Errorf("volume = %s, want 1000000000", volume)
	}
	if count != 1 {
		t.Errorf("trade_count = %d, want 1", count)
	}

	// The on-time trade is inside the policy's reach, so the refresher
	// must leave it to the policy (unscheduled here): nothing materialised.
	var onTimeRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1m WHERE base_asset = $1`, onTimePair.Base.String()).Scan(&onTimeRows); err != nil {
		t.Fatalf("count on-time rows: %v", err)
	}
	if onTimeRows != 0 {
		t.Errorf("on-time trade has %d prices_1m rows; the refresher must not refresh it", onTimeRows)
	}
}

// TestLateTradeRefresh_ShutdownFlushMaterialisesDrainWrites: a late trade
// written after Run stopped (the sink's shutdown drain) reaches prices_1m
// through FlushOnShutdown, as the indexer calls it once both writers stop.
func TestLateTradeRefresh_ShutdownFlushMaterialisesDrainWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	pair := lateRefreshPair(t, "DRAIN2492")
	lateTS := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	late := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger})
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); late.Run(runCtx) }()
	stop()
	<-runDone

	in := make(chan consumer.Event, 1)
	in <- lateRefreshTrade(pair, lateTS, "00000000000000000000000000000000000000000000000000000000000a0003")
	close(in)
	pipeline.PersistEvents(ctx, logger, store, in, pipeline.SinkModeAll, late)

	flushCtx, flushCancel := context.WithTimeout(ctx, pipeline.LateTradeShutdownFlushBudget)
	late.FlushOnShutdown(flushCtx)
	flushCancel()

	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT trade_count FROM prices_1m WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`,
		pair.Base.String(), pair.Quote.String(), lateTS.Truncate(time.Minute)).Scan(&count); err != nil {
		t.Fatalf("no prices_1m row for the drained late trade after the shutdown flush: %v", err)
	}
	if count != 1 {
		t.Errorf("trade_count = %d, want 1", count)
	}
}

func lateRefreshPair(t *testing.T, code string) c.Pair {
	t.Helper()
	a, err := c.NewClassicAsset(code, priceableIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	p, err := c.NewPair(a, c.NativeAsset())
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func lateRefreshTrade(p c.Pair, ts time.Time, hash string) sdex.TradeEvent {
	return sdex.TradeEvent{Trade: c.Trade{
		Source:      "test-late-refresh",
		Ledger:      71_000_000,
		TxHash:      hash,
		Timestamp:   ts,
		Pair:        p,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
	}}
}
