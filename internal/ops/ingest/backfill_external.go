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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// externalInsertBudget bounds the DATABASE half of a backfill-external
// run, separately from the venue-walk budget.
//
// One shared deadline used to cover both halves, which made a long walk
// self-defeating: the fills endpoint is paced at one page per 1.1s, so a
// multi-year pair walk spends hours in pagination and then has whatever
// is left — possibly nothing — to write tens of millions of rows with.
// A walk that used its whole budget therefore discarded every fill it
// had just paid the venue rate limit to fetch. The insert loop gets its
// own clock so salvage is possible at all.
const externalInsertBudget = 12 * time.Hour

// backfillExternal drives the Backfiller interface for one external
// venue. Operator passes the venue-native symbol (the same shape
// each venue's Streamer would subscribe with): "XLMUSDT" for
// Binance, "XLM/USD" for Kraken, "xlmusd" for Bitstamp, "XLM-USD"
// for Coinbase. Keeps the CLI surface honest to venue conventions
// rather than inventing our own cross-venue normalisation.
//
//nolint:gocognit,gocyclo,funlen // ops-CLI subcommand: flag parsing + venue dispatch + insert-loop in one function is the most readable shape
func backfillExternal(args []string) error {
	fs := flag.NewFlagSet("backfill-external", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Venue: binance | kraken | bitstamp | coinbase (required)")
	pairSym := fs.String("pair", "", "Venue-native symbol, e.g. XLMUSDT / XLM/USD / xlmusd / XLM-USD (required)")
	fromStr := fs.String("from", "", "Start time, RFC 3339 (required, e.g. 2024-01-01T00:00:00Z)")
	toStr := fs.String("to", "", "End time, RFC 3339 (required, e.g. 2024-12-31T00:00:00Z)")
	granStr := fs.String("granularity", "1h", "Candle granularity as a Go duration (1m / 15m / 1h / 4h / 1d / 1w)")
	rawTrades := fs.Bool("raw-trades", false, "kraken only: walk the /Trades fills endpoint instead of /OHLC — the deep-history path (OHLC serves only the most recent 720 candles; board #44). Slower (rate-limited pagination) but reaches the pair's full history with exact per-fill prices.")
	allowOverlap := fs.Bool("allow-overlap", false, "Write even though the trades table already holds rows for this source+pair inside [-from, -to). Rows from another path (live stream, candles vs fills) carry a different tx_hash and would be counted twice; use only to re-run or resume a window this command itself wrote.")
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

	// Re-derive path (INV-3 / migration 0109): stamp a positive
	// derive_generation so a corrected trade re-derive UPDATEs the stored
	// row in place (usd_volume et al.) and wins over the live gen-0 value.
	store.SetDeriveGeneration(time.Now().Unix())

	// Mirror the indexer's USD-volume wiring so an ops-driven backfill
	// populates usd_volume exactly the way live ingest does. This MUST
	// install every tier: combined with the positive derive_generation
	// above, a store missing the FX resolver computes NULL and then wins
	// the upsert, overwriting correct stored values. (It previously
	// mirrored L2.2 phase 1 only — see InstallUSDVolumeResolution.)
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
	); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// partialFetchResume reports whether a venue walk that ended early —
// on context expiry, or on hitting a venue's historical-depth horizon
// (externalkraken.ErrDepthExceeded) — still handed back usable fills,
// and the instant a follow-up run should resume from.
//
// The venue walkers page forward in time and return what they have
// alongside the error, so an expired budget or an exceeded depth horizon
// is a stopping point, not a corruption: the fills already fetched are
// as good as any others. The high-water timestamp is the resume cursor —
// inserts are idempotent (ON CONFLICT), so re-running from it costs at
// most one duplicate page. Any other error is a real fault and must not
// be salvaged.
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

// partialWalkError is the non-zero exit for a salvaged run. The range
// asked for was NOT covered, so the command must never exit 0 however
// well the writes went — an operator scripting a chunked walk has to be
// able to tell a completed slice from a truncated one.
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

// refuseStoredOverlap refuses a window that already holds trades for
// (source, pair). An off-chain row's identity is its synthesised tx_hash
// plus ts. A candle has no venue trade id, so it never matches a live row;
// a kraken raw fill shares the streamer's tx_hash, but its REST ts is not
// proven to equal the streamed ts at the stored microsecond. Writing over
// live rows would double-count their volume with no VWAP, outlier or
// divergence guard to notice. Any stored row
// refuses, not only live ones: the tool cannot tell a row it wrote
// itself from one it did not, and -allow-overlap covers its own re-runs.
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
	// overlap is re-fetched ahead of each window so a fill on the boundary
	// survives whether the venue's `since` cursor is inclusive or not; the
	// duplicates are dropped by trade identity before the write.
	overlap time.Duration
	// pace spaces windows apart: the venue walker paces only between its
	// own pages, so back-to-back windows would otherwise burst requests.
	pace time.Duration
}

// krakenRawWindows bounds a -raw-trades run to one day of fills in memory.
// A pair's deep history is ~10^7 fills; held whole, it is killed by the
// memory cap before a row is written. pace mirrors kraken's tradesRateLimit.
var krakenRawWindows = windowPlan{size: 24 * time.Hour, overlap: time.Second, pace: 1100 * time.Millisecond}

type (
	windowFetch func(ctx context.Context, from, to time.Time) ([]canonical.Trade, error)
	windowSink  func(trades []canonical.Trade) error
)

// backfillKrakenRawTrades drives the kraken fills walk one window at a
// time, writing each window before fetching the next, so memory is bounded
// by a window and a failure costs at most the window it happened in.
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

// walkWindowed fetches [from, to) window by window and hands each window
// to sink before fetching the next. Every window before the one that
// stops the walk is fully written, so resuming at the stopping window's
// start skips nothing and re-fetches at most one window. The partial
// fills that window's walk returned are discarded: writing them would put
// rows inside the resume range, which refuseStoredOverlap then blocks
// unless the operator disables the overlap guard for the rest. A per-row
// write fault does not stop the walk (the rows are already logged and
// lost) but keeps the exit non-zero; an infra write fault stops it.
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

// dropSeen removes trades already handed to the sink by the previous
// window's overlap, and returns the identities the next window's overlap
// may repeat (those at or after tailFrom).
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

// windowStopError is the non-zero exit of a windowed walk that did not
// reach -to. It carries every cause (errors.Is sees each) and the resume
// point: the window's start, before which everything is written.
func windowStopError(start, end time.Time, written int, causes []error) error {
	return fmt.Errorf("backfill-external: walk stopped in window %s -> %s with %d trade(s) written — range incomplete, resume with -from %s: %w",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), written,
		start.UTC().Format(time.RFC3339Nano), errors.Join(causes...))
}

// tradeInserter is the storage seam insertBackfilledTrades depends on —
// split out so the insert-loop's failure policy (infra-abort vs
// data-fault-skip, and the non-zero exit on any drop) is unit-testable
// with a fake, without a live Postgres.
type tradeInserter interface {
	InsertTrade(ctx context.Context, t canonical.Trade) error
}

// insertBackfilledTrades is the DB-seamed core of the insert loop: an
// infra fault (DB unreachable/restarting/out of capacity) affects every
// remaining insert identically, so looping through the rest and counting
// each as "skipped" would misreport a total write outage as a pile of
// per-row data faults and still exit 0 — it aborts immediately instead,
// so the operator sees ONE clear error and can re-run once the DB
// recovers. A per-row data fault is skipped and logged, but ANY skip
// makes the command return a non-nil error: a dropped row has no
// dead-letter, so "some rows didn't make it" must never exit 0.
func insertBackfilledTrades(ctx context.Context, store tradeInserter, trades []canonical.Trade, progressEvery int, log io.Writer, t0 time.Time) error {
	inserted, skipped := 0, 0
	for i, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			if timescale.IsInfraError(err) {
				return fmt.Errorf("backfill-external: aborting at trade %d/%d (%s) after infra fault — %d inserted, %d skipped so far, %d not attempted: %w",
					i, len(trades), tr.TxHash, inserted, skipped, len(trades)-i, err)
			}
			skipped++
			_, _ = fmt.Fprintf(log, "insert trade %d (%s): %v\n", i, tr.TxHash, err)
			continue
		}
		inserted++
		if progressEvery > 0 && inserted%progressEvery == 0 {
			_, _ = fmt.Fprintf(log, "  ... %d inserted, %d skipped\n", inserted, skipped)
		}
	}
	_, _ = fmt.Fprintf(log, "backfill-external: done — %d inserted, %d skipped in %v\n",
		inserted, skipped, time.Since(t0).Round(time.Millisecond))
	return opsutil.RunOutcome{
		Verb: "backfill-external", Noun: "trade",
		Attempted: len(trades), Written: inserted, Failed: skipped,
	}.Err()
}

// buildBackfiller maps the -source flag to the venue's Backfiller
// implementation. Each venue's DefaultPairs is consulted to resolve
// the venue-native symbol into a canonical.Pair. Unknown sources or
// unconfigured pairs return a clear error rather than a generic
// "not in map".
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
	}
	return nil, canonical.Pair{}, fmt.Errorf("unknown -source %q (supported: binance, kraken, bitstamp, coinbase)", source)
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

// summariseDryRun prints a compact stats view for --dry-run mode.
// Shows first/last trade timestamps, trade count, and pair-level
// volume totals so the operator can sanity-check a range before
// committing a large insert.
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

// amountToFloat converts a canonical.Amount at the given decimal
// scale to a float64 for display. Precision-lossy; never use this
// path for anything that writes back to storage.
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
