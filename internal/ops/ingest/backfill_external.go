package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalbinance "github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	externalbitstamp "github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	externalcoinbase "github.com/Stellar-Index/StellarIndex/internal/sources/external/coinbase"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
	externalpoloniex "github.com/Stellar-Index/StellarIndex/internal/sources/external/poloniex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// externalInsertBudget bounds the DATABASE half of backfill-external on its own clock, so a long paced venue walk cannot leave nothing
// to write the fills it fetched.
const externalInsertBudget = 12 * time.Hour

// backfillExternal drives the Backfiller interface for one external venue; the operator passes the venue-native symbol ("XLMUSDT"
// Binance, "XLM/USD" Kraken, "xlmusd" Bitstamp, "XLM-USD" Coinbase).
//
//nolint:gocognit,gocyclo,funlen // ops-CLI subcommand: flag parsing + venue dispatch + insert-loop in one function is the most readable shape
func backfillExternal(args []string) error {
	fs := flag.NewFlagSet("backfill-external", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Venue: binance | kraken | bitstamp | coinbase | poloniex_via_btc (required)")
	pairSym := fs.String("pair", "", "Venue-native symbol, e.g. XLMUSDT / XLM/USD / xlmusd / XLM-USD (required)")
	fromStr := fs.String("from", "", "Start time, RFC 3339 (required, e.g. 2024-01-01T00:00:00Z)")
	toStr := fs.String("to", "", "End time, RFC 3339 (required, e.g. 2024-12-31T00:00:00Z)")
	granStr := fs.String("granularity", "1h", "Candle granularity as a Go duration (1m / 15m / 1h / 4h / 1d / 1w)")
	rawTrades := fs.Bool("raw-trades", false, "kraken only: walk the /Trades fills endpoint instead of /OHLC — the deep-history path (OHLC serves only the most recent 720 candles, so an older window returns none). Slower (rate-limited pagination) but reaches the pair's full history with exact per-fill prices.")
	allowOverlap := fs.Bool("allow-overlap", false, "Write even though the trades table already holds rows for this source+pair inside [-from, -to). Rows from another path (live stream, candles vs fills) carry a different tx_hash and would be counted twice; use only to re-run or resume a window this command itself wrote. A candle run is still refused if the window holds a row it would not overwrite (e.g. one written at another -granularity).")
	gate := opsutil.RegisterWriteGate(fs)
	progressEvery := fs.Int("progress-every", 1000, "Print a progress line every N trades inserted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *source == "" || *pairSym == "" || *fromStr == "" || *toStr == "" {
		fs.Usage()
		return fmt.Errorf("-config, -source, -pair, -from, -to all required")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	from, err := time.Parse(time.RFC3339, *fromStr)
	if err != nil {
		return fmt.Errorf("parse -from %q: %w", *fromStr, err)
	}
	to, err := time.Parse(time.RFC3339, *toStr)
	if err != nil {
		return fmt.Errorf("parse -to %q: %w", *toStr, err)
	}
	if !from.Before(to) {
		return fmt.Errorf("-from %v must be before -to %v", from, to)
	}
	if *source == externalpoloniex.SourceName {
		if err := externalpoloniex.CheckRange(from, to); err != nil {
			return err
		}
	}
	granularity, err := time.ParseDuration(*granStr)
	if err != nil {
		return fmt.Errorf("parse -granularity %q: %w", *granStr, err)
	}

	backfiller, pair, err := buildBackfiller(*source, *pairSym)
	if err != nil {
		return err
	}
	// Before the walk, which can take a day: the probe is cheap and the
	// refusal is the common outcome for a window that reaches live data.
	if !dryRun && !*allowOverlap {
		if err := checkStoredOverlap(*cfgPath, *source, pair, from, to); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "backfill-external: source=%s pair=%s granularity=%v from=%s to=%s dry-run=%v\n",
		*source, pair.String(), granularity,
		from.Format(time.RFC3339), to.Format(time.RFC3339), dryRun)

	if *rawTrades {
		kr, ok := backfiller.(*externalkraken.Streamer)
		if !ok {
			return fmt.Errorf("-raw-trades is kraken-only (venue %q has no fills-pagination path)", *source)
		}
		return backfillKrakenRawTrades(kr, pair, from, to, dryRun, *cfgPath, *progressEvery)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	t0 := time.Now()
	trades, err := backfiller.Backfill(ctx, pair, from, to, granularity)
	trades, dropped := dropUnsettledCandles(trades, to, time.Now())
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "backfill-external: dropped %d candle(s) not closed by min(-to, now)\n", dropped)
	}
	resumeFrom, partial := partialFetchResume(trades, err)
	if err != nil && !partial {
		return fmt.Errorf("backfill: %w", err)
	}
	if partial {
		fmt.Fprintf(os.Stderr, "backfill-external: WARNING venue walk ended early (%v) holding %d trade(s) — writing them, then exiting non-zero. Resume with -from %s\n",
			err, len(trades), resumeFrom.UTC().Format(time.RFC3339Nano))
	}
	fmt.Fprintf(os.Stderr, "backfill-external: fetched %d trades in %v\n",
		len(trades), time.Since(t0).Round(time.Millisecond))

	if dryRun {
		summariseDryRun(trades)
		if partial {
			return partialWalkError(len(trades), resumeFrom, nil)
		}
		return nil
	}

	// The walk's context may already be expired (that is exactly the
	// case `partial` covers), so the write half runs on its own.
	insCtx, insCancel := context.WithTimeout(context.Background(), externalInsertBudget)
	defer insCancel()

	store, err := openBackfillStore(insCtx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if *allowOverlap {
		if err := refuseForeignRows(insCtx, store, *source, pair, from, walkedTo(to, resumeFrom, partial), trades); err != nil {
			return err
		}
	}
	insErr := insertBackfilledTrades(insCtx, store, trades, *progressEvery, os.Stderr, t0)
	if partial {
		return partialWalkError(len(trades), resumeFrom, insErr)
	}
	return insErr
}

// openBackfillStore opens the trades store wired exactly as live ingest
// writes, so a backfilled row is derived the same way a streamed one is.
func openBackfillStore(ctx context.Context, cfgPath string) (*timescale.Store, error) {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return nil, err
	}
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}

	// Re-derive path (migration 0109): stamp a positive derive_generation so
	// a corrected trade re-derive UPDATEs the stored row in place
	// (usd_volume et al.) and wins over the live gen-0 value.
	store.SetDeriveGeneration(time.Now().Unix())

	// Mirror the indexer's USD-volume wiring. MUST install every tier: a
	// store missing the FX resolver computes NULL and then wins the upsert,
	// overwriting correct stored values. See InstallUSDVolumeResolution.
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
		cfg.PricingGuard.FiatPeggedClassicAssets,
	); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// dropUnsettledCandles keeps only candle trades whose bar closed before min(to, now).
// Candles are stamped at their last instant (close-1s, or close-1ms for binance), so truncating the cutoff to the second makes "stamp < cutoff" mean "bar end <= cutoff".
func dropUnsettledCandles(trades []canonical.Trade, to, now time.Time) ([]canonical.Trade, int) {
	cutoff := to
	if now.Before(cutoff) {
		cutoff = now
	}
	cutoff = cutoff.Truncate(time.Second)
	kept := trades[:0]
	for _, tr := range trades {
		if tr.Timestamp.Before(cutoff) {
			kept = append(kept, tr)
		}
	}
	return kept, len(trades) - len(kept)
}

// partialFetchResume reports whether a venue walk that ended early (context expiry or ErrDepthExceeded) still returned usable fills, and the instant to resume from.
// The high-water timestamp is the cursor; inserts are idempotent (ON CONFLICT), so a re-run costs at most one duplicate page. Any other error is a real fault and must not be salvaged.
func partialFetchResume(trades []canonical.Trade, err error) (time.Time, bool) {
	if err == nil || len(trades) == 0 {
		return time.Time{}, false
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, externalkraken.ErrDepthExceeded) {
		return time.Time{}, false
	}
	high := trades[0].Timestamp
	for _, tr := range trades[1:] {
		if tr.Timestamp.After(high) {
			high = tr.Timestamp
		}
	}
	return high, true
}

// partialWalkError is the non-zero exit for a salvaged run: the range was NOT covered, so it must never exit 0 or a scripted chunked
// walk cannot tell a truncated slice from a complete one.
func partialWalkError(n int, resumeFrom time.Time, insErr error) error {
	at := resumeFrom.UTC().Format(time.RFC3339Nano)
	if insErr != nil {
		return fmt.Errorf("backfill-external: venue walk ended early with %d trade(s) salvaged AND the write had faults (%w) — range incomplete, resume with -from %s -allow-overlap", n, insErr, at)
	}
	return fmt.Errorf("backfill-external: venue walk ended early — %d trade(s) salvaged but the requested range is NOT complete; resume with -from %s -allow-overlap", n, at)
}

// errBackfillOverlap marks a refused run whose window already holds rows.
var errBackfillOverlap = errors.New("backfill window overlaps stored trades")

// storedTradeProbe is the storage seam refuseStoredOverlap depends on.
type storedTradeProbe interface {
	EarliestTradeInWindow(ctx context.Context, source string, pair canonical.Pair, from, to time.Time) (time.Time, bool, error)
}

// refuseStoredOverlap refuses a window that already holds trades for (source, pair).
// Writing over live rows would double-count volume with no VWAP, outlier or divergence guard; any stored row refuses because the tool cannot tell its own rows from others, and -allow-overlap covers its own re-runs.
func refuseStoredOverlap(ctx context.Context, probe storedTradeProbe, source string, pair canonical.Pair, from, to time.Time) error {
	first, found, err := probe.EarliestTradeInWindow(ctx, source, pair, from, to)
	if err != nil {
		return fmt.Errorf("backfill-external: overlap probe: %w", err)
	}
	if !found {
		return nil
	}
	at := first.UTC().Format(time.RFC3339Nano)
	return fmt.Errorf("backfill-external: %w: %s %s already has stored rows from %s inside [-from, -to); "+
		"end the run with -to %s, or pass -allow-overlap only to re-run a window this command wrote",
		errBackfillOverlap, source, pair.String(), at, at)
}

// errBackfillForeignRows marks an -allow-overlap run refused because the
// window holds stored rows the run would not overwrite.
var errBackfillForeignRows = errors.New("backfill window holds rows this run would not rewrite")

// foreignTradeProbe is the storage seam refuseForeignRows depends on.
type foreignTradeProbe interface {
	TradesInWindowOutside(ctx context.Context, source string, pair canonical.Pair, from, to time.Time, keep []canonical.Trade) (int64, time.Time, error)
}

// refuseForeignRows bounds -allow-overlap to re-running a window this command wrote at the same granularity.
// A stored row outside the run's identities would sit beside the new rows and double the window's volume, so the write is refused.
func refuseForeignRows(ctx context.Context, probe foreignTradeProbe, source string, pair canonical.Pair, from, to time.Time, trades []canonical.Trade) error {
	n, first, err := probe.TradesInWindowOutside(ctx, source, pair, from, to, trades)
	if err != nil {
		return fmt.Errorf("backfill-external: overlap probe: %w", err)
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("backfill-external: %w: %s %s has %d stored row(s) from %s inside the walked window that this run's candles would not overwrite "+
		"(another -granularity, the live stream, or pre-granularity candle identities); writing would count their volume twice. "+
		"Re-run at the granularity that wrote the window, or end the run with -to %s",
		errBackfillForeignRows, source, pair.String(), n, first.UTC().Format(time.RFC3339Nano), first.UTC().Format(time.RFC3339Nano))
}

// walkedTo is the exclusive end of the range a walk covered: the requested -to, or just past the high-water trade of an early-ended
// walk.
func walkedTo(to, resumeFrom time.Time, partial bool) time.Time {
	if partial && resumeFrom.Before(to) {
		return resumeFrom.Add(time.Microsecond)
	}
	return to
}

// checkStoredOverlap runs refuseStoredOverlap against the configured store.
func checkStoredOverlap(cfgPath, source string, pair canonical.Pair, from, to time.Time) error {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()
	return refuseStoredOverlap(ctx, store, source, pair, from, to)
}

// windowPlan slices a fills walk into bounded spans of venue history.
type windowPlan struct {
	size time.Duration // history held in memory at once, and the checkpoint grain
	// overlap is re-fetched ahead of each window so a boundary fill survives an inclusive or exclusive `since` cursor; duplicates are
	// dropped by trade identity before the write.
	overlap time.Duration
	// pace spaces windows apart: the venue walker paces only between its
	// own pages, so back-to-back windows would otherwise burst requests.
	pace time.Duration
}

// krakenRawWindows bounds a -raw-trades run to one day of fills in memory: a pair's deep history (~10^7 fills) held whole is killed by
// the memory cap. pace mirrors kraken's tradesRateLimit.
var krakenRawWindows = windowPlan{size: 24 * time.Hour, overlap: time.Second, pace: 1100 * time.Millisecond}

type (
	windowFetch func(ctx context.Context, from, to time.Time) ([]canonical.Trade, error)
	windowSink  func(trades []canonical.Trade) error
)

// backfillKrakenRawTrades walks kraken fills one window at a time, writing each before fetching the next, so memory is bounded by a
// window.
func backfillKrakenRawTrades(kr *externalkraken.Streamer, pair canonical.Pair, from, to time.Time, dryRun bool, cfgPath string, progressEvery int) error {
	// Deep pagination is slow by design (venue rate limit), so the walk
	// gets a day rather than the 30-minute candle budget.
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	fetch := func(ctx context.Context, f, t time.Time) ([]canonical.Trade, error) {
		return kr.BackfillTrades(ctx, pair, f, t)
	}

	if dryRun {
		var sum dryRunSummary
		err := walkWindowed(ctx, krakenRawWindows, from, to, fetch, func(trades []canonical.Trade) error {
			sum.add(trades)
			return nil
		}, os.Stderr)
		sum.print()
		return err
	}

	openCtx, openCancel := context.WithTimeout(context.Background(), time.Minute)
	defer openCancel()
	store, err := openBackfillStore(openCtx, cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	t0 := time.Now()
	return walkWindowed(ctx, krakenRawWindows, from, to, fetch, func(trades []canonical.Trade) error {
		insCtx, insCancel := context.WithTimeout(context.Background(), externalInsertBudget)
		defer insCancel()
		return insertBackfilledTrades(insCtx, store, trades, progressEvery, os.Stderr, t0)
	}, os.Stderr)
}

// walkWindowed fetches [from, to) window by window, handing each to sink before the next.
// The stopping window's partial fills are discarded (writing them would trip refuseStoredOverlap on resume). A per-row write fault keeps the exit non-zero without stopping; an infra write fault stops the walk.
func walkWindowed(ctx context.Context, plan windowPlan, from, to time.Time, fetch windowFetch, sink windowSink, log io.Writer) error {
	var (
		written int
		faults  []error
		seen    map[string]struct{}
	)
	for start := from; start.Before(to); {
		end, fetchFrom := windowBounds(plan, from, to, start)
		if start.After(from) {
			if err := waitPace(ctx, plan.pace); err != nil {
				return windowStopError(start, end, written, append(faults, err))
			}
		}
		trades, err := fetch(ctx, fetchFrom, end)
		if err != nil {
			return windowStopError(start, end, written, append(faults, err))
		}
		trades, seen = dropSeen(trades, seen, end.Add(-plan.overlap))
		if len(trades) > 0 {
			if err := sink(trades); err != nil {
				if timescale.IsInfraError(err) {
					return windowStopError(start, end, written, append(faults, err))
				}
				faults = append(faults, err)
			}
		}
		written += len(trades)
		_, _ = fmt.Fprintf(log, "backfill-external: window %s -> %s done, %d trade(s) so far — checkpoint: resume with -from %s\n",
			start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), written, end.UTC().Format(time.RFC3339Nano))
		start = end
	}
	return errors.Join(faults...)
}

// windowBounds returns the window starting at start, clamped to to, and
// the fetch start that re-reads plan.overlap before it, clamped to from.
func windowBounds(plan windowPlan, from, to, start time.Time) (end, fetchFrom time.Time) {
	end = start.Add(plan.size)
	if end.After(to) {
		end = to
	}
	fetchFrom = start.Add(-plan.overlap)
	if fetchFrom.Before(from) {
		fetchFrom = from
	}
	return end, fetchFrom
}

// dropSeen removes trades the previous window's overlap already handed to the sink and returns identities at or after tailFrom that the
// next overlap may repeat.
func dropSeen(trades []canonical.Trade, seen map[string]struct{}, tailFrom time.Time) ([]canonical.Trade, map[string]struct{}) {
	next := make(map[string]struct{})
	kept := trades[:0]
	for _, tr := range trades {
		if _, dup := seen[tr.TxHash]; dup {
			continue
		}
		kept = append(kept, tr)
		if !tr.Timestamp.Before(tailFrom) {
			next[tr.TxHash] = struct{}{}
		}
	}
	return kept, next
}

func waitPace(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// windowStopError is the non-zero exit of a windowed walk that did not reach -to; it carries every cause (errors.Is sees each) and the
// resume point, the window start, before which everything is written.
func windowStopError(start, end time.Time, written int, causes []error) error {
	return fmt.Errorf("backfill-external: walk stopped in window %s -> %s with %d trade(s) written — range incomplete, resume with -from %s: %w",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), written,
		start.UTC().Format(time.RFC3339Nano), errors.Join(causes...))
}

// tradeInserter is the storage seam for insertBackfilledTrades, so its failure policy (infra-abort vs data-fault-skip, non-zero exit on
// any drop) is unit-testable with a fake.
type tradeInserter interface {
	InsertTrade(ctx context.Context, t canonical.Trade) error
	BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error
}

// backfillInsertChunk is the rows per BatchInsertTrades call: well under the
// store's one-statement cap, so a chunk commits or fails as a whole.
const backfillInsertChunk = 1000

type insertTally struct{ inserted, skipped int }

// insertBackfilledTrades is the DB-seamed core of the insert loop.
// An infra fault aborts immediately (it hits every insert, so counting skips would misreport an outage as data faults); a per-row fault is skipped and logged, but any skip returns non-nil because a dropped row has no dead-letter.
func insertBackfilledTrades(ctx context.Context, store tradeInserter, trades []canonical.Trade, progressEvery int, log io.Writer, t0 time.Time) error {
	var n insertTally
	for start := 0; start < len(trades); start += backfillInsertChunk {
		end := min(start+backfillInsertChunk, len(trades))
		before := n.inserted
		if err := insertBackfillChunk(ctx, store, trades, start, end, &n, log); err != nil {
			return err
		}
		if progressEvery > 0 && n.inserted/progressEvery > before/progressEvery {
			_, _ = fmt.Fprintf(log, "  ... %d inserted, %d skipped\n", n.inserted, n.skipped)
		}
	}
	_, _ = fmt.Fprintf(log, "backfill-external: done — %d inserted, %d skipped in %v\n",
		n.inserted, n.skipped, time.Since(t0).Round(time.Millisecond))
	return opsutil.RunOutcome{
		Verb: "backfill-external", Noun: "trade",
		Attempted: len(trades), Written: n.inserted, Failed: n.skipped,
	}.Err()
}

// insertBackfillChunk writes trades[start:end] in one batch. Rows Validate rejects go through InsertTrade
// because BatchInsertTrades drops them silently; a non-infra batch fault is retried row by row (inserts are idempotent).
func insertBackfillChunk(ctx context.Context, store tradeInserter, trades []canonical.Trade, start, end int, n *insertTally, log io.Writer) error {
	valid := make([]canonical.Trade, 0, end-start)
	var rowByRow []int
	for i := start; i < end; i++ {
		if trades[i].Validate() != nil {
			rowByRow = append(rowByRow, i)
			continue
		}
		valid = append(valid, trades[i])
	}
	if len(valid) > 0 {
		err := store.BatchInsertTrades(ctx, valid)
		switch {
		case err == nil:
			n.inserted += len(valid)
		case timescale.IsInfraError(err):
			return infraAbortError(trades, start, *n, err)
		default:
			_, _ = fmt.Fprintf(log, "insert batch of trades %d-%d failed, isolating row by row: %v\n", start, end-1, err)
			rowByRow = rowByRow[:0]
			for i := start; i < end; i++ {
				rowByRow = append(rowByRow, i)
			}
		}
	}
	for _, i := range rowByRow {
		if err := store.InsertTrade(ctx, trades[i]); err != nil {
			if timescale.IsInfraError(err) {
				return infraAbortError(trades, i, *n, err)
			}
			n.skipped++
			_, _ = fmt.Fprintf(log, "insert trade %d (%s): %v\n", i, trades[i].TxHash, err)
			continue
		}
		n.inserted++
	}
	return nil
}

func infraAbortError(trades []canonical.Trade, i int, n insertTally, err error) error {
	return fmt.Errorf("backfill-external: aborting at trade %d/%d (%s) after infra fault — %d inserted, %d skipped so far, %d not attempted: %w",
		i, len(trades), trades[i].TxHash, n.inserted, n.skipped, len(trades)-i, err)
}

// buildBackfiller maps the -source flag to the venue's Backfiller, resolving the venue-native symbol via DefaultPairs; unknown sources or pairs return a clear error.
func buildBackfiller(source, symbol string) (external.Backfiller, canonical.Pair, error) {
	switch source {
	case externalbinance.SourceName:
		pm, err := externalbinance.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("binance pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalbinance.NewStreamer(pm), pair, nil
	case externalkraken.SourceName:
		pm, err := externalkraken.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("kraken pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalkraken.NewStreamer(pm), pair, nil
	case externalbitstamp.SourceName:
		pm, err := externalbitstamp.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("bitstamp pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalbitstamp.NewStreamer(pm), pair, nil
	case externalcoinbase.SourceName:
		pm, err := externalcoinbase.DefaultPairs()
		if err != nil {
			return nil, canonical.Pair{}, fmt.Errorf("coinbase pairs: %w", err)
		}
		pair, ok := pm[symbol]
		if !ok {
			return nil, canonical.Pair{}, unknownPairError(source, symbol, pm)
		}
		return externalcoinbase.NewStreamer(pm), pair, nil
	case externalpoloniex.SourceName:
		return buildPoloniexBackfiller(symbol)
	}
	return nil, canonical.Pair{}, fmt.Errorf("unknown -source %q (supported: binance, kraken, bitstamp, coinbase, poloniex_via_btc)", source)
}

// buildPoloniexBackfiller wires the XLM/BTC x BTC/USD derivation. The BTC
// leg is Bitstamp's daily BTC/USD, which reaches back before 2015.
func buildPoloniexBackfiller(symbol string) (external.Backfiller, canonical.Pair, error) {
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		return nil, canonical.Pair{}, err
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		return nil, canonical.Pair{}, err
	}
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		return nil, canonical.Pair{}, err
	}
	if symbol != "XLM/USD" {
		return nil, canonical.Pair{}, fmt.Errorf("-source %s supports only -pair XLM/USD, got %q", externalpoloniex.SourceName, symbol)
	}
	pm, err := externalbitstamp.DefaultPairs()
	if err != nil {
		return nil, canonical.Pair{}, fmt.Errorf("bitstamp pairs: %w", err)
	}
	btcPair, ok := pm["btcusd"]
	if !ok {
		return nil, canonical.Pair{}, unknownPairError(externalbitstamp.SourceName, "btcusd", pm)
	}
	return &externalpoloniex.Backfiller{BTCLeg: externalbitstamp.NewStreamer(pm), BTCPair: btcPair}, pair, nil
}

// unknownPairError prints the configured set so the operator can
// see the venue-specific symbol format without consulting docs.
func unknownPairError(source, want string, pm map[string]canonical.Pair) error {
	keys := make([]string, 0, len(pm))
	for k := range pm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("pair %q not in %s DefaultPairs — known symbols: %v", want, source, keys)
}

// summariseDryRun prints first/last trade timestamps, count and per-pair volume so the operator can sanity-check a range before a large
// insert.
func summariseDryRun(trades []canonical.Trade) {
	var sum dryRunSummary
	sum.add(trades)
	sum.print()
}

// dryRunSummary accumulates the dry-run view incrementally so a windowed
// walk can report on its whole range without holding it.
type dryRunSummary struct {
	count                 int
	first, last           canonical.Trade
	totalBase, totalQuote float64
}

func (s *dryRunSummary) add(trades []canonical.Trade) {
	for _, t := range trades {
		if s.count == 0 {
			s.first = t
		}
		s.last = t
		s.count++
		// Convert 10^8-scaled Amount to float for display. Precision
		// loss here is fine — it's a dry-run summary, not a computed
		// price.
		s.totalBase += amountToFloat(t.BaseAmount, 8)
		s.totalQuote += amountToFloat(t.QuoteAmount, 8)
	}
}

func (s *dryRunSummary) print() {
	if s.count == 0 {
		fmt.Println("(no trades in range)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FIELD\tVALUE")
	_, _ = fmt.Fprintf(w, "trade count\t%d\n", s.count)
	_, _ = fmt.Fprintf(w, "first ts\t%s\n", s.first.Timestamp.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "last  ts\t%s\n", s.last.Timestamp.Format(time.RFC3339))
	_, _ = fmt.Fprintf(w, "pair\t%s\n", s.first.Pair.String())
	_, _ = fmt.Fprintf(w, "total base volume\t%.8f\n", s.totalBase)
	_, _ = fmt.Fprintf(w, "total quote volume\t%.8f\n", s.totalQuote)
	if s.totalBase > 0 {
		_, _ = fmt.Fprintf(w, "vwap (quote/base)\t%.8f\n", s.totalQuote/s.totalBase)
	}
	_ = w.Flush()
}

// amountToFloat converts a canonical.Amount at the given decimal scale to float64 for display only; lossy, never for storage.
func amountToFloat(a canonical.Amount, decimals int) float64 {
	bi := a.BigInt()
	if bi == nil {
		return 0
	}
	// Build "INT.FRAC" then parse via strconv.
	s := bi.String()
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	cut := len(s) - decimals
	formatted := s[:cut] + "." + s[cut:]
	if neg {
		formatted = "-" + formatted
	}
	f, _ := strconv.ParseFloat(formatted, 64)
	return f
}

// ─── verify-decoders ─────────────────────────────────────────────
