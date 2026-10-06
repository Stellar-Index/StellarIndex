//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
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
