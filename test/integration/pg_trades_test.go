//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBatchInsertTrades_OneSideZeroFillIsStored pins that an SDEX
// one-side-zero fill (a leg that rounded to 0 stroops) is stored by the batch
// writer, one row each, beside the good trades of its batch, and counted on
// stellarindex_trades_zero_leg_admitted_total. A both-zero row is still
// malformed: it is dropped before the all-or-nothing INSERT (so it cannot
// sink the batch) and counted as an insert error.
func TestBatchInsertTrades_OneSideZeroFillIsStored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mkTrade := func(ledger uint32, base, quote int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      ledger,
			TxHash:      fmt.Sprintf("%064x", ledger),
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(base)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
		}
	}

	// Three ordinary trades, a zero-quote and a zero-base fill, and one
	// both-zero row, interleaved so no special row is conveniently first or last.
	good := []c.Trade{
		mkTrade(60_000_000, 1_000_000_000, 12_000_000),
		mkTrade(60_000_001, 2_000_000_000, 24_000_000),
		mkTrade(60_000_002, 3_000_000_000, 36_000_000),
	}
	zeroQuote := mkTrade(60_000_003, 5_000_000_000, 0)
	zeroBase := mkTrade(60_000_004, 0, 7_000_000)
	bothZero := mkTrade(60_000_005, 0, 0)
	batch := []c.Trade{good[0], zeroQuote, good[1], bothZero, zeroBase, good[2]}

	errKind := obs.InsertErrorKindTradeDropped
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", errKind))

	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}

	stored := append(append([]c.Trade{}, good...), zeroQuote, zeroBase)
	if n := countTrades(t, store, "sdex"); n != len(stored) {
		t.Fatalf("stored sdex trades = %d, want %d (3 good + 2 one-side-zero)", n, len(stored))
	}
	for _, g := range stored {
		if !tradeExists(t, store, g.Source, g.TxHash, g.OpIndex) {
			t.Errorf("trade ledger=%d base=%s quote=%s missing", g.Ledger, g.BaseAmount, g.QuoteAmount)
		}
	}
	if tradeExists(t, store, bothZero.Source, bothZero.TxHash, bothZero.OpIndex) {
		t.Errorf("both-zero row tx=%s was stored; Validate must reject it", bothZero.TxHash)
	}

	zeroDelta := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex")) - zeroBefore
	if zeroDelta != 2 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 2 (one per stored one-side-zero fill)", zeroDelta)
	}
	t.Logf("TradesZeroLegAdmittedTotal{sdex} delta = %v", zeroDelta)
	if d := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", errKind)) - errBefore; d != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,%s} delta = %v, want 1 (the both-zero row only)", errKind, d)
	}
}

// countTrades returns the number of rows in `trades` for a source.
// tradeExists reports whether a specific (source, tx_hash, op_index) trade row
// is present.
func tradeExists(t *testing.T, store *timescale.Store, source, txHash string, opIndex uint32) bool {
	t.Helper()
	const q = `SELECT COUNT(*) FROM trades WHERE source = $1 AND tx_hash = $2 AND op_index = $3`
	var n int
	if err := store.DB().QueryRow(q, source, txHash, opIndex).Scan(&n); err != nil {
		t.Fatalf("tradeExists %s/%s/%d: %v", source, txHash, opIndex, err)
	}
	return n > 0
}

// TestBatchInsertTrades_PopulatesClassicAssetRegistry proves that
// the LIVE indexer, which ingests trades EXCLUSIVELY through BatchInsertTrades
// (persistWorker → batch path), runs the classic-asset / issuer registry hook
// that the single-row InsertTrade path runs. Otherwise classic_assets +
// issuers are permanently under-populated for every batch-ingested asset.
//
// This test drives a classic-asset trade through BOTH paths and asserts the
// registry lands identically; the batch-path assertions
// (classic_assets row present, issuers row present) fail with "no rows" if
// the registry hook never runs on the batch path.
// the registry hook never ran on the batch path.
func TestBatchInsertTrades_PopulatesClassicAssetRegistry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerG = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerG)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	store.ResetAssetRegistryDedupeForTest()
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	mkTrade := func(ledger uint32, txTail rune) c.Trade {
		txHash := "beadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfeedbeadfee" + string(txTail)
		return c.Trade{
			Source:      "test-batch-registry",
			Ledger:      ledger,
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}

	// Batch-insert two distinct trades on the same classic asset in one call.
	batch := []c.Trade{
		mkTrade(60_000_000, 'a'),
		mkTrade(60_000_001, 'b'),
	}
	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}

	// classic_assets must now carry the asset: without the fix this
	// row did not exist and readRegistry fatals with "no rows".
	gotCount, gotLastLedger := readRegistry(t, store, usdc.String())
	if gotCount == 0 {
		t.Fatalf("classic_assets observation_count = 0, want >= 1 — batch path skipped the registry hook (C2-13b regression)")
	}
	// The two distinct landed trades are deduped to ONE registry upsert per
	// asset within the batch (dedupe-cached), advancing last_seen to the
	// highest ledger observed.
	if gotLastLedger != 60_000_001 {
		t.Errorf("classic_assets last_seen_ledger = %d, want 60_000_001 (highest landed ledger)", gotLastLedger)
	}

	// issuers must carry the issuer G-strkey too.
	if n := countIssuers(t, store, issuerG); n != 1 {
		t.Errorf("issuers rows for %s = %d, want 1 — batch path skipped registerIssuerSeen", issuerG, n)
	}

	// A replay of the SAME batch (cold dedupe cache = simulated restart) must
	// NOT inflate observation_count: the hook only fires for genuinely-landed
	// (xmax=0) rows, matching the single-row path's guard.
	store.ResetAssetRegistryDedupeForTest()
	if err := store.BatchInsertTrades(ctx, batch); err != nil {
		t.Fatalf("BatchInsertTrades (replay): %v", err)
	}
	replayCount, _ := readRegistry(t, store, usdc.String())
	if replayCount != gotCount {
		t.Errorf("after replay: observation_count = %d, want %d (duplicate batch must not advance the registry)", replayCount, gotCount)
	}
}

// countIssuers returns the number of issuers rows for the given G-strkey.
func countIssuers(t *testing.T, store *timescale.Store, gStrkey string) int {
	t.Helper()
	const q = `SELECT COUNT(*) FROM issuers WHERE g_strkey = $1`
	var n int
	if err := store.DB().QueryRow(q, gStrkey).Scan(&n); err != nil {
		t.Fatalf("countIssuers %s: %v", gStrkey, err)
	}
	return n
}

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

// TestLatestTradeReadsBreakSameLedgerTiesOnTheFullKey: several of one
// source's trades share (ts, ledger) whenever a ledger fills more than
// one offer, so both latest-trade readers must break that tie on
// (tx_hash, op_index) — the key /v1/history orders on — and agree with
// the newest row TradesInRange serves. The winner is inserted last in
// each stored direction, so a read that stops at (ts, ledger) meets a
// loser first.
func TestLatestTradeReadsBreakSameLedgerTiesOnTheFullKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	// Prepared statements are per-session: the plan check below reads
	// the one the reader prepared.
	db.SetMaxOpenConns(1)

	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	aqua, err := c.NewClassicAsset("AQUA", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usdc, err := c.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(aqua, usdc)
	if err != nil {
		t.Fatal(err)
	}
	a, b := aqua.String(), usdc.String()
	ts := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	txA := strings.Repeat("a", 64)
	txC := strings.Repeat("c", 64)

	// Older history from another source, so the planner has a market
	// worth skip-scanning.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
		                    base_asset, quote_asset, base_amount, quote_amount)
		SELECT 'bulk', 60000000 + h, lpad(to_hex(h), 64, '0'), 0,
		       $1::timestamptz - make_interval(mins => h), $2, $3, 1, 2
		  FROM generate_series(1, 60000) h`, ts, a, b); err != nil {
		t.Fatalf("seed bulk: %v", err)
	}
	for _, r := range []struct {
		base, quote, tx string
		op              int
	}{
		{a, b, txA, 0},
		{b, a, txA, 1},
		{a, b, txC, 0},
		{b, a, txC, 2}, // the winner: highest tx_hash, then op_index
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
			                    base_asset, quote_asset, base_amount, quote_amount)
			VALUES ('sdex', 61000000, $1, $2, $3, $4, $5, 5, 7)`,
			r.tx, r.op, ts, r.base, r.quote); err != nil {
			t.Fatalf("seed tie: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `ANALYZE trades`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	isWinner := func(tr c.Trade) bool {
		return tr.Source == "sdex" && tr.TxHash == txC && tr.OpIndex == 2
	}

	hist, err := store.TradesInRange(ctx, pair, ts.Add(-time.Hour), ts.Add(time.Hour), 1000)
	if err != nil || len(hist) == 0 {
		t.Fatalf("TradesInRange: %d rows, %v", len(hist), err)
	}
	if newest := hist[len(hist)-1]; !isWinner(newest) {
		t.Fatalf("instrument check: /v1/history's newest row is %s/%d, want %s/2", newest.TxHash, newest.OpIndex, txC)
	}

	last, err := store.LatestTradesForPair(ctx, pair, 1)
	if err != nil || len(last) != 1 {
		t.Fatalf("LatestTradesForPair: %d rows, %v", len(last), err)
	}
	if !isWinner(last[0]) {
		t.Errorf("LatestTradesForPair = %s/%d, want %s/2 — the newest row /v1/history serves",
			last[0].TxHash, last[0].OpIndex, txC)
	}

	perSource, err := store.LatestTradePerSource(ctx, pair, "")
	if err != nil {
		t.Fatalf("LatestTradePerSource: %v", err)
	}
	var sdex *c.Trade
	for i := range perSource {
		if perSource[i].Source == "sdex" {
			sdex = &perSource[i]
		}
	}
	if len(perSource) != 2 || sdex == nil {
		t.Fatalf("LatestTradePerSource returned %d rows (sdex present: %v), want bulk + sdex", len(perSource), sdex != nil)
	}
	if !isWinner(*sdex) {
		t.Errorf("LatestTradePerSource[sdex] = %s/%d, want %s/2 — the newest row /v1/history serves",
			sdex.TxHash, sdex.OpIndex, txC)
	}

	// The tiebreak must not cost the per-source read its skip scan: the
	// index ends at ledger, so ordering the DISTINCT ON by more keys
	// sorts the whole market instead.
	stmt := capturePreparedStatement(t, ctx, db, nil, "DISTINCT ON (source)", "FROM trades")
	mustExecPlan(t, ctx, db, `SET plan_cache_mode = force_custom_plan`)
	mustExecPlan(t, ctx, db, `PREPARE tiebreak_plan_probe AS `+stmt)
	rows, err := db.QueryContext(ctx, `EXPLAIN EXECUTE tiebreak_plan_probe('`+a+`', '`+b+`', '', 'binance,bitstamp,coinbase,kraken')`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "SkipScan") {
		t.Errorf("LatestTradePerSource lost its skip scan — its cost is now O(rows in market):\n%s", plan.String())
	}
}

// TestTradeInsertOutcome_NewVsDuplicate pins the diagnostic metric
// `stellarindex_trade_insert_outcome_total{source, outcome}`: a
// fresh trade increments outcome=new, a re-insertion (same PK)
// increments outcome=duplicate. The older counter (trade_inserts_total) can
// climb at 157/min while the trades hypertable's max(ts) is 11 h old — every
// attempt an ON CONFLICT DO NOTHING short-circuit from a stuck cursor. This
// metric makes that failure mode observable.
func TestTradeInsertOutcome_NewVsDuplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("USDC: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	const src = "test-outcome-metric"
	trade := c.Trade{
		Source:      src,
		Ledger:      62_900_000,
		TxHash:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 5, 28, 1, 0, 0, 0, time.UTC),
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: c.NewAmount(big.NewInt(150_000_000)),
	}

	beforeNew := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "new"))
	beforeDup := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "duplicate"))

	// First insert — must take the `new` branch.
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade #1: %v", err)
	}
	gotNew := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "new")) - beforeNew
	gotDup := testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "duplicate")) - beforeDup
	if gotNew != 1 {
		t.Errorf("after fresh insert: outcome=new delta = %v, want 1", gotNew)
	}
	if gotDup != 0 {
		t.Errorf("after fresh insert: outcome=duplicate delta = %v, want 0", gotDup)
	}

	// Replay the exact same trade — must take the `duplicate` branch.
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade #2 (replay): %v", err)
	}
	gotNew = testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "new")) - beforeNew
	gotDup = testutil.ToFloat64(obs.TradeInsertOutcomeTotal.WithLabelValues(src, "duplicate")) - beforeDup
	if gotNew != 1 {
		t.Errorf("after exact replay: outcome=new delta = %v, want still 1", gotNew)
	}
	if gotDup != 1 {
		t.Errorf("after exact replay: outcome=duplicate delta = %v, want 1", gotDup)
	}
}

// TestBulkBackfillTrades_Throughput measures the two trade writers against a
// PROD-SHAPED target and prints both rates. It is the measurement behind the
// -bulk-trades flag; a claimed speed-up with no number is worth nothing here.
//
// What "prod-shaped" means, and what it still is not:
//
//   - The target chunks are POPULATED and COMPRESSED. `trades` compresses at 7
//     days (migration 0001), so every chunk a historical backfill lands in is
//     compressed, and TimescaleDB pays a large penalty enforcing the trade PK
//     against compressed data. Measured on this harness in isolation: landing
//     rows in a compressed chunk runs at roughly a third of the rate of the
//     same rows into a fresh one. Skipping this step flatters both writers and
//     changes their ratio.
//   - The USD-volume resolvers are installed, exactly as ch_rebuild.go
//     installs them, so both writers pay real `tradeUSDVolume` resolution.
//   - It UNDERSTATES the bulk writer. The dominant per-row cost on r1 is FX
//     resolver latency against a 69 GB `prices_1m`; here `prices_1m` is nearly
//     empty, so each lookup is about as cheap as a round trip can be. The half
//     of this change that parallelises those lookups therefore has far less to
//     recover on a laptop than it does on the real box.
//   - Docker Desktop is CPU-bound well below r1. Parallel writers saturate
//     earlier here than they will there.
//
// The assertion is deliberately weak — "not slower" — because a tight ratio
// gate on shared CI hardware fails for reasons that have nothing to do with
// this code. The NUMBERS are the deliverable; read them in the log.
func TestBulkBackfillTrades_Throughput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// ── build the prod-shaped target ────────────────────────────────────
	// Half a million rows of another source across the days the backfill
	// targets, then compress. The backfilled source itself stays absent —
	// that is the r1 situation: `trades` holds 836M rows and zero sdex rows
	// below the trade floor.
	const seedRows = 500_000
	seedStart := time.Now()
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`
        INSERT INTO trades (source, ledger, tx_hash, op_index, ts,
                            base_asset, quote_asset, base_amount, quote_amount,
                            usd_volume, derive_generation)
        SELECT 'binance', 40000000 + (g/48)::int, lpad(to_hex(g), 64, '0'), (g %% 48)::int,
               timestamptz '2024-03-01 00:00:00Z' + ((g/48) * interval '5 seconds'),
               'TK' || lpad(((g*7) %% 200)::text, 2, '0') || '-%[1]s',
               'TK' || lpad(((g*13) %% 200)::text, 2, '0') || '-%[1]s',
               1000000 + g, 2000000 + g*3, NULL, 0
          FROM generate_series(1, %[2]d) g`, bulkPegIssuer, seedRows)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var nCompressed int
	if err := db.QueryRowContext(ctx, `
        SELECT count(*) FROM (
          SELECT compress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass)
            FROM timescaledb_information.chunks
           WHERE hypertable_name = 'trades') z`).Scan(&nCompressed); err != nil {
		t.Fatalf("compress: %v", err)
	}
	t.Logf("target prepared: %d seed rows, %d compressed chunks, in %s",
		seedRows, nCompressed, time.Since(seedStart).Round(time.Millisecond))
	if nCompressed == 0 {
		t.Fatal("no chunk got compressed — the benchmark would measure the easy case and " +
			"report a ratio prod cannot reproduce")
	}

	const gen = 1_700_000_000
	const n = 40_000
	store := bulkStore(t, ctx, dsn, gen)

	// Both writers land into COMPRESSED, populated chunks (the seed spans
	// 2024/03/01 .. 2024/03/30 at 5s/ledger), in disjoint ledger windows.
	const upsertLo = 50_000_000
	const bulkLo = 55_000_000
	upsertRows := bulkTradeSet(t, n, upsertLo, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), "sdex")
	bulkRows := bulkTradeSet(t, n, bulkLo, time.Date(2024, 3, 12, 0, 0, 0, 0, time.UTC), "sdex")

	// ── OLD PATH: exactly what drainAndWrite does today — 1000-row batches
	// through the generation-guarded upsert, one after another.
	oldStart := time.Now()
	for i := 0; i < len(upsertRows); i += 1000 {
		end := min(i+1000, len(upsertRows))
		if err := store.BatchInsertTrades(ctx, upsertRows[i:end]); err != nil {
			t.Fatalf("BatchInsertTrades: %v", err)
		}
	}
	oldDur := time.Since(oldStart)

	// ── NEW PATH: one bulk call over the whole buffer.
	newStart := time.Now()
	res, err := store.BulkBackfillTrades(ctx, bulkRows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	newDur := time.Since(newStart)
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("bulk run fell back to %q (%s) — this would be measuring the OLD path twice",
			res.Path, res.FallbackReason)
	}

	oldRate := float64(n) / oldDur.Seconds()
	newRate := float64(n) / newDur.Seconds()
	t.Logf("OLD  BatchInsertTrades (1000-row upsert batches): %6d rows in %-9s = %8.0f rows/sec",
		n, oldDur.Round(time.Millisecond), oldRate)
	t.Logf("NEW  BulkBackfillTrades (COPY, parallel):         %6d rows in %-9s = %8.0f rows/sec",
		n, newDur.Round(time.Millisecond), newRate)
	t.Logf("speed-up: %.2fx", newRate/oldRate)

	// Both writers must actually have landed everything they were given.
	if got := len(readTradeRows(t, store.DB(), "sdex", upsertLo, upsertLo+999_999)); got != n {
		t.Fatalf("upsert path stored %d rows, want %d", got, n)
	}
	if got := len(readTradeRows(t, store.DB(), "sdex", bulkLo, bulkLo+999_999)); got != n {
		t.Fatalf("bulk path stored %d rows, want %d", got, n)
	}
	if newRate < oldRate {
		t.Fatalf("the bulk path is SLOWER than the batch upsert (%.0f vs %.0f rows/sec) — "+
			"the flag exists to be faster; do not ship it on the strength of the design alone",
			newRate, oldRate)
	}
}

// bulkTradeSetTS is unused by the suite but kept next to the benchmark as the
// documented shape of a realistic SDEX buffer, so a future measurement does
// not have to re-derive it.
var _ = func() []c.Trade { return nil }

// The `trades` bulk backfill writer (timescale.Store.BulkBackfillTrades) is
// the opt-in path `stellarindex-ops ch-rebuild -sdex -write -bulk-trades`
// uses. These tests prove the three things that make it safe to prefer over
// the live writer on a historical range:
//
//  1. it produces the SAME ROWS as Store.BatchInsertTrades on the same input
//     (identical values in every column either path writes, plus the same
//     source_entry_counts tally and classic-asset registry effect);
//  2. it REFUSES its own precondition — a range that already holds rows for
//     the source falls back to the upsert instead of COPYing into a conflict;
//  3. it is faster, measured, on a realistically-shaped target.
//
// PRODUCTION CONSTRUCTOR. Both paths are reached through timescale.Open +
// timescale.InstallUSDVolumeResolution + Store.SetDeriveGeneration — exactly
// what internal/ops/chops/ch_rebuild.go does before it drains (ch_rebuild.go
// "storage open" / "usd_volume resolution — MANDATORY on this path"). These
// tests use that constructor, not a hand-built Store, so a wiring change that
// would break the real tool breaks them too.

const bulkPegIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// bulkStore opens a store wired the way ch-rebuild wires it.
func bulkStore(t *testing.T, ctx context.Context, dsn string, gen int64) *timescale.Store {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetDeriveGeneration(gen)
	if err := timescale.InstallUSDVolumeResolution(store,
		[]string{"USDC-" + bulkPegIssuer}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	return store
}

func bulkAsset(t *testing.T, code string) c.Asset {
	t.Helper()
	a, err := c.NewClassicAsset(code, bulkPegIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(%s): %v", code, err)
	}
	return a
}

// bulkTrades builds n SDEX-shaped trades. The pair mix matters: a USDC quote
// resolves through the declared peg with no DB lookup at all, while the other
// two shapes fall through to the FX resolver — which is where a historical
// backfill actually spends its time.
func bulkTradeSet(t *testing.T, n int, baseLedger uint32, baseTS time.Time, source string) []c.Trade {
	t.Helper()
	const nTokens = 200
	usdc := bulkAsset(t, "USDC")
	tokens := make([]c.Asset, nTokens)
	for i := range tokens {
		tokens[i] = bulkAsset(t, fmt.Sprintf("TK%02d", i))
	}
	out := make([]c.Trade, 0, n)
	for i := range n {
		var base, quote c.Asset
		switch i % 20 {
		case 0, 1, 2, 3, 4, 5, 6, 7, 8:
			base, quote = c.NativeAsset(), tokens[i%nTokens]
		case 9, 10, 11, 12, 13, 14, 15:
			base, quote = tokens[i%nTokens], tokens[(i*7+3)%nTokens]
		default:
			base, quote = tokens[i%nTokens], usdc
		}
		pair, err := c.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		out = append(out, c.Trade{
			Source:      source,
			Ledger:      baseLedger + uint32(i/48), //nolint:gosec // bounded test data
			TxHash:      fmt.Sprintf("%064x", uint64(baseLedger)*1_000_000+uint64(i)),
			OpIndex:     uint32(i % 48), //nolint:gosec // bounded test data
			Timestamp:   baseTS.Add(time.Duration(i/48) * 5 * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(int64(1_000_000 + i))),
			QuoteAmount: c.NewAmount(big.NewInt(int64(2_000_000 + i*3))),
			Maker:       "",
			Taker:       fmt.Sprintf("TAKER-%d", i%7),
		})
	}
	return out
}

// storedTradeRow is every column either write path is responsible for, plus
// the two columns NEITHER writes (routed_via, signer) so the test would catch
// a bulk path that started writing them.
type storedTradeRow struct {
	source, txHash          string
	ledger, opIndex         int64
	ts                      time.Time
	baseAsset, quoteAsset   string
	baseAmount, quoteAmount string
	usdVolume               sql.NullString
	maker, taker            sql.NullString
	deriveGeneration        int64
	routedVia, signer       sql.NullString
}

func readTradeRows(t *testing.T, db *sql.DB, source string, loLedger, hiLedger uint32) []storedTradeRow {
	t.Helper()
	rows, err := db.Query(`
        SELECT source, ledger, tx_hash, op_index, ts,
               base_asset, quote_asset, base_amount::text, quote_amount::text,
               usd_volume::text, maker, taker, derive_generation, routed_via, signer
          FROM trades
         WHERE source = $1::text
           AND ledger BETWEEN $2::int AND $3::int
         ORDER BY source, ledger, tx_hash, op_index, ts`, source, int64(loLedger), int64(hiLedger))
	if err != nil {
		t.Fatalf("read trades: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []storedTradeRow
	for rows.Next() {
		var r storedTradeRow
		if err := rows.Scan(&r.source, &r.ledger, &r.txHash, &r.opIndex, &r.ts,
			&r.baseAsset, &r.quoteAsset, &r.baseAmount, &r.quoteAmount,
			&r.usdVolume, &r.maker, &r.taker, &r.deriveGeneration,
			&r.routedVia, &r.signer); err != nil {
			t.Fatalf("scan trade: %v", err)
		}
		r.ts = r.ts.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func sourceEntryCount(t *testing.T, db *sql.DB, source string) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRow(
		`SELECT entry_count FROM source_entry_counts WHERE source = $1::text`, source).Scan(&n); err != nil {
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("source_entry_counts: %v", err)
	}
	return n.Int64
}

// TestBulkBackfillTrades_IdenticalToBatchUpsert writes the SAME input through
// both writers, into two ranges that differ only by the ledger/ts offset, and
// compares every stored column. A divergence here means the bulk path is not
// a drop-in for the upsert on an empty range.
func TestBulkBackfillTrades_IdenticalToBatchUpsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	const gen = 1_700_000_000
	const n = 4_000
	store := bulkStore(t, ctx, dsn, gen)
	db := store.DB()

	upsertTS := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	bulkTS := time.Date(2024, 5, 5, 12, 0, 0, 0, time.UTC)

	// Same logical rows, two disjoint identities. Both sets carry a one-side-
	// zero fill (unstorable) and an exact intra-batch PK duplicate, so
	// the comparison covers the storability gate and the dedupe collapse too,
	// not just the happy path.
	// BOTH sets are source "sdex". A fabricated source name would make this
	// test pass vacuously: tradeUSDVolume returns nil for any source
	// external.Lookup does not classify as an exchange, so every usd_volume
	// would be NULL on both paths and the column comparison would prove
	// nothing. The two runs are separated by LEDGER WINDOW instead.
	const upsertLo, upsertHi = 50_000_000, 50_999_999
	const bulkLo, bulkHi = 55_000_000, 55_999_999
	upsertRows := bulkTradeSet(t, n, upsertLo, upsertTS, "sdex")
	bulkRows := bulkTradeSet(t, n, bulkLo, bulkTS, "sdex")
	// One unstorable one-side-zero fill and one exact intra-batch PK
	// duplicate, so the comparison covers the storability gate and the dedupe
	// collapse. The duplicate is EXACT rather than a differing later copy:
	// sortTradesByConflictKey uses a non-stable sort, so which of two
	// equal-key copies survives is unspecified, and a test that depended on
	// it would be asserting an implementation accident rather than identity.
	spoil := func(rows []c.Trade) []c.Trade {
		zero := rows[7]
		zero.QuoteAmount = c.NewAmount(big.NewInt(0))
		dup := rows[11]
		return append(append(append([]c.Trade{}, rows...), zero), dup)
	}
	upsertRows = spoil(upsertRows)
	bulkRows = spoil(bulkRows)

	if err := store.BatchInsertTrades(ctx, upsertRows); err != nil {
		t.Fatalf("BatchInsertTrades: %v", err)
	}
	res, err := store.BulkBackfillTrades(ctx, bulkRows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("bulk path = %q (%s), want the COPY path — the range was empty",
			res.Path, res.FallbackReason)
	}

	got := readTradeRows(t, db, "sdex", bulkLo, bulkHi)
	want := readTradeRows(t, db, "sdex", upsertLo, upsertHi)
	if len(got) != len(want) {
		t.Fatalf("row count: bulk=%d upsert=%d — the two writers disagree on which rows are storable",
			len(got), len(want))
	}
	if len(got) == 0 {
		t.Fatal("no rows landed at all")
	}
	if int64(len(got)) != res.Copied {
		t.Fatalf("COPY reported %d rows, %d are stored", res.Copied, len(got))
	}
	// The one-side-zero fill must be absent from BOTH, and the dupe
	// collapsed to one row in BOTH.
	if len(got) != n {
		t.Fatalf("stored %d rows for %d distinct storable inputs — the unstorable fill or the "+
			"intra-batch duplicate was not collapsed", len(got), n)
	}
	for i := range got {
		g, w := got[i], want[i]
		// Identity differs BY CONSTRUCTION (different source/ledger/ts window);
		// everything derived must not.
		if g.baseAsset != w.baseAsset || g.quoteAsset != w.quoteAsset {
			t.Fatalf("row %d assets: bulk=(%s,%s) upsert=(%s,%s)", i, g.baseAsset, g.quoteAsset, w.baseAsset, w.quoteAsset)
		}
		if g.baseAmount != w.baseAmount || g.quoteAmount != w.quoteAmount {
			t.Fatalf("row %d amounts: bulk=(%s,%s) upsert=(%s,%s)", i, g.baseAmount, g.quoteAmount, w.baseAmount, w.quoteAmount)
		}
		if g.usdVolume != w.usdVolume {
			t.Fatalf("row %d usd_volume: bulk=%v upsert=%v — the bulk path must run the same tradeUSDVolume waterfall",
				i, g.usdVolume, w.usdVolume)
		}
		if g.maker != w.maker || g.taker != w.taker {
			t.Fatalf("row %d maker/taker: bulk=(%v,%v) upsert=(%v,%v) — empty must land as NULL, not ''",
				i, g.maker, g.taker, w.maker, w.taker)
		}
		if g.deriveGeneration != w.deriveGeneration || g.deriveGeneration != gen {
			t.Fatalf("row %d derive_generation: bulk=%d upsert=%d want=%d",
				i, g.deriveGeneration, w.deriveGeneration, gen)
		}
		if g.routedVia.Valid || g.signer.Valid {
			t.Fatalf("row %d: the bulk path wrote routed_via=%v / signer=%v — both are owned by their "+
				"own post-insert sweepers and neither insert path may set them", i, g.routedVia, g.signer)
		}
	}
	// usd_volume must actually be exercised, not vacuously NULL everywhere.
	var populated int
	for _, g := range got {
		if g.usdVolume.Valid {
			populated++
		}
	}
	if populated == 0 {
		t.Fatal("no row resolved a usd_volume — the comparison would pass vacuously; " +
			"check InstallUSDVolumeResolution wiring in this test")
	}
	t.Logf("compared %d rows; %d carry a resolved usd_volume", len(got), populated)

	// source_entry_counts: the bulk path bumps once for the whole buffer, the
	// upsert path once per batch. Same landed count either way.
	// Both runs bumped the one "sdex" tally row; the total is what each path
	// claims it landed, so a bulk path that mis-counted its own inserts shows
	// up here.
	if got := sourceEntryCount(t, db, "sdex"); got != int64(2*n) {
		t.Fatalf("source_entry_counts[sdex] = %d, want %d (%d upserted + %d bulk-copied)",
			got, 2*n, n, n)
	}
	// The classic-asset registry hook fires on both paths.
	var registered int
	if err := db.QueryRow(`SELECT count(*) FROM classic_assets`).Scan(&registered); err != nil {
		t.Fatalf("classic_assets: %v", err)
	}
	if registered == 0 {
		t.Fatal("classic_assets is empty — neither path ran the registry hook, so this " +
			"assertion cannot distinguish them")
	}
}

// TestBulkBackfillTrades_RefusesNonEmptyRange proves the precondition is
// CHECKED, not assumed from the caller: one pre-existing row inside the
// buffer's (source, ledger, ts) box is enough to push the whole buffer onto
// the upsert path — where the stored row keeps its generation guard.
func TestBulkBackfillTrades_RefusesNonEmptyRange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	baseTS := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	rows := bulkTradeSet(t, 500, 50_000_000, baseTS, "sdex")

	// Land ONE row of the buffer first, at the LIVE generation.
	live := bulkStore(t, ctx, dsn, 0)
	if err := live.InsertTrade(ctx, rows[250]); err != nil {
		t.Fatalf("seed InsertTrade: %v", err)
	}

	store := bulkStore(t, ctx, dsn, 1_700_000_000)
	res, err := store.BulkBackfillTrades(ctx, rows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathUpsert {
		t.Fatalf("bulk path = %q, want %q — a single stored row inside the buffer's ledger/ts box "+
			"must refuse the COPY precondition", res.Path, timescale.BulkBackfillPathUpsert)
	}
	if res.FallbackReason == "" {
		t.Fatal("fell back with no reason recorded — an operator cannot tell a silent revert from a fast run")
	}
	t.Logf("refused, as required: %s", res.FallbackReason)

	// Every row still landed, exactly once, through the upsert.
	got := readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)
	if len(got) != len(rows) {
		t.Fatalf("stored %d rows, want %d — the fallback must still write the whole buffer", len(got), len(rows))
	}

	// And a SECOND call now also refuses (the range is emphatically non-empty),
	// which is the re-run case an operator hits after a partial run.
	res2, err := store.BulkBackfillTrades(ctx, rows, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades re-run: %v", err)
	}
	if res2.Path != timescale.BulkBackfillPathUpsert {
		t.Fatalf("re-run path = %q, want the upsert fallback", res2.Path)
	}
	if n := len(readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)); n != len(rows) {
		t.Fatalf("re-run stored %d rows, want %d — the fallback upsert must stay idempotent in row count", n, len(rows))
	}
}

// TestBulkBackfillTrades_EmptyProbeIsLedgerScoped pins the shape of the
// precondition query itself: a stored row for the SAME source at a ledger
// OUTSIDE the buffer's extent must not refuse the fast path (otherwise every
// re-derive below a populated floor would fall back forever), while a row
// inside it must.
func TestBulkBackfillTrades_EmptyProbeIsLedgerScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	// A populated "live" region far above the backfill window — this is r1's
	// actual shape: sdex rows above the trade floor, nothing below it.
	live := bulkStore(t, ctx, dsn, 0)
	above := bulkTradeSet(t, 200, 61_600_000, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "sdex")
	if err := live.BatchInsertTrades(ctx, above); err != nil {
		t.Fatalf("seed above floor: %v", err)
	}

	store := bulkStore(t, ctx, dsn, 1_700_000_000)
	below := bulkTradeSet(t, 500, 50_000_000, time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC), "sdex")
	res, err := store.BulkBackfillTrades(ctx, below, timescale.BulkBackfillOptions{})
	if err != nil {
		t.Fatalf("BulkBackfillTrades: %v", err)
	}
	if res.Path != timescale.BulkBackfillPathCopy {
		t.Fatalf("path = %q (%s), want the COPY path — rows ABOVE the backfill window must not "+
			"refuse a window that is itself empty", res.Path, res.FallbackReason)
	}
	if n := len(readTradeRows(t, store.DB(), "sdex", 0, 2_000_000_000)); n != len(above)+len(below) {
		t.Fatalf("stored %d rows, want %d", n, len(above)+len(below))
	}
}

// TestDecompressTradesChunk_DoesNotConvoyTheDatabase is the DB-backed
// reproduction of an r1 incident, on the deployed pair
// (TimescaleDB 2.26.4 / PG 15), and the proof that the fix removes it.
//
// WHAT HAPPENED. A deploy restarted stellarindex-aggregator at 00:12:23
// UTC. Its cold-start VWAP alias-map aggregation spilled to disk
// (wait_event = IO/BufFileRead) and held AccessShareLock on `trades` for
// 18+ minutes. A `usd-volume-restamp -chunks` run was mid-window; its
// decompress_chunk asked for AccessExclusiveLock on a chunk of that
// hypertable and could not have it, so it QUEUED. A pending exclusive
// request is not a private wait — PostgreSQL puts every LATER request for
// that object behind it, however trivial:
//
//	decompress_chunk (restamp)      blocked 1,984 s
//	UPDATE trades  x2 (restamp)     blocked 1,164 s
//	postgres_exporter scrapes x3    blocked   917 s
//	chunks_detailed_size (watcher)  blocked   904 s
//
// The exporter being in that list is what made it dangerous: alerting went
// cascade-blind for the whole window while /v1/status read `degraded` and
// both systemd units read `active`.
//
// THE SHAPE OF THIS TEST is those three sessions, in that order:
//
//	A — holds AccessShareLock on `trades` in an open transaction (the
//	    aggregator's spilled read);
//	B — DecompressTradesChunk on the compressed chunk (the restamp);
//	C — one trivial `SELECT count(*) FROM trades` issued once B's request
//	    is observably queued (the postgres_exporter scrape).
//
// C is the assertion. Against an UNBOUNDED decompress it never returns: it
// is queued behind B's pending exclusive request and dies at its own
// lock_timeout. Against the bounded one B withdraws after
// tradesChunkDecompressLock.wait (5 s) and spends the next 15 s not
// asking, so C gets its AccessShareLock and answers.
//
// The second half is the crash-safety half: withdrawing must not mean
// giving up. Once A commits, B's next attempt takes the lock and the chunk
// ends DECOMPRESSED — the state the caller asked for — rather than the
// walk failing and leaving a human to finish it.
func TestDecompressTradesChunk_DoesNotConvoyTheDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ── a compressed `trades` chunk, as the 7-day policy leaves them ──
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		tr := mkIntegrationTrade("sdex", 20+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(ch, true) FROM show_chunks('trades') ch`); err != nil {
		t.Fatalf("compress the fixture chunk: %v", err)
	}
	chunks, err := store.TradesChunksInRange(ctx, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("TradesChunksInRange: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Compressed {
		t.Fatalf("fixture: want exactly one COMPRESSED chunk, got %+v", chunks)
	}
	chunk := chunks[0]

	// ── session A: the aggregator's long cold-start read ──────────────
	//
	// Its own pool, so nothing here can be served by a connection the
	// store is also using. The transaction stays open — that is what
	// holds AccessShareLock on the hypertable and its chunk.
	reader, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readerTx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("session A begin: %v", err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = readerTx.Rollback()
		}
	})
	var seen int
	if err := readerTx.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&seen); err != nil {
		t.Fatalf("session A read: %v", err)
	}
	if seen != 5 {
		t.Fatalf("session A saw %d trades, want 5", seen)
	}

	// ── session B: the restamp's decompress ───────────────────────────
	type decompressResult struct{ err error }
	done := make(chan decompressResult, 1)
	go func() { done <- decompressResult{store.DecompressTradesChunk(ctx, chunk)} }()

	// ── wait until B's request is observably QUEUED ───────────────────
	//
	// Both with and without the fix there is a window in which the
	// decompress is waiting on the lock; without it, that window never
	// ends. Starting C from inside the window is what makes the two
	// outcomes differ by behaviour rather than by timing luck.
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	waitForQueuedDecompress(t, ctx, probe)

	// ── session C: the postgres_exporter scrape ───────────────────────
	//
	// 9 s is deliberately just past the 5 s the bounded decompress may
	// hold a request pending, and far short of the 904–1,984 s the real
	// convoy inflicted. A failure here IS the incident.
	scrapeCtx, scrapeCancel := context.WithTimeout(ctx, 60*time.Second)
	defer scrapeCancel()
	conn, err := probe.Conn(scrapeCtx)
	if err != nil {
		t.Fatalf("session C conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(scrapeCtx, `SET lock_timeout = '9s'`); err != nil {
		t.Fatalf("session C set lock_timeout: %v", err)
	}
	start := time.Now()
	var n int
	err = conn.QueryRowContext(scrapeCtx, `SELECT count(*) FROM trades`).Scan(&n)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a trivial read was CONVOYED behind the decompress's pending exclusive lock "+
			"(%v after %s) — this is the 2026-09-10 pile-up: the decompress is parking an "+
			"unbounded AccessExclusiveLock request and PostgreSQL is queueing everything behind it", err, elapsed)
	}
	if n != 5 {
		t.Errorf("session C read %d trades, want 5", n)
	}
	t.Logf("trivial read completed in %s while the decompress was contending", elapsed)

	// ── the decompress must not have GIVEN UP ─────────────────────────
	//
	// Withdrawing is only safe because it comes back. Release the reader
	// and the next attempt takes the lock.
	if err := readerTx.Commit(); err != nil {
		t.Fatalf("session A commit: %v", err)
	}
	committed = true

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("DecompressTradesChunk = %v, want it to retry past the contention and succeed", res.err)
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("DecompressTradesChunk never returned after the conflicting lock was released")
	}

	var compressed bool
	if err := store.DB().QueryRowContext(ctx,
		`SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`,
		chunk.Schema, chunk.Name).Scan(&compressed); err != nil {
		t.Fatalf("read the chunk's state: %v", err)
	}
	if compressed {
		t.Error("the chunk is still compressed: the bounded wait turned into a silent no-op")
	}
}

// TestDecompressTradesChunk_WithdrawsItsRequestBetweenAttempts pins the
// mechanism rather than one of its effects: a decompress that cannot have
// its lock must spend most of its time NOT asking.
//
// That is the whole cure. PostgreSQL convoys on a PENDING exclusive
// request — every later request for the object queues behind it — so the
// damage is a function of how long ours is outstanding, not of how long we
// would like the lock. The bounded loop asks for 5 s, withdraws, and stays
// quiet for 15 s; during that quiet the queue behind it drains, which is
// what lets a trivial read through. Sampling pg_stat_activity across a
// full cycle turns that into an assertion: with an UNBOUNDED decompress
// every sample finds it waiting, because the one request it made never
// goes away.
//
// It carries the crash-safety half too, which is the objection the bound
// has to answer — a decompress that fails mid-chunk is the state this
// project fears most. It cannot arise: the statement runs in its own
// transaction, so a `lock_timeout` expiry (SQLSTATE 55P03) rolls back
// atomically, the chunk is left exactly as compressed as it was with every
// row readable, and [timescale.Store.RestampTradesChunk]'s existing
// contract ("a failed decompress runs nothing") is untouched.
func TestDecompressTradesChunk_WithdrawsItsRequestBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		tr := mkIntegrationTrade("sdex", 30+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(ch, true) FROM show_chunks('trades') ch`); err != nil {
		t.Fatalf("compress the fixture chunk: %v", err)
	}
	chunks, err := store.TradesChunksInRange(ctx, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("TradesChunksInRange: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Compressed {
		t.Fatalf("fixture: want exactly one COMPRESSED chunk, got %+v", chunks)
	}
	chunk := chunks[0]

	reader, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readerTx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	defer func() { _ = readerTx.Rollback() }()
	var seen int
	if err := readerTx.QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&seen); err != nil {
		t.Fatalf("blocker read: %v", err)
	}

	// A caller-imposed deadline shorter than the retry budget is how the
	// stop is reached without waiting 35 minutes for it. It is longer than
	// one full ask+drain cycle (5 s + 15 s), so a loop that withdraws has
	// to be caught doing it.
	shortCtx, shortCancel := context.WithTimeout(ctx, 28*time.Second)
	defer shortCancel()
	var decompressErr error
	finished := make(chan struct{})
	go func() {
		decompressErr = store.DecompressTradesChunk(shortCtx, chunk)
		close(finished)
	}()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	asking, quiet := sampleDecompressLockWait(t, ctx, sampler, finished)
	t.Logf("decompress had a lock request outstanding in %d sample(s), none in %d", asking, quiet)
	if asking == 0 {
		t.Fatal("never observed the decompress waiting on the lock; the fixture did not reproduce the contention")
	}
	if quiet == 0 {
		t.Fatalf("the decompress had a lock request outstanding in ALL %d samples across a full ask+drain cycle — "+
			"it is parking a pending AccessExclusiveLock, and PostgreSQL queues every later request for that chunk behind it", asking)
	}

	<-finished
	if decompressErr == nil {
		t.Fatal("DecompressTradesChunk = nil while a conflicting lock was held for its whole budget")
	}

	var compressed bool
	if err := store.DB().QueryRowContext(ctx,
		`SELECT is_compressed FROM timescaledb_information.chunks WHERE chunk_schema = $1 AND chunk_name = $2`,
		chunk.Schema, chunk.Name).Scan(&compressed); err != nil {
		t.Fatalf("read the chunk's state: %v", err)
	}
	if !compressed {
		t.Fatal("the chunk was left DECOMPRESSED by a decompress that never got its lock — " +
			"the bounded wait must roll back atomically, not half-open a 160 GB chunk")
	}
	// And the rows are still there and readable through the compressed
	// chunk: "still compressed" must mean intact, not merely flagged.
	var rows int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM trades`).Scan(&rows); err != nil {
		t.Fatalf("read trades after the refused decompress: %v", err)
	}
	if rows != 5 {
		t.Errorf("trades holds %d rows after the refused decompress, want 5", rows)
	}
}

// decompressAskingSQL is true while some backend running decompress_chunk
// has a heavyweight-lock request outstanding. Reading pg_stat_activity
// costs no lock on `trades`, so this stays answerable from inside a convoy
// — the same property the stellarindex_pg_lock_convoy probe depends on.
const decompressAskingSQL = `
	SELECT EXISTS (
	  SELECT 1 FROM pg_stat_activity
	   WHERE query ILIKE '%decompress_chunk%'
	     AND query NOT ILIKE '%pg_stat_activity%'
	     AND wait_event_type = 'Lock')`

// sampleDecompressLockWait polls until it has seen the decompress both
// ASKING for the lock and, AFTERWARDS, not asking — or until the
// decompress returns. A bounded loop produces both counts within one
// cycle; an unbounded one produces only the first, because its single
// request never goes away.
//
// Quiet samples before the first asking one are DISCARDED, deliberately.
// They are the window before the goroutine's statement reached the
// server, and counting them would let the test pass on a race rather
// than on a withdrawal: a quiet sample only means something once we have
// watched the request exist.
func sampleDecompressLockWait(t *testing.T, ctx context.Context, db *sql.DB, finished <-chan struct{}) (asking, quiet int) {
	t.Helper()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-finished:
			return asking, quiet
		case <-tick.C:
		}
		var pending bool
		if err := db.QueryRowContext(ctx, decompressAskingSQL).Scan(&pending); err != nil {
			t.Fatalf("sample pg_stat_activity: %v", err)
		}
		switch {
		case pending:
			asking++
		case asking > 0:
			quiet++
			return asking, quiet
		}
	}
}

// waitForQueuedDecompress blocks until a backend running decompress_chunk
// is waiting on a heavyweight lock — i.e. the restamp's exclusive request
// is queued. Polling the server beats sleeping a guessed interval: the
// window is what the test needs to be inside, and on an unfixed build it
// opens once and never closes.
func waitForQueuedDecompress(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var queued bool
		err := db.QueryRowContext(ctx, decompressAskingSQL).Scan(&queued)
		if err != nil {
			t.Fatalf("poll for the queued decompress: %v", err)
		}
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no decompress_chunk ever queued on a lock; the fixture did not reproduce the contention")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// These pin, on the deployed pair (TimescaleDB 2.26.4 / PG 15), what the
// chunk restamp's 5 s lock bound actually covers and the long-holder check
// in front of it. Each attempt opens with LOCK TABLE on the locks the
// function takes first, so a refusal there costs no work; the function's
// late AccessExclusiveLock request is bounded at the same 5 s; and a
// transaction that has held a lock on the chunk for over a minute (the
// incident shape) is waited out before any work starts.

// openCompressedTradesChunk opens a store on a fresh database holding one
// compressed `trades` chunk and returns both.
func openCompressedTradesChunk(t *testing.T, ctx context.Context) (string, *timescale.Store, timescale.TradeChunk) {
	t.Helper()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 40+i, day.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 500_000_000)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `SELECT compress_chunk(ch, true) FROM show_chunks('trades') ch`); err != nil {
		t.Fatalf("compress the fixture chunk: %v", err)
	}
	chunks, err := store.TradesChunksInRange(ctx, day.Add(-24*time.Hour), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("TradesChunksInRange: %v", err)
	}
	if len(chunks) != 1 || !chunks[0].Compressed {
		t.Fatalf("fixture: want exactly one COMPRESSED chunk, got %+v", chunks)
	}
	return dsn, store, chunks[0]
}

// holdInOpenTx runs stmt in a transaction on its own pool and leaves the
// transaction open; the returned func ends it.
func holdInOpenTx(t *testing.T, ctx context.Context, dsn, stmt string) func() {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("holder %q: %v", stmt, err)
	}
	ended := false
	end := func() {
		if !ended {
			ended = true
			_ = tx.Rollback()
		}
	}
	t.Cleanup(end)
	return end
}

// chunkLocksOfOthersSQL lists the chunk locks held or requested by the
// backend running the restamp's statements ($3 matches its query).
const chunkLocksOfOthersSQL = `
	SELECT l.mode, l.granted
	  FROM pg_locks l
	  JOIN pg_stat_activity a ON a.pid = l.pid
	 WHERE l.relation = to_regclass(format('%I.%I', $1::text, $2::text))
	   AND a.query ILIKE $3
	   AND a.query NOT ILIKE '%pg_locks%'`

type lockSample struct {
	mode    string
	granted bool
}

// lockSnapshot is one poll: when it was taken and what it found.
type lockSnapshot struct {
	at    time.Time
	locks []lockSample
}

// sampleRestampChunkLocks polls the chunk's locks held by a backend whose
// current query matches like, every 100 ms for d.
func sampleRestampChunkLocks(t *testing.T, ctx context.Context, db *sql.DB, chunk timescale.TradeChunk, like string, d time.Duration) []lockSnapshot {
	t.Helper()
	var out []lockSnapshot
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		rows, err := db.QueryContext(ctx, chunkLocksOfOthersSQL, chunk.Schema, chunk.Name, like)
		if err != nil {
			t.Fatalf("sample pg_locks: %v", err)
		}
		s := lockSnapshot{at: time.Now()}
		for rows.Next() {
			var l lockSample
			if err := rows.Scan(&l.mode, &l.granted); err != nil {
				t.Fatal(err)
			}
			s.locks = append(s.locks, l)
		}
		_ = rows.Close()
		out = append(out, s)
	}
	return out
}

// TestDecompressTradesChunk_WaitsOutALongHolderBeforeWorking is the
// incident shape: a reader whose transaction has been open for over a
// minute holds AccessShareLock on the chunk. The decompress must not start
// work whose end-stage lock that reader would refuse; it waits with no
// request pending and starts once the reader is gone.
func TestDecompressTradesChunk_WaitsOutALongHolderBeforeWorking(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)

	end := holdInOpenTx(t, ctx, dsn, `SELECT count(*) FROM trades`)
	time.Sleep(62 * time.Second) // past the 60 s long-holder age

	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(ctx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	// 8 s spans more than one 5 s request: an attempt started against the
	// holder would be seen holding or asking for a chunk lock.
	for i, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "%_chunk%", 8*time.Second) {
		if len(s.locks) > 0 {
			t.Fatalf("sample %d: the decompress took or asked for chunk locks %+v while a long-running reader held the chunk", i, s.locks)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("DecompressTradesChunk returned %v while the long holder was still there", err)
	default:
	}

	end()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DecompressTradesChunk = %v, want it to start once the holder was gone", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("DecompressTradesChunk never returned after the long holder ended")
	}
	if chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk is still compressed")
	}
}

// TestDecompressTradesChunk_RefusedOpeningLockStartsNoWork: a writer's
// RowExclusiveLock conflicts with the ExclusiveLock both functions open
// with. The attempt's own LOCK TABLE is what waits and is refused, so
// decompress_chunk never runs and the error says no work was lost.
func TestDecompressTradesChunk_RefusedOpeningLockStartsNoWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)
	holdInOpenTx(t, ctx, dsn, `LOCK TABLE ONLY `+chunk.Schema+`.`+chunk.Name+` IN ROW EXCLUSIVE MODE`)

	shortCtx, shortCancel := context.WithTimeout(ctx, 12*time.Second)
	defer shortCancel()
	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(shortCtx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	var asked bool
	for _, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "LOCK TABLE ONLY%IN EXCLUSIVE MODE", 4*time.Second) {
		for _, l := range s.locks {
			if l.mode == "ExclusiveLock" && !l.granted {
				asked = true
			}
		}
	}
	if !asked {
		t.Fatal("never saw the attempt's LOCK TABLE waiting for ExclusiveLock on the chunk")
	}
	err = <-done
	if err == nil {
		t.Fatal("DecompressTradesChunk = nil while a writer held the chunk for its whole window")
	}
	for _, want := range []string{"refused before any work", "0 refused inside the statement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want containing %q", err, want)
		}
	}
	if !chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk was left decompressed by a refused attempt")
	}
}

// TestDecompressTradesChunk_EndStageRequestIsBoundedToo: with a young
// reader on the chunk, the attempt gets its ExclusiveLock and then waits
// for AccessExclusiveLock on the same chunk — the late request — and that
// wait ends at the same 5 s bound, inside the statement.
func TestDecompressTradesChunk_EndStageRequestIsBoundedToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn, store, chunk := openCompressedTradesChunk(t, ctx)
	holdInOpenTx(t, ctx, dsn, `SELECT count(*) FROM trades`)

	shortCtx, shortCancel := context.WithTimeout(ctx, 12*time.Second)
	defer shortCancel()
	done := make(chan error, 1)
	go func() { done <- store.DecompressTradesChunk(shortCtx, chunk) }()

	sampler, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	var endStage, sawAsk bool
	var firstAsk, lastAsk time.Time
	start := time.Now()
	for _, s := range sampleRestampChunkLocks(t, ctx, sampler, chunk, "%decompress_chunk%", 9*time.Second) {
		var heldExclusive, askingAccessExclusive bool
		for _, l := range s.locks {
			heldExclusive = heldExclusive || (l.mode == "ExclusiveLock" && l.granted)
			askingAccessExclusive = askingAccessExclusive || (l.mode == "AccessExclusiveLock" && !l.granted)
		}
		if askingAccessExclusive {
			if !sawAsk {
				firstAsk = s.at
			}
			sawAsk, lastAsk = true, s.at
			endStage = endStage || heldExclusive
		}
	}
	if !endStage {
		t.Fatal("never saw decompress_chunk holding ExclusiveLock while asking for AccessExclusiveLock on the chunk")
	}
	if asked := lastAsk.Sub(firstAsk); asked > 6*time.Second {
		t.Errorf("the end-stage request stayed pending %s, want it withdrawn at the 5 s bound", asked)
	}
	t.Logf("end-stage request pending from %s to %s after the call", firstAsk.Sub(start), lastAsk.Sub(start))

	err = <-done
	if err == nil || !strings.Contains(err.Error(), "1 refused inside the statement") {
		t.Fatalf("err = %v, want the refusal counted as inside the statement", err)
	}
	if !chunkIsCompressed(t, ctx, store.DB(), chunk.String()) {
		t.Error("the chunk was left decompressed by a refused attempt")
	}
}

// Store.EarliestTradeInWindow is the probe backfill-external refuses an
// overlapping window on. It must be scoped to exactly (source, pair) and
// the half-open [from, to): a row for another venue, another pair, or
// outside the window must neither refuse the run nor be reported as the
// -to bound.
func TestEarliestTradeInWindow_ScopedToSourcePairAndWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlmUSD := earliestPair(t, "crypto:XLM", "fiat:USD")
	xlmEUR := earliestPair(t, "crypto:XLM", "fiat:EUR")
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	want := from.Add(3*time.Hour + 250*time.Microsecond)

	seed := []struct {
		source string
		pair   c.Pair
		ts     time.Time
	}{
		{"kraken", xlmUSD, from.Add(-time.Second)},  // before the window
		{"kraken", xlmUSD, to},                      // `to` is exclusive
		{"bitstamp", xlmUSD, from.Add(time.Hour)},   // another venue
		{"kraken", xlmEUR, from.Add(time.Hour)},     // another pair
		{"kraken", xlmUSD, want.Add(2 * time.Hour)}, // later in window
		{"kraken", xlmUSD, want},                    // the answer
	}
	for i, s := range seed {
		tr := c.Trade{
			Source:      s.source,
			TxHash:      fmt.Sprintf("%064x", i+1),
			Timestamp:   s.ts,
			Pair:        s.pair,
			BaseAmount:  c.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(17_000_000)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d InsertTrade: %v", i, err)
		}
	}

	got, found, err := store.EarliestTradeInWindow(ctx, "kraken", xlmUSD, from, to)
	if err != nil {
		t.Fatalf("EarliestTradeInWindow: %v", err)
	}
	if !found || !got.Equal(want) {
		t.Fatalf("EarliestTradeInWindow = (%v, %v), want (%v, true)", got, found, want)
	}

	// A window holding only other venues' and other pairs' rows is empty.
	_, found, err = store.EarliestTradeInWindow(ctx, "kraken", xlmUSD, from, from.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("EarliestTradeInWindow (narrow): %v", err)
	}
	if found {
		t.Fatal("window with only another venue's and another pair's rows reported as occupied")
	}
}

func earliestPair(t *testing.T, base, quote string) c.Pair {
	t.Helper()
	b, err := c.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", base, err)
	}
	q, err := c.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", quote, err)
	}
	p, err := c.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// TestTradesInRangeAfterFromSource executes the single-source page read:
// only the named source's rows come back, the full-PK cursor still pages
// through them, and an empty source is the unfiltered read.
func TestTradesInRangeAfterFromSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	for i, src := range []string{"sdex", "soroswap", "sdex", "soroswap", "sdex"} {
		tr := mkIntegrationTrade(src, i+1, t0.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 12_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	from, to := t0.Add(-time.Minute), t0.Add(time.Hour)

	got, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("source=sdex returned %d rows, want 3", len(got))
	}
	for _, tr := range got {
		if tr.Source != "sdex" {
			t.Errorf("source=sdex returned a %q row", tr.Source)
		}
	}

	// Cursor pagination over the filtered stream.
	first, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to, time.Time{}, 0, "", "", 0, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page: rows=%d err=%v", len(first), err)
	}
	f := first[0]
	next, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to,
		f.Timestamp, f.Ledger, f.TxHash, f.Source, f.OpIndex, 100)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next) != 2 || next[0].Ledger != f.Ledger+2 {
		t.Fatalf("second page = %d rows (first ledger %d), want the remaining 2 starting two ledgers after the first page", len(next), firstLedger(next))
	}

	none, err := store.TradesInRangeAfterFromSource(ctx, pair, "no-such-source", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown source: rows=%d err=%v, want empty", len(none), err)
	}

	// The off-chain exclusion still applies when a source is named.
	cex := mkIntegrationTrade("binance", 6, t0.Add(6*time.Minute), pair, 1_000_000_000, 12_000_000)
	if err := store.InsertTrade(ctx, cex); err != nil {
		t.Fatalf("InsertTrade binance: %v", err)
	}
	offChain, err := store.TradesInRangeAfterFromSource(ctx, pair, "binance", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil || len(offChain) != 0 {
		t.Fatalf("off-chain source: rows=%d err=%v, want empty", len(offChain), err)
	}

	all, err := store.TradesInRangeAfterFromSource(ctx, pair, "", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil || len(all) != 5 {
		t.Fatalf("empty source: rows=%d err=%v, want the unfiltered 5", len(all), err)
	}
}

func firstLedger(ts []c.Trade) uint32 {
	if len(ts) == 0 {
		return 0
	}
	return ts[0].Ledger
}

// TestTradesInRangeAndMarkets covers the two read-paths backing
// /v1/history and /v1/markets: time-bounded trade lookup and
// distinct-pair enumeration. Proves the hypertable indexes + GROUP
// BY behave correctly end-to-end.
func TestTradesInRangeAndMarkets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Two pairs: XLM/USDC and XLM/USDC.fake — enough to exercise
	// DistinctPairs grouping without needing many assets.
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	// AQUA's issuer — a real, CRC-valid mainnet G-strkey, distinct
	// from USDC's issuer above. We need the strkey to round-trip
	// through canonical.NewClassicAsset's CRC check, so a
	// hand-crafted "USDC-issuer with last char tweaked" string
	// (which a format-only validator would accept) is rejected.
	fake, err := c.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatal(err)
	}
	pairA, _ := c.NewPair(c.NativeAsset(), usdc)
	pairB, _ := c.NewPair(c.NativeAsset(), fake)

	// Anchor at a fixed point so the window queries are deterministic.
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	trades := []c.Trade{
		mkIntegrationTrade("sdex", 1, t0.Add(0*time.Minute), pairA, 1_000_000_000, 12_000_000),
		mkIntegrationTrade("sdex", 2, t0.Add(10*time.Minute), pairA, 1_000_000_000, 12_100_000),
		mkIntegrationTrade("sdex", 3, t0.Add(20*time.Minute), pairA, 1_000_000_000, 12_200_000),
		mkIntegrationTrade("sdex", 4, t0.Add(30*time.Minute), pairB, 1_000_000_000, 12_050_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	// ─── TradesInRange ──────────────────────────────────────────────
	// Window covering only the middle two pairA trades (10-25 min).
	windowStart := t0.Add(10 * time.Minute)
	windowEnd := t0.Add(25 * time.Minute)
	got, err := store.TradesInRange(ctx, pairA, windowStart, windowEnd, 100)
	if err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d trades in window, want 2", len(got))
	}
	// Must be ordered ts ASC.
	if !got[0].Timestamp.Before(got[1].Timestamp) {
		t.Errorf("trades not in ascending-ts order")
	}
	// Must be pairA only — pairB shouldn't leak in.
	for _, tr := range got {
		if !tr.Pair.Equal(pairA) {
			t.Errorf("pair B leaked into pair A query: %+v", tr.Pair)
		}
	}

	// Empty window → empty slice, no error.
	empty, err := store.TradesInRange(ctx, pairA, t0.Add(-1*time.Hour), t0.Add(-30*time.Minute), 100)
	if err != nil {
		t.Fatalf("TradesInRange (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty slice for empty window, got %d", len(empty))
	}

	// from > to rejection.
	if _, err := store.TradesInRange(ctx, pairA, windowEnd, windowStart, 100); err == nil {
		t.Error("TradesInRange should reject from > to")
	}

	// ─── DistinctPairs ──────────────────────────────────────────────
	// Force-refresh prices_1m so DistinctPairs sees the seeded
	// trades. Per rc.45 (commit 8717bc20), DistinctPairs reads the
	// 1-min continuous aggregate rather than scanning the raw
	// trades table — without this refresh the seeded rows are
	// present in `trades` but absent from `prices_1m` until the
	// 30 s policy fires (longer than the test window). Mirrors the
	// pattern in test/integration/api_test.go:65-74.
	// DistinctPairs enumerates pairs from prices_1d (the right-granularity
	// rewrite) and reads 24h volume from prices_1m — refresh BOTH, or
	// the pair list comes back empty even though prices_1m has the rows.
	for _, stmt := range []string{
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
		`CALL refresh_continuous_aggregate('prices_1d', NULL, NULL)`,
	} {
		if _, err := store.DB().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("refresh cagg: %v", err)
		}
	}

	markets, next, err := store.DistinctPairs(ctx, "", 500)
	if err != nil {
		t.Fatalf("DistinctPairs: %v", err)
	}
	if len(markets) != 2 {
		t.Fatalf("got %d markets, want 2 (XLM/USDC + XLM/AQUA)", len(markets))
	}
	if next != "" {
		t.Errorf("expected empty cursor on final page, got %q", next)
	}

	// Every returned market should have LastTradeAt populated.
	for _, m := range markets {
		if m.LastTradeAt.IsZero() {
			t.Errorf("market %s|%s has zero last_trade_at",
				m.Pair.Base.String(), m.Pair.Quote.String())
		}
	}

	// ─── DistinctPairs pagination round-trip ────────────────────────
	// Limit=1 forces paging; confirm the two pairs come back across
	// pages with a non-empty cursor after page 1 and an empty cursor
	// after page 2. Guards the recent markets.go change where the
	// page-break logic was rewritten — the `hasMore` signal must
	// fire only when a page was actually held back.
	var paged []timescale.Market
	cursor := ""
	for iter := 0; iter < 5; iter++ {
		page, nextC, err := store.DistinctPairs(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("paged iter %d: %v", iter, err)
		}
		if len(page) > 1 {
			t.Fatalf("paged iter %d: limit=1 returned %d rows", iter, len(page))
		}
		paged = append(paged, page...)
		if nextC == "" {
			break
		}
		cursor = nextC
	}
	if len(paged) != 2 {
		t.Errorf("paginated DistinctPairs returned %d total rows, want 2", len(paged))
	}
}

func mkIntegrationTrade(source string, nonce int, ts time.Time, pair c.Pair, base, quote int64) c.Trade {
	// Generate a unique 64-char *hex* tx_hash per (source, nonce).
	// Earlier revision embedded the literal source string ("sdex")
	// into the hash, which broke canonical.Trade.Validate's
	// 64-char-hex check once validation tightened. Now we hex-
	// encode each source byte so the hash stays parseable.
	const hex = "0123456789abcdef"
	h := make([]byte, 64)
	for i := range h {
		h[i] = '0'
	}
	for i, b := range []byte(source) {
		if 2*i+1 >= 32 {
			break
		}
		h[32+2*i] = hex[b>>4]
		h[32+2*i+1] = hex[b&0xf]
	}
	h[62] = hex[(nonce>>4)&0xf]
	h[63] = hex[nonce&0xf]

	return c.Trade{
		Source:      source,
		Ledger:      uint32(50_000_000 + nonce),
		TxHash:      string(h),
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(base)),
		QuoteAmount: c.NewAmount(big.NewInt(quote)),
	}
}

// flakeyFXResolver implements timescale.USDVolumeFXResolver. It resolves the
// prices it knows unless `fail` is set, in which case it returns a transient
// error — the exact class (deadlock / statement-timeout / pool blip / Patroni
// failover) that VWAPUSDFXResolver.USDPriceAt surfaces and does NOT
// negative-cache, so each independent write is a fresh chance to miss.
type flakeyFXResolver struct {
	prices map[string]string
	fail   bool
}

func (r *flakeyFXResolver) USDPriceAt(_ context.Context, asset c.Asset, _ time.Time) (string, bool, error) {
	if r.fail {
		return "", false, errors.New("transient pg fault (deadlock/statement-timeout)")
	}
	p, ok := r.prices[asset.String()]
	if !ok {
		return "", false, nil
	}
	return p, true, nil
}

// TestUSDVolumeGenerationAwareNullPreservation is the proven-red regression:
// in production persist_per_source=true, a DEX trade
// is DOUBLE-WRITTEN at deriveGeneration=0 by both the dispatcher's
// BatchInsertTrades and the projector's InsertTrade on the same store. If one
// writer resolves a usd_volume and the other races the same PK while its FX
// resolver hits a transient fault (returning nil), an upsert
// (`usd_volume = EXCLUDED.usd_volume` gated only by
// `derive_generation <= EXCLUDED.derive_generation`, equal included) overwrote
// the populated value with NULL — permanently deflating the pair's volume.
//
// The fix is GENERATION-AWARE, not a blanket COALESCE: at the LIVE / equal
// generation a NULL incoming must NOT regress a populated value, but a
// strictly-HIGHER-generation re-derive is still allowed to write an honest
// NULL (store.go:147-155, the tier-3b de-poisoning case). This test pins both:
//
//   - EQUAL (gen-0) NULL must PRESERVE the stored value. FAILS on the unfixed
//     `usd_volume = EXCLUDED.usd_volume` (row goes NULL). To reproduce red:
//     revert the CASE in both writers to `usd_volume = EXCLUDED.usd_volume`.
//   - HIGHER-generation NULL must CLEAR the stored value. FAILS on the naive
//     unconditional `COALESCE(EXCLUDED.usd_volume, trades.usd_volume)` the
//     skeptic panel rejected (which would block the honest de-poisoning NULL).
func TestUSDVolumeGenerationAwareNullPreservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A fresh SEP-41 base with no market (never resolvable) and a quote
	// token the resolver prices — the tier-3 FX path, where a transient miss
	// yields a nil usd_volume for the racing writer.
	base, err := c.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("base asset: %v", err)
	}
	quote, err := c.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("quote asset: %v", err)
	}
	pair, err := c.NewPair(base, quote)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	const (
		source = "soroswap" // SubclassDEX
		nonce  = 42
	)
	ledger := int64(50_000_000 + nonce)

	// 2.5 quote units (25,000,000 stroops at 1e7) x $2.00 = $5.00 — a small,
	// below-ceiling single-leg print, so W1-flow-price-serve-1's bound does
	// not apply and the value populates.
	res := &flakeyFXResolver{prices: map[string]string{quote.String(): "2.00"}}
	store.SetUSDVolumeFXResolver(res)

	readUSD := func() sql.NullString {
		const q = `SELECT usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
		var uv sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, source, ledger).Scan(&uv); err != nil {
			t.Fatalf("read usd_volume: %v", err)
		}
		return uv
	}
	isFive := func(uv sql.NullString) bool {
		return uv.Valid && (uv.String == "5.00000000" || uv.String == "5")
	}

	// Writer A, gen 0: resolves the quote leg → usd_volume = $5.00 lands.
	trA := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trA); err != nil {
		t.Fatalf("InsertTrade (writer A): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("writer A: usd_volume = %v, want $5.00 populated", uv)
	}

	// Writer B, gen 0: same PK, but its resolver hits a transient fault →
	// computes NULL. At EQUAL generation the fix must PRESERVE $5.00.
	// Overwriting with `usd_volume = EXCLUDED.usd_volume` would null this row.
	res.fail = true
	trB := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trB); err != nil {
		t.Fatalf("InsertTrade (writer B, transient miss): %v", err)
	}
	if uv := readUSD(); !isFive(uv) {
		t.Fatalf("EQUAL-generation NULL must NOT regress a populated value: "+
			"usd_volume = %v, want $5.00 preserved (W1-flowtradeingest-1)", uv)
	}

	// Higher-generation honest NULL (the tier-3b de-poisoning case): a
	// properly-wired re-derive at gen 5 that legitimately computes NULL must
	// be allowed to CLEAR the value. The reDeriveNullVolumeGuard requires
	// resolution to be installed for a gen>0 NULL write; install it (no-pegs
	// no-op is enough to flip the installed flag).
	if err := timescale.InstallUSDVolumeResolution(store, nil, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}
	store.SetDeriveGeneration(5)
	// Honest NULL = the re-derive legitimately finds no price (not-found,
	// nil error). A resolver ERROR is refused in re-derive mode instead.
	res.fail = false
	delete(res.prices, quote.String())
	trC := mkIntegrationTrade(source, nonce, ts, pair, 1_000_000_000, 25_000_000)
	if err := store.InsertTrade(ctx, trC); err != nil {
		t.Fatalf("InsertTrade (writer C, gen-5 honest NULL): %v", err)
	}
	if uv := readUSD(); uv.Valid {
		t.Fatalf("HIGHER-generation re-derive must be able to write an honest NULL "+
			"(de-poisoning): usd_volume = %q, want NULL — a blanket COALESCE would "+
			"wrongly preserve it", uv.String)
	}
}

// TestInsertTrade_PopulatesUSDVolume proves the L2.2 caveat fix
// shipped end-to-end: a binance + fiat:USD trade lands with a
// non-NULL `usd_volume` column matching the expected sum/1e8
// conversion, and an on-chain trade lands with `usd_volume IS NULL`.
func TestInsertTrade_PopulatesUSDVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := c.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := c.NewPair(xlm, usd)
	xlmUSDC, _ := c.NewPair(xlm,
		func() c.Asset {
			a, _ := c.NewCryptoAsset("USDC")
			return a
		}())

	// Anchor in the past for deterministic queries.
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	// Binance + fiat:USD: usd_volume = 12_000_000 / 1e8 = 0.12.
	binTrade := mkIntegrationTrade("binance", 1, ts, xlmUSD, 100_000_000, 12_000_000)
	// Soroswap + USDC (on-chain DEX): out of scope → usd_volume NULL.
	swapTrade := mkIntegrationTrade("soroswap", 2, ts, xlmUSDC, 100_000_000, 12_000_000)

	for _, tr := range []c.Trade{binTrade, swapTrade} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	const q = `SELECT usd_volume FROM trades WHERE source = $1 AND ledger = $2`

	t.Run("binance + fiat:USD: usd_volume populated", func(t *testing.T) {
		var v sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, "binance", binTrade.Ledger).Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !v.Valid {
			t.Fatal("usd_volume = NULL, want a populated value")
		}
		// FloatString(8) on 12_000_000/1e8 → "0.12000000"; Postgres
		// NUMERIC may render as "0.12000000" or trim trailing zeros
		// depending on the column scale — accept either form.
		if v.String != "0.12000000" && v.String != "0.12" {
			t.Errorf("usd_volume = %q, want 0.12 or 0.12000000", v.String)
		}
	})

	t.Run("soroswap + USDC (on-chain): usd_volume NULL", func(t *testing.T) {
		var v sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, "soroswap", swapTrade.Ledger).Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if v.Valid {
			t.Errorf("usd_volume = %q, want NULL (on-chain source out of scope)", v.String)
		}
	})
}

// TestInsertTrade_L76XLMBaseAnchorPopulatesUSDVolume proves the XLM-base
// anchor end-to-end: with the FX resolver wired (the r1
// production shape whenever `[trades].usd_pegged_classic_assets` is
// non-empty), a pure-Soroban SEP-41 trade stored as base=XLM,
// quote=TOKEN — the orientation [timescale.Store.Volume24hUSDForAsset]'s
// insert-time tier 3 can't cover, since TOKEN has no direct
// USD-pegged market — now lands a non-NULL `usd_volume` at INSERT
// time via the tier-4 XLM-base anchor, not just via the query-time
// [timescale.Store.SorobanVolume24hUSDForAsset] fallback.
//
// Fixture (one closed 1-minute bucket ~2h back):
//   - native/USDC  vwap 0.5           → the XLM→USD anchor (1 XLM = $0.50)
//   - XLM/token    10 XLM based       → tier-4 anchor: 10 * 0.5 = $5.00
//
// Because usd_volume is now populated AT INSERT, it propagates through
// prices_1m's `volume_usd` column — so even the PLAIN
// Volume24hUSDForAsset reader (which only ever summed the insert-time
// column) now reports the anchored figure, and the anchored
// SorobanVolume24hUSDForAsset reader's per-trade COALESCE(usd_volume, …) takes
// the same row without re-deriving it — no double count.
func TestInsertTrade_L76XLMBaseAnchorPopulatesUSDVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", usdcIssuer)
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}

	// Recognise classic USDC as a USD peg so the anchor trade's own
	// leg resolves, then wire the FX resolver on the same peg list —
	// the r1 production shape (cmd/stellarindex-indexer/main.go).
	spec, err := timescale.NewUSDVolumeQuoteSpec([]string{"USDC-" + usdcIssuer}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	resolver, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs:   []string{"USDC-" + usdcIssuer},
		Freshness: -1, // disabled — deterministic against the 2h-old fixture
	})
	if err != nil {
		t.Fatalf("NewVWAPUSDFXResolver: %v", err)
	}
	store.SetUSDVolumeFXResolver(resolver)

	xlmUSDC, _ := c.NewPair(xlm, usdc)   // anchor: vwap = 0.5
	xlmToken, _ := c.NewPair(xlm, token) // tier-4 case: base=XLM, quote=pure SEP-41

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	anchorTrade := mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000) // 100 XLM / 50 USDC → vwap 0.5
	xlmBaseTrade := mkIntegrationTrade("soroswap", 2, ts, xlmToken, 100_000_000, 300)         // 10 XLM based, 300 (arbitrary) token quote

	// Insert + refresh the anchor FIRST, and only then insert the
	// tier-4 trade: [timescale.VWAPUSDFXResolver] reads `prices_1m`
	// (the CAGG), not raw `trades`, so the XLM/USD anchor must
	// already be materialised by the time InsertTrade's synchronous
	// tier-4 lookup runs — matching production, where the anchor
	// leg's bucket was refreshed well before a brand-new trade
	// arrives.
	if err := store.InsertTrade(ctx, anchorTrade); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m (anchor): %v", err)
	}
	if err := store.InsertTrade(ctx, xlmBaseTrade); err != nil {
		t.Fatalf("InsertTrade xlmBaseTrade: %v", err)
	}

	// The tier-4 trade's usd_volume column is populated at INSERT
	// time — not NULL, unlike the pre-L7.6 behaviour
	// (test/integration/soroban_volume_test.go's un-resolved fixture).
	const q = `SELECT usd_volume FROM trades WHERE source = $1 AND ledger = $2`
	var v sql.NullString
	if err := store.DB().QueryRowContext(ctx, q, "soroswap", xlmBaseTrade.Ledger).Scan(&v); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !v.Valid {
		t.Fatal("usd_volume = NULL, want a populated value (tier-4 XLM-base anchor)")
	}
	if got := mustFloat(t, v.String); got < 4.99 || got > 5.01 {
		t.Errorf("usd_volume = %s (%.4f), want ~5.00 (10 XLM * $0.50)", v.String, got)
	}

	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Plain reader now ALSO sees the tier-4 leg — it sums whatever
	// landed in usd_volume, and this trade's column is no longer NULL.
	plain, _, err := store.Volume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, plain); got < 4.99 || got > 5.01 {
		t.Errorf("plain Volume24hUSDForAsset = %s (%.4f), want ~5.00", plain, got)
	}

	// Anchored reader takes the stored usd_volume for the SAME row —
	// no double count against its own base_asset='native' CASE.
	anchored, _, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, anchored); got < 4.99 || got > 5.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want ~5.00 (no double count)", anchored, got)
	}
}

func candleRows(t *testing.T, source string, pair c.Pair, hourStart time.Time, g time.Duration) []c.Trade {
	t.Helper()
	// Pair-specific: a shared symbol gives two pairs the same PK at one
	// (source, closeTs), so the second seed upserts over the first.
	symbol := pair.Base.Code + pair.Quote.Code
	var out []c.Trade
	for open := hourStart; open.Before(hourStart.Add(time.Hour)); open = open.Add(g) {
		closeTs := open.Add(g - time.Second)
		h, err := scale.CandleTxHash(symbol, closeTs.Unix(), g)
		if err != nil {
			t.Fatalf("CandleTxHash: %v", err)
		}
		out = append(out, c.Trade{
			Source:      source,
			TxHash:      h,
			Timestamp:   closeTs,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(17_000_000)),
		})
	}
	return out
}

// Store.TradesInWindowOutside is what backfill-external's -allow-overlap
// guard refuses on. Over a window holding an hour of stored 1m candles, a
// 1h re-run must see all 60 minute rows as ones it would not overwrite
// (writing it would double the hour's volume), while a 1m re-run sees
// none. Rows for another venue, pair or outside the window never count.
func TestTradesInWindowOutside_CandleGranularityReRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlmUSD := earliestPair(t, "crypto:XLM", "fiat:USD")
	xlmEUR := earliestPair(t, "crypto:XLM", "fiat:EUR")
	hour := time.Date(2025, 4, 18, 18, 0, 0, 0, time.UTC)
	from, to := hour, hour.Add(time.Hour)

	minutes := candleRows(t, "kraken", xlmUSD, hour, time.Minute)
	seed := append([]c.Trade{}, minutes...)
	seed = append(seed, candleRows(t, "bitstamp", xlmUSD, hour, time.Minute)...)
	seed = append(seed, candleRows(t, "kraken", xlmEUR, hour, time.Minute)...)
	seed = append(seed, candleRows(t, "kraken", xlmUSD, to, time.Minute)[0]) // `to` is exclusive
	for i, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d InsertTrade: %v", i, err)
		}
	}

	hourly := candleRows(t, "kraken", xlmUSD, hour, time.Hour)
	n, first, err := store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, hourly)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (1h batch): %v", err)
	}
	if n != 60 || !first.Equal(minutes[0].Timestamp) {
		t.Fatalf("1h batch over stored 1m rows = (%d, %v), want (60, %v)", n, first, minutes[0].Timestamp)
	}

	n, _, err = store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, minutes)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (1m batch): %v", err)
	}
	if n != 0 {
		t.Fatalf("same-granularity re-run sees %d foreign row(s), want 0", n)
	}

	// An empty batch sees every stored row in scope and nothing else.
	n, _, err = store.TradesInWindowOutside(ctx, "kraken", xlmUSD, from, to, nil)
	if err != nil {
		t.Fatalf("TradesInWindowOutside (empty batch): %v", err)
	}
	if n != 60 {
		t.Fatalf("empty batch sees %d row(s), want the 60 in scope", n)
	}
}

// TestZeroLegAdmission drives an SDEX zero-quote fill through the real
// migrated schema and InsertTrade: the row is stored, /v1/history serves it
// with price null, prices_1m counts it without pricing it, and the
// /v1/price last-trade fallback (LatestTradesForPair) never returns it.
func TestZeroLegAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", priceableIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	mk := func(ledger uint32, off time.Duration, base, quote int64) c.Trade {
		return c.Trade{
			Source: "sdex", Ledger: ledger, TxHash: fmt.Sprintf("%064x", ledger),
			Timestamp: t0.Add(off), Pair: pair,
			BaseAmount:  c.NewAmount(big.NewInt(base)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
		}
	}
	priced := mk(61_000_000, 5*time.Second, 1_000_000_000, 120_000_000) // 0.12
	zeroQuote := mk(61_000_001, 50*time.Second, 5_000_000_000, 0)       // newest
	for _, tr := range []c.Trade{priced, zeroQuote} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade(ledger %d): %v", tr.Ledger, err)
		}
	}

	t.Run("stored", func(t *testing.T) {
		var quote string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT quote_amount::text FROM trades WHERE ledger = $1 AND source = 'sdex'`,
			zeroQuote.Ledger).Scan(&quote); err != nil {
			t.Fatalf("zero-quote row not stored: %v", err)
		}
		if quote != "0" {
			t.Errorf("stored quote_amount = %s, want 0", quote)
		}
	})

	t.Run("history price null", func(t *testing.T) {
		srv := v1.New(v1.Options{History: apiHistoryAdapter{s: store}})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		var env struct {
			Data []v1.TradeRow `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/history?base=native&quote="+usdc.String()+
			"&from="+t0.Add(-time.Minute).Format(time.RFC3339)+
			"&to="+t0.Add(2*time.Minute).Format(time.RFC3339), &env)
		if len(env.Data) != 2 {
			t.Fatalf("history returned %d rows, want 2", len(env.Data))
		}
		for _, r := range env.Data {
			switch r.Ledger {
			case zeroQuote.Ledger:
				if r.Price != nil {
					t.Errorf("zero-quote row price = %q, want null", *r.Price)
				}
			case priced.Ledger:
				if r.Price == nil || *r.Price != "0.1200000000" {
					t.Errorf("priced row price = %v, want 0.1200000000", r.Price)
				}
			default:
				t.Errorf("unexpected ledger %d", r.Ledger)
			}
		}
	})

	t.Run("prices_1m counts but does not price", func(t *testing.T) {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('prices_1m', $1::timestamptz, $2::timestamptz)`,
			t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil {
			t.Fatalf("refresh prices_1m: %v", err)
		}
		got := readPriceableRow(t, ctx, store.DB(), "1m",
			ohlcDustPair{base: pair.Base.String(), quote: pair.Quote.String()}, t0)
		assertNumeric(t, "vwap", got.vwap, "0.12")
		assertNumeric(t, "last_price", got.last, "0.12")
		assertNumeric(t, "low_price", got.low, "0.12")
		if got.tradeCount != 2 {
			t.Errorf("trade_count = %d, want 2", got.tradeCount)
		}
	})

	t.Run("LatestTradesForPair skips the zero leg", func(t *testing.T) {
		got, err := store.LatestTradesForPair(ctx, pair, 1)
		if err != nil {
			t.Fatalf("LatestTradesForPair: %v", err)
		}
		if len(got) != 1 || got[0].Ledger != priced.Ledger {
			t.Fatalf("LatestTradesForPair = %+v, want only ledger %d (the newest priceable trade)", got, priced.Ledger)
		}
		snap, ok := v1.LastTradeToSnapshot(got[0], 7)
		if !ok || snap.Price != "0.1200000" {
			t.Errorf("LastTradeToSnapshot = (%q, %v), want (0.1200000, true)", snap.Price, ok)
		}
	})
}
