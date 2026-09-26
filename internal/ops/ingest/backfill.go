package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// SorobanEventsPseudoSource is the backfill-only source name that
// populates the `soroban_events` raw-event landing zone (ADR-0029).
// It is NOT registered with internal/sources/external.Registry —
// soroban-events is a pseudo-source: the catch-all dispatcher hook
// sees every event regardless of per-source decoder routing, so
// the backfill swaps the per-source decoder chain for "no decoders,
// raw sink only" when this name is requested.
//
// Used as -source soroban-events on the ops command line. Not
// permitted alongside other sources in the same invocation (the
// pseudo-source isn't a peer of trades / oracles — it's the
// distinct "raw-event capture" mode of operation).
const SorobanEventsPseudoSource = "soroban-events"

// ─── stellarindex-ops backfill ──────────────────────────────────
//
// Replays a bounded ledger range through the same dispatcher +
// decoder + sink path the live indexer uses, producing trade rows
// into the trades hypertable. CAGGs (1m / 15m / 1h / 4h / 1d / 1w /
// 1mo per migration 0002) auto-materialise on the inserted rows.
//
// Differs from the indexer in three load-bearing ways:
//
//  1. Bounded range [-from, -to]. ledgerstream.Stream exits at -to;
//     no live tail. Backfill is a one-shot operation.
//  2. No cursor row written. The indexer's `ledgerstream` cursor
//     drives "resume from cursor+1" on restart. Backfill has its
//     own explicit -from; if it crashed and were to share that
//     cursor, the indexer would mis-resume from a historical
//     ledger on its next start.
//  3. BackfillSafe gate. internal/sources/external.Registry marks
//     every on-chain Soroban source `BackfillSafe=false` until its
//     decoder has been audited against every WASM version that ran
//     for the replay range (AGENTS.md "Soroban DeFi contracts
//     upgrade in place"). Backfill refuses to run an unsafe source.
//
// Trade-row idempotency is the storage layer's responsibility — the
// trades hypertable currently dedupes on (source, ledger, tx_hash,
// op_index, ts), so re-running over the same range is a no-op only
// when the replay reproduces the same timestamp too. Aggregator CAGGs
// recompute from the underlying rows so duplicate suppression at
// insert time is sufficient once that storage identity matches.

// backfillOpts holds the parsed + validated CLI inputs. Pulled out
// of the entry point so flag-parsing + validation are unit-testable
// without executing the pipeline.
type backfillOpts struct {
	cfgPath  string
	from     uint32
	to       uint32
	sources  []string // resolved: -source override or cfg.Ingestion.EnabledSources
	bucket   string   // resolved: -bucket override or cfg.Storage.S3BucketArchive
	dryRun   bool
	resume   bool // when true, look up the prior cursor and skip already-processed ledgers
	parallel int  // number of concurrent chunks to run; 1 = sequential (default)
	// refreshCAGGs controls the post-chunk continuous-aggregate
	// materialisation call. Defaults to true. Pre-2026-05-13
	// backfills did not refresh CAGGs and consequently lost their
	// data to the 90-day raw-trades retention before the policy
	// refresher's natural cadence picked the inserts up. Operators
	// should leave this on; the only legitimate reason to disable
	// is debugging a refresh failure where re-running the
	// underlying chunk is the desired recovery path.
	refreshCAGGs bool
	// heartbeat publishes liveness + progress for the whole run
	// (C6-020). Shared by every chunk goroutine — JobHeartbeat is
	// mutex-guarded and walkedTotal is atomic, so the per-chunk
	// callbacks can report into it concurrently. nil-safe: an inert
	// heartbeat (no node_exporter textfile dir) makes every call a
	// no-op, which is the laptop/CI case.
	heartbeat   *opsutil.JobHeartbeat
	walkedTotal *atomic.Uint64
	// heartbeatPath is the -heartbeat flag: an explicit node_exporter
	// textfile path, or "" for the auto-resolved default.
	heartbeatPath string
}

// chunkRange is one sub-range of a parallel backfill: [from, to]
// inclusive. Workers process distinct chunkRanges concurrently;
// each writes its own cursor row keyed on the chunk-specific
// (from, to) so resume-on-restart works per-chunk.
type chunkRange struct {
	from uint32
	to   uint32
}

// planBackfillChunks splits [from, to] into n contiguous,
// non-overlapping sub-ranges. The last chunk absorbs any rounding
// remainder so the union of the chunks covers [from, to] exactly.
//
// Caller invariants (enforced by parseBackfillFlags): n >= 1,
// to >= from. With n == 1, returns the original range as a single
// chunk (sequential mode, same shape as the pre-parallelism path).
func planBackfillChunks(from, to uint32, n int) []chunkRange {
	if n <= 1 {
		return []chunkRange{{from: from, to: to}}
	}
	total := uint64(to) - uint64(from) + 1
	size := total / uint64(n)
	if size == 0 {
		// More workers than ledgers; degrade to one chunk per ledger
		// up to the range size, rest unused.
		size = 1
	}
	out := make([]chunkRange, 0, n)
	cur := uint64(from)
	for i := 0; i < n && cur <= uint64(to); i++ {
		end := cur + size - 1
		if i == n-1 || end > uint64(to) {
			end = uint64(to)
		}
		out = append(out, chunkRange{from: uint32(cur), to: uint32(end)})
		cur = end + 1
	}
	return out
}

// backfillCursorSource is the value stamped on every backfill
// cursor row in the ingestion_cursors table. Distinct from the
// indexer's "ledgerstream" so a backfill crash + restart does NOT
// pollute the indexer's resume position.
const backfillCursorSource = "backfill"

// backfillCursorSub returns the sub-source key that distinguishes
// concurrent / overlapping backfill runs. We need the (from, to,
// sorted-sources) tuple in the key so two operators replaying
// different ranges or different source subsets don't share a cursor
// row and step on each other.
func backfillCursorSub(opts backfillOpts) string {
	sorted := make([]string, len(opts.sources))
	copy(sorted, opts.sources)
	sort.Strings(sorted)
	return opsutil.RangeCursorKey(opts.from, opts.to) + ":" + strings.Join(sorted, ",")
}

func backfill(args []string) error {
	opts, cfg, err := parseBackfillFlags(args)
	if err != nil {
		return err
	}

	if !opsutil.PrintWriteBanner(!opts.dryRun) {
		chunks := planBackfillChunks(opts.from, opts.to, opts.parallel)
		_, _ = fmt.Fprintf(os.Stderr,
			"backfill dry-run:\n  range:    [%d, %d] (%d ledgers)\n  sources:  %v\n  bucket:   %s\n  parallel: %d (chunks: %d)\n",
			opts.from, opts.to, opts.to-opts.from+1, opts.sources, opts.bucket, opts.parallel, len(chunks))
		for i, c := range chunks {
			_, _ = fmt.Fprintf(os.Stderr, "  chunk %d: [%d, %d] (%d ledgers)\n", i, c.from, c.to, c.to-c.from+1)
		}
		return nil
	}

	rootCtx, cancel := opsutil.SignalContext()
	defer cancel()

	logger := opsutil.MkBackfillLogger()

	store, err := timescale.Open(rootCtx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// A-CRIT-2 (audit-2026-07-24): the main on-chain backfill re-processes ledgers
	// that may overlap live ingest (gap-fill within the trades window / running
	// alongside live). It writes the same trade PKs, so ON CONFLICT UPDATEs the
	// existing rows. Without the USD-volume resolvers installed it computes
	// usd_volume=NULL and — at the default gen 0 — still wins the guard (0<=0),
	// overwriting live-ingested correct values with NULL. Mirror the indexer /
	// backfill_external / ch_rebuild wiring: a positive generation so a corrected
	// re-derive is authoritative, AND the resolvers so it writes real values (not
	// NULL). The reDeriveNullVolumeGuard now backstops any future omission.
	store.SetDeriveGeneration(time.Now().Unix())
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
	); err != nil {
		return fmt.Errorf("install usd-volume resolution: %w", err)
	}

	chunks := planBackfillChunks(opts.from, opts.to, opts.parallel)
	logger.Info("backfill starting",
		"from", opts.from,
		"to", opts.to,
		"sources", opts.sources,
		"bucket", opts.bucket,
		"parallel", opts.parallel,
		"chunks", len(chunks),
	)

	// C6-020: liveness + progress for the whole walk, so a wedged backfill
	// (stalled S3 read, storage write that never returns, OOM-killed chunk
	// goroutine) is distinguishable from a working one without tailing the
	// journal. Inert off-r1 — see opsutil.NewJobHeartbeat.
	opts.heartbeat = opsutil.NewJobHeartbeat("backfill", opts.heartbeatPath, nil)
	opts.walkedTotal = &atomic.Uint64{}
	if opts.heartbeat.Enabled() {
		logger.Info("backfill heartbeat", "textfile", opts.heartbeat.Path())
	}
	opts.heartbeat.Start()
	backfillOK := false
	defer func() { opts.heartbeat.Stop(backfillOK) }()

	// Sequential fast-path. Same shape as the pre-parallelism code,
	// minus the redundant goroutine + channel hop. Lets `-parallel 1`
	// (the default) keep its existing semantics: one cursor row, one
	// ledgerstream, one events channel.
	if len(chunks) == 1 {
		err := runBackfillChunk(rootCtx, logger, opts, cfg, store, chunks[0])
		backfillOK = err == nil
		return err
	}

	// Parallel path. Each chunk is independent: its own dispatcher,
	// its own events channel, its own PersistEvents goroutine, its
	// own cursor row keyed on the chunk-specific (from, to). The
	// shared store's connection pool fans across chunks (postgres
	// max_connections is the only ceiling — typical 100 vs ~3 conns
	// per chunk = 30+ chunks supported on stock config).
	if err := runChunksGuarded(logger, "backfill-chunk", chunks, func(chunkLogger *slog.Logger, c chunkRange) error {
		return runBackfillChunk(rootCtx, chunkLogger, opts, cfg, store, c)
	}); err != nil {
		return err
	}

	logger.Info("backfill complete",
		"from", opts.from,
		"to", opts.to,
		"ledgers", opts.to-opts.from+1,
		"parallel", opts.parallel,
	)
	backfillOK = true
	return nil
}

// runChunkGuarded runs one chunk's work (run) with panic recovery so a
// panic inside a single parallel chunk cannot take the whole backfill
// process down (NS27): an unrecovered panic in any goroutine terminates
// the entire Go process, not just the goroutine that panicked. The
// panicking chunk is reported via worker.Report (same accounting as every
// other detached worker) and surfaced through errCh so the backfill still
// returns a non-nil error instead of silently completing short.
func runChunkGuarded(logger *slog.Logger, name string, i int, c chunkRange, errCh chan<- error, wg *sync.WaitGroup, run func() error) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			worker.Report(logger, name, r)
			errCh <- fmt.Errorf("chunk %d [%d, %d]: panic: %v", i, c.from, c.to, r)
		}
	}()
	if err := run(); err != nil {
		errCh <- fmt.Errorf("chunk %d [%d, %d]: %w", i, c.from, c.to, err)
	}
}

// runChunksGuarded runs every chunk concurrently, each under
// runChunkGuarded, and joins their errors. It is the one fan-out for
// backfill and resume-stalled, so neither can spawn an unguarded chunk.
func runChunksGuarded(logger *slog.Logger, name string, chunks []chunkRange, run func(*slog.Logger, chunkRange) error) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(chunks))
	for i, c := range chunks {
		wg.Add(1)
		chunkLogger := logger.With("chunk", i, "chunk_from", c.from, "chunk_to", c.to)
		go runChunkGuarded(chunkLogger, fmt.Sprintf("%s-%d", name, i), i, c, errCh, &wg, func() error {
			return run(chunkLogger, c)
		})
	}
	wg.Wait()
	close(errCh)

	var combined []error
	for e := range errCh {
		combined = append(combined, e)
	}
	return errors.Join(combined...)
}

// buildChunkDispatcher constructs the per-chunk dispatcher and,
// when the soroban-events pseudo-source is in play, wires the
// RawEventSink (ADR-0029). Returns the dispatcher + the sink (nil
// when not pseudo so the caller can branch on it for teardown).
//
// Factored out of runBackfillChunk to keep that function within
// the funlen limit; the dispatcher-construction surface had grown
// to ~50 lines that read linearly but blew the limit when combined
// with the chunk lifecycle.
func buildChunkDispatcher(
	ctx context.Context,
	logger *slog.Logger,
	opts backfillOpts,
	cfg config.Config,
	store *timescale.Store,
	pseudo bool,
) (*dispatcher.Dispatcher, *sorobanevents.AsyncSink, error) {
	// Every entry point (backfill, resume-stalled) reaches the decoders
	// through here, so the source policy is enforced here, not only at
	// flag parse.
	if err := checkBackfillSourcePolicy(opts.sources, cfg, opts.from, opts.to); err != nil {
		return nil, nil, err
	}
	realSources := filterOutSorobanEventsPseudo(opts.sources)

	var soroswapOpts []soroswap.DecoderOption
	if !pseudo && len(realSources) > 0 {
		// Soroswap pair registry — load and arm live-upsert. Each
		// chunk runs its own dispatcher so each calls this
		// independently; the store is shared so chunks see each
		// other's upserted pairs on next load. See
		// internal/pipeline/soroswap_registry.go.
		var err error
		soroswapOpts, err = pipeline.SoroswapPersistenceOptions(ctx, store, logger, ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("soroswap registry: %w", err)
		}
	}
	// gated=nil is safe only because checkBackfillSourcePolicy above
	// refused every projector-owned source: an empty identity gate
	// here would otherwise make blend write zero rows and exit 0, and
	// SinkModeAll would make the rest a second writer (invariant [7]).
	disp, err := pipeline.BuildDispatcher(realSources, cfg.Oracle, nil, soroswapOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("build dispatcher: %w", err)
	}
	if !pseudo {
		return disp, nil, nil
	}

	// soroban-events pseudo-source: wire the RawEventSink (ADR-0029).
	// Sink lifecycle is local to this chunk so a parallel-N backfill
	// gets N independent sinks each with their own batch buffer.
	// Stop()'d at chunk end to flush any partial batch.
	rawSink := sorobanevents.NewAsyncSink(store, sorobanevents.AsyncSinkOptions{
		IsPermanentFault: timescale.IsPermanentDataError,
		BufferSize:       4096,
		BatchSize:        1000,
		FlushInterval:    time.Second,
		WriteTimeout:     10 * time.Second,
		Logger:           logger.With("component", "soroban-events-sink"),
	})
	rawSink.Start()
	disp.SetRawEventSink(rawSink)
	return disp, rawSink, nil
}

// runBackfillChunk processes a single chunkRange end-to-end:
// dispatcher build → events channel → PersistEvents goroutine →
// ledgerstream over [chunk.from, chunk.to] → cursor upsert per
// ledger. Cursor sub_source is chunk-specific (uses chunk.from /
// chunk.to, NOT the overall opts.from / opts.to) so concurrent
// chunks never share a cursor row.
//
//nolint:gocognit,funlen // chunk lifecycle is linear setup → stream → teardown; splitting reduces readability of dependency-construction order.
func runBackfillChunk(ctx context.Context, logger *slog.Logger, opts backfillOpts, cfg config.Config, store *timescale.Store, chunk chunkRange) error { //nolint:gocyclo // one cohesive backfill orchestration: chunk stream -> async drain -> post-drain durable cursor (C2-14); splitting scatters the watermark narrative
	chunkOpts := opts
	chunkOpts.from = chunk.from
	chunkOpts.to = chunk.to
	cursorSub := backfillCursorSub(chunkOpts)

	startFrom := chunk.from
	if opts.resume {
		c, err := store.GetCursor(ctx, backfillCursorSource, cursorSub)
		switch {
		case errors.Is(err, timescale.ErrNotFound):
			logger.Info("no prior cursor — starting from chunk-from", "from", chunk.from)
		case err != nil:
			return fmt.Errorf("load resume cursor: %w", err)
		case c.LastLedger >= chunk.to:
			logger.Info("prior cursor at or past chunk-to — already complete",
				"cursor", c.LastLedger, "to", chunk.to)
			return nil
		default:
			startFrom = c.LastLedger + 1
			logger.Info("resuming from prior cursor",
				"cursor", c.LastLedger,
				"start_from", startFrom,
				"to", chunk.to,
				"remaining", chunk.to-startFrom+1)
		}
	}

	pseudo := hasSorobanEventsPseudo(opts.sources)
	disp, rawSink, err := buildChunkDispatcher(ctx, logger, opts, cfg, store, pseudo)
	if err != nil {
		return err
	}

	events := make(chan consumer.Event, 256)
	sinkDone := make(chan struct{})
	var persistLoss pipeline.ShutdownLoss // read only after <-sinkDone
	go func() {
		defer close(sinkDone)
		// Backfill always uses SinkModeAll — the projector only runs
		// in the indexer, not in `stellarindex-ops backfill`, so this
		// subcommand keeps writing every event class itself. See
		// ADR-0032 § "Out of scope for projector".
		persistLoss = pipeline.PersistEvents(ctx, logger, store, events, pipeline.SinkModeAll)
	}()

	// Ctx-cancel safety net for the raw-event sink (ADR-0029).
	// PushEvent applies back-pressure (blocks when the buffer is
	// full) — required for cursor coherence — but the dispatcher's
	// hot path has no ctx awareness, so a blocked PushEvent would
	// pin the Stream callback even after rootCtx is cancelled. If
	// ctx fires while the producer is mid-dispatch we early-Stop
	// the sink so blocked PushEvent calls unblock via the stopping
	// channel, the dispatcher returns, and ledgerstream.Stream can
	// honour cancellation.
	if rawSink != nil {
		go func() {
			select {
			case <-ctx.Done():
				rawSink.Stop()
			case <-sinkDone:
			}
		}()
	}

	streamCfg := pipeline.LedgerstreamConfig(cfg, opts.bucket)
	// Count actual LCM callbacks so the chunk-complete log line can
	// distinguish "this chunk walked N ledgers" from "the chunk
	// covered an N-ledger range." F-0159 (2026-05-26): a backfill
	// run against a bucket with no files in the target range logged
	// `chunk complete ... ledgers=5331` and exited in 200ms — the
	// `ledgers=` value was the chunk's [from,to] range size, not the
	// count of ledgers actually walked. Operators read the log as a
	// false-positive "backfill complete" and moved on without the
	// gap being filled.
	var (
		walked uint64
		// C2-14: the last ledger whose events were FULLY enqueued onto the
		// sink channel (ProcessLedger returned nil). This is NOT yet a
		// "durably persisted" marker — the async PersistEvents drain (+
		// batched trades + the soroban rawSink) commit the rows later. The
		// cursor is advanced to this value only AFTER the drain below
		// confirms every enqueued row landed.
		lastFullyEnqueued uint32
	)
	streamErr := ledgerstream.Stream(ctx, streamCfg, startFrom, chunk.to,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			walked++
			if err := pipeline.ProcessLedger(ctx, disp, events, logger, lcm, cfg.Stellar.Passphrase()); err != nil {
				return err
			}
			// C6-020: report into the run-wide heartbeat AFTER the ledger
			// was actually processed — a counter bumped before the work
			// would keep advancing while ProcessLedger wedges, which is
			// the exact hang the progress alert exists to catch.
			if opts.walkedTotal != nil {
				opts.heartbeat.Progress(opts.walkedTotal.Add(1), uint64(lcm.LedgerSequence()))
			}
			// C2-14 (durability): DO NOT advance the cursor here. Advancing
			// at enqueue time moved the resume watermark PAST rows that were
			// still buffered in the sink channel / trade batch / rawSink — a
			// crash between this enqueue and the async commit lost those rows
			// with the cursor already beyond them (same class as C2-1). Record
			// only the last fully-enqueued ledger; the durable advance happens
			// after <-sinkDone + rawSink.Stop() below prove the rows committed.
			lastFullyEnqueued = lcm.LedgerSequence()
			return nil
		},
	)

	close(events)
	<-sinkDone

	rawMin, rawUnlanded := drainRawSink(logger, rawSink)
	// A dropped or abandoned row means lastFullyEnqueued overstates what
	// is durable; every checkpoint below uses the capped value.
	lastFullyEnqueued, lossErr := capCheckpointAtLoss(lastFullyEnqueued, startFrom, persistLoss, rawMin, rawUnlanded)

	// C2-14 (durability): the resume cursor advances ONLY after this
	// point, and (DAT-09 / REL-08) ONLY after a successful — or
	// explicitly-skipped — CAGG refresh below. The sink goroutine has
	// fully drained (<-sinkDone: every enqueued event either committed
	// or block-and-retried per ADR-0041) and the soroban rawSink has
	// flushed its final batch (Stop above), and lastFullyEnqueued is
	// capped below anything either dropped or abandoned, so every row
	// for ledgers [startFrom, lastFullyEnqueued] is durable — but a chunk
	// is not "complete" for resume purposes until its CAGGs are
	// materialised too: the resume gate below (`c.LastLedger >=
	// chunk.to` → short-circuit, skip re-walk AND re-refresh) would
	// otherwise trust a chunk whose trades are durably inserted but
	// still invisible to the served price views, and a crash between
	// the old early cursor-advance and materialisation left them
	// un-refreshed forever (the May 2026 ~80M-trade loss this refresh
	// exists to prevent, reintroduced one layer up).
	streamFailed := streamErr != nil && !errors.Is(streamErr, context.Canceled)
	if streamFailed || (lossErr != nil && ctx.Err() == nil) {
		// The stream itself aborted, or a sink left rows unlanded (the
		// cursor is already capped below them). Still checkpoint whatever fully
		// drained before the abort — those rows are durably committed
		// above — so a resume continues from the failure point instead
		// of restarting the whole chunk. This does NOT wait for a CAGG
		// refresh: the chunk failed outright (the returned error fails
		// the run visibly), and a subsequent successful resume performs
		// its own CAGG refresh before ITS checkpoint.
		checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment (F-1318)
		if !streamFailed {
			return lossErr
		}
		return errors.Join(fmt.Errorf("stream: %w", streamErr), lossErr)
	}
	if ierr := backfillChunkInterrupted(ctx.Err(), chunk, lastFullyEnqueued); ierr != nil {
		// Graceful shutdown (SIGINT/SIGTERM): checkpoint what fully
		// drained and stop HERE — `ctx` is dead, so the CAGG refresh
		// below would fail spuriously. The chunk is NOT complete, so it
		// fails the run; see backfillChunkInterrupted.
		checkpointBackfillChunk(logger, store, cursorSub, interruptedCheckpoint(chunk, lastFullyEnqueued), startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment (F-1318)
		if lossErr != nil {
			ierr = errors.Join(ierr, lossErr)
		}
		return ierr
	}

	// F-0159: report BOTH the chunk's range size and the count of
	// ledgers actually walked. The old `ledgers=` field was the
	// range size; if it didn't match `ledgers_walked` the bucket
	// was missing files. Fail loudly on a complete miss across a
	// non-zero range — that's almost always a bucket-mistargeting
	// bug (wrong --bucket flag, wrong endpoint, wrong region) and
	// the silent success was misleading enough to ship a
	// false-positive "gap filled" signal in production.
	//
	// RLT-266: a PARTIAL walk fails too — see backfillChunkCoverage.
	// The check sits BEFORE the CAGG refresh and the completing
	// checkpoint so a short chunk is never refreshed-and-recorded as
	// done; the rows that did drain are checkpointed exactly as the
	// stream-abort branch above does, so a -resume continues from the
	// truncation point (and fails again, loudly, while the objects are
	// still absent).
	if cerr := backfillChunkCoverage(chunk, startFrom, walked, opts.bucket); cerr != nil {
		logger.Error("chunk INCOMPLETE",
			"from", chunk.from,
			"to", chunk.to,
			"start_from", startFrom,
			"ledgers_walked", walked,
			"last_fully_enqueued", lastFullyEnqueued,
		)
		checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment (F-1318)
		return cerr
	}
	logger.Info("chunk complete",
		"from", chunk.from,
		"to", chunk.to,
		"chunk_size_ledgers", chunk.to-chunk.from+1,
		"ledgers_walked", walked,
	)

	// CAGG refresh path: skipped for the soroban-events pseudo-source
	// (the soroban_events hypertable has no CAGGs built on top of it
	// — future per-source decoders read from it directly via SELECT).
	// Nothing to materialise, so the checkpoint below is unconditional.
	switch {
	case pseudo:
		logger.Info("skipping CAGG refresh — soroban-events has no CAGGs")
	case opts.refreshCAGGs:
		// Force-materialise the long-lived CAGGs over the chunk's
		// timestamp range. Historically this was data-loss protection:
		// a 90-day retention policy on raw `trades` dropped historical
		// inserts before the refresher's natural cadence materialised
		// them (the May 2026 SDEX backfill — cursors completed, trades
		// inserted, retention dropped them within 24h, ~80M trades of
		// work lost). Migration 0031 REMOVED that policy (and the
		// 30-day one on prices_1m/15m); per ADR-0034 raw trades are
		// kept forever. What is left is a correctness-of-the-served-
		// view concern, not a loss one: without this refresh the
		// backfilled range's CAGG buckets stay unmaterialised — the
		// rows are in `trades` but absent from every OHLC/VWAP read —
		// until the next policy run or a manual refresh covers them.
		//
		// Every aggregate rooted on a table the chunk wrote is refreshed
		// ([timescale.TradesCAGGs], [timescale.OracleCAGGs]): a view left
		// out keeps a permanent hole in every backfilled range.
		//
		// DAT-09 / REL-08: a refresh failure here is FATAL to the
		// chunk — the function returns before the checkpoint below, so
		// the durable cursor does NOT advance past an unmaterialised
		// chunk. A resume re-walks and re-attempts the refresh.
		if err := refreshCAGGsForChunk(ctx, logger, store, chunk); err != nil {
			return fmt.Errorf("post-chunk CAGG refresh: %w", err)
		}
	default:
		logger.Warn("skipping CAGG refresh (-refresh-caggs=false)",
			"impact", "the range's buckets in every trades aggregate (prices_*, twap_*, dex_volume_by_pair_1d, source_volume_1h, pools_per_source_1h) and every oracle_prices_* rung stay unmaterialised until a manual refresh_continuous_aggregate covers them — the CAGG policies only roll forward, so a historical range is never picked up on their own cadence. The trades and oracle rows are durable, but every OHLC/VWAP/TWAP, volume and oracle-history read over the range is short until then",
		)
	}

	checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment (F-1318)
	return nil
}

// backfillChunkCoverage turns a chunk walk that did not cover its range
// into a hard error, naming the bucket it read (RLT-266).
//
// `backfill` was the third copy of the "vacuous success on a tolerated
// trailing miss" class and the only one that still failed open: it
// errored on walked == 0 alone (F-0159), while chops.backfillCoverage and
// censusCoverage both fail a PARTIAL walk. pipeline.LedgerstreamConfig
// opts every walk into TolerateTrailingMissing, and ledgerstream measures
// that tolerance window against the walk's own `to` — the CHUNK's top,
// not the network tip — so for any chunk (or any request under 65,536
// ledgers) "trailing edge" degrades to "anywhere in the range": a missing
// object ends the walk WITHOUT an error. The SDK also drops its prefetch
// buffer on the miss, so the walk stops up to a buffer short of the hole.
// The chunk then logged "chunk complete", refreshed the CAGGs over what
// it got and exited 0 — a trade hole whose only evidence was a success.
//
// The bar is the command's contract: "[from,to] has been walked". The
// default bucket is the archive, an hourly MIRROR of live, so a `-to`
// near the tip legitimately comes up short — and that is an incomplete
// backfill too, not a success. Failing closed costs a `-resume` re-run
// (writes are idempotent; the drained prefix is checkpointed); failing
// open costs a hole nobody looks for.
//
// `startFrom` is the ledger this run actually started at (post-resume),
// not chunk.from: ledgers banked by an earlier run are that run's
// business, and re-charging them here would fail every resumed chunk —
// the same rule censusCoverage states.
func backfillChunkCoverage(chunk chunkRange, startFrom uint32, walked uint64, bucket string) error {
	if startFrom > chunk.to {
		return nil // nothing was requested of this run
	}
	want := uint64(chunk.to) - uint64(startFrom) + 1
	switch {
	case walked == 0:
		return fmt.Errorf(
			"backfill walked 0 of %d ledgers in range [%d,%d] from bucket %q — "+
				"bucket likely has no files in this range; check --bucket and the "+
				"galexie-archive/-live mirror for the target range",
			want, startFrom, chunk.to, bucket,
		)
	case walked < want:
		return fmt.Errorf(
			"backfill walked only %d of %d ledgers in range [%d,%d] from bucket %q — %d ledgers were NOT walked: "+
				"an object is missing from the bucket and the trailing-missing tolerance ended the walk early (see the "+
				"ledgerstream WARN above for the missing sequence; the archive bucket mirrors live hourly, so a -to near "+
				"the tip comes up short until the mirror catches up). The range is NOT complete: no CAGG refresh was run "+
				"and the chunk is not recorded as done; the walked prefix is checkpointed, so re-run with -resume once "+
				"the objects exist",
			walked, want, startFrom, chunk.to, bucket, want-walked,
		)
	}
	return nil
}

// drainRawSink stops the soroban-events sink (nil when not wired),
// letting it flush its residual buffer under its own drain grace, and
// reports the lowest ledger it dropped or abandoned. A drop is not only
// a hard-kill artefact: a graceful SIGINT while the sink applies
// back-pressure drops the rest of the in-flight ledger's rows.
func drainRawSink(logger *slog.Logger, rawSink *sorobanevents.AsyncSink) (uint32, bool) {
	if rawSink == nil {
		return 0, false
	}
	rawSink.Stop()
	logger.Info("soroban-events sink drained",
		"written", rawSink.WrittenCount(),
		"dropped", rawSink.DroppedCount(),
		"skipped", rawSink.SkippedCount(),
		"lost", rawSink.LostCount(),
	)
	if dropped := rawSink.DroppedCount(); dropped > 0 {
		logger.Warn("soroban-events: rows dropped at shutdown",
			"dropped", dropped,
			"impact", "the dropped rows are NOT in soroban_events; the resume cursor is held below them so a resume re-walks their ledger")
	}
	if lost := rawSink.LostCount(); lost > 0 {
		logger.Error("soroban-events: rows permanently lost — re-derive the logged ledger ranges from the CH lake (ADR-0034)",
			"lost", lost,
			"impact", "rows abandoned at shutdown hold the resume cursor below them; permanent data faults do not")
	}
	return rawSink.LowestUnlandedLedger()
}

// capCheckpointAtLoss lowers the resume checkpoint below the first row a
// sink failed to land, and returns an error naming the loss so the chunk
// fails. An abandoned row with no ledger (a non-trade served-tier event)
// gives no safe cap, so the checkpoint falls below startFrom: nothing new
// is recorded and a resume re-walks the chunk's remainder (idempotent).
func capCheckpointAtLoss(lastFullyEnqueued, startFrom uint32, loss pipeline.ShutdownLoss, rawMin uint32, rawUnlanded bool) (uint32, error) {
	if loss.Rows == 0 && !rawUnlanded {
		return lastFullyEnqueued, nil
	}
	capAt := lastFullyEnqueued
	lower := func(l uint32) {
		if l > 0 && l-1 < capAt {
			capAt = l - 1
		}
	}
	if loss.LedgerUnknown {
		lower(startFrom)
	}
	lower(loss.MinLedger)
	if rawUnlanded {
		lower(rawMin)
	}
	return capAt, fmt.Errorf(
		"sinks did not land every enqueued row (served-tier rows abandoned: %d, lowest ledger %d, ledger-less: %t; "+
			"soroban-events lowest unlanded ledger: %d); resume checkpoint held at %d",
		loss.Rows, loss.MinLedger, loss.LedgerUnknown, rawMin, capAt)
}

// backfillChunkInterrupted fails a chunk whose walk was stopped by
// SIGINT/SIGTERM (ctxErr != nil). An interrupted chunk has not had its
// CAGGs refreshed and usually has not walked its range, so returning nil
// let `backfill` log "backfill complete" and exit 0 on a partial range.
func backfillChunkInterrupted(ctxErr error, chunk chunkRange, lastFullyEnqueued uint32) error {
	if ctxErr == nil {
		return nil
	}
	return fmt.Errorf(
		"backfill chunk [%d,%d] interrupted before completion (last fully drained ledger %d; 0 = none): %w — "+
			"the range is NOT complete and no CAGG refresh ran; re-run with -resume",
		chunk.from, chunk.to, lastFullyEnqueued, ctxErr,
	)
}

// interruptedCheckpoint caps an interrupted chunk's checkpoint below
// chunk.to: a cursor at chunk.to makes -resume short-circuit the chunk as
// complete, skipping the CAGG refresh the interruption prevented.
func interruptedCheckpoint(chunk chunkRange, lastFullyEnqueued uint32) uint32 {
	if lastFullyEnqueued >= chunk.to && chunk.to > 0 {
		return chunk.to - 1
	}
	return lastFullyEnqueued
}

// checkpointBackfillChunk durably advances the backfill cursor to
// lastFullyEnqueued. No-op when nothing was fully enqueued (or
// enqueued below startFrom — nothing new to record). Uses a FRESH
// bounded context: on a graceful SIGINT the parent ctx is already
// canceled by the time callers reach this point, and passing it would
// make the checkpoint fail instantly — silently discarding the resume
// watermark for the ledgers just drained (F-1318 pattern).
func checkpointBackfillChunk(logger *slog.Logger, store *timescale.Store, cursorSub string, lastFullyEnqueued, startFrom uint32) {
	if lastFullyEnqueued == 0 || lastFullyEnqueued < startFrom {
		return
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := store.UpsertCursor(cctx, backfillCursorSource, cursorSub, lastFullyEnqueued); err != nil { //nolint:contextcheck // deliberate fresh ctx: the parent is already canceled on a graceful SIGINT (F-1318) — using it would silently drop the resume watermark; see the doc comment above
		logger.Warn("backfill cursor upsert (post-drain)",
			"ledger", lastFullyEnqueued,
			"err", err)
	}
}

// caggRefresher is the storage seam refreshCAGGsForChunk depends on —
// split out so its failure-aggregation logic (DAT-09 / REL-08: a
// per-view failure must make the WHOLE refresh fail, not just log and
// continue) is unit-testable with a fake, without a live Postgres.
// *timescale.Store satisfies this structurally.
type caggRefresher interface {
	LedgerRangeToTimeRange(ctx context.Context, from, to uint32) (time.Time, time.Time, error)
	LedgerRangeToOracleTimeRange(ctx context.Context, from, to uint32) (time.Time, time.Time, error)
	Prices1mRetentionArmed(ctx context.Context) (bool, error)
	timescale.CAGGStepRefresher
}

// caggRefreshMu serialises the refresh loop across every `-parallel`
// worker in this process.
//
// `-parallel N` is N goroutines in ONE process (see the WaitGroup fan-
// out in runBackfill), each walking the same refresh plan
// ([chunkCAGGRefreshPlan]) at the end of its own chunk. TimescaleDB already serialises two
// refreshes of the SAME continuous aggregate — but it does it by
// rejecting the loser with 55P03 immediately, not by making it wait.
// [timescale.Store.RefreshContinuousAggregate] absorbs that with a
// bounded retry, which held while the contended view was prices_1mo (a
// handful of calendar buckets). It does not hold for prices_1m, now
// first in the list and by far the longest rung: its cost is the
// chunk's trade count, hundreds of thousands of rows for a sub-chunk
// of the documented `-parallel 4` weekly loop. A worker that loses
// that race retries for a fixed budget and then fails — and per
// DAT-09 / REL-08 a refresh failure is FATAL to the chunk, so the
// cursor does not checkpoint and the loop halts on a collision that
// is not a fault at all.
//
// The lock is BROADER than the race it removes, and that is a real
// cost rather than a free one. Timescale's 55P03 is per continuous
// aggregate: two workers refreshing DIFFERENT views never collided and
// would have run concurrently. Holding one mutex across the whole loop
// serialises those too, so W workers over V views take W×V×t where a
// per-view lock would pipeline to (W+V−1)×t. What stays parallel is
// the decode + insert phase, which is where the wall-clock of a
// backfill actually goes and which runs outside this lock.
//
// It is taken anyway because a lost race here is not a slow chunk but
// a FAILED one — DAT-09 / REL-08 makes a refresh error fatal, the
// cursor does not checkpoint, and the operator re-walks the chunk
// under `-resume`. Paying refresh throughput to remove that is the
// right trade at V=19 views; a per-view lock is the shape to reach for
// if the refresh tail ever dominates a run. It also leaves the retry
// budget for genuine contention from another process (the policy
// refresher, or an operator's manual re-materialisation).
var caggRefreshMu sync.Mutex

// refreshCAGGsForChunk refreshes every aggregate rooted on a table the
// chunk wrote — [timescale.TradesCAGGs] over the chunk's trades time
// span, [timescale.OracleCAGGs] over its oracle_updates span — in
// [timescale.PlanCAGGRefresh] order, so twap_* re-materialise only after
// prices_1m has been forced under them. Idempotent.
//
// Every independent view is still attempted after one fails — a single
// wedged view must not leave the rest un-materialised — but (DAT-09 /
// REL-08) any failure makes the function return a non-nil error, so the
// caller does NOT advance the durable cursor past the chunk. A view built
// on prices_1m is skipped when prices_1m's own refresh failed:
// recomputing it from stale minute rows would overwrite good history.
func refreshCAGGsForChunk(ctx context.Context, logger *slog.Logger, store caggRefresher, chunk chunkRange) error {
	plan, err := chunkCAGGRefreshPlan(ctx, logger, store, chunk)
	if err != nil || len(plan) == 0 {
		return err
	}
	// One worker at a time through the whole plan — see caggRefreshMu.
	// Held across the loop rather than per view: releasing between
	// rungs would just hand the next worker a view this one is about
	// to ask for, which is the collision the lock exists to remove.
	caggRefreshMu.Lock()
	defer caggRefreshMu.Unlock()

	armed, err := store.Prices1mRetentionArmed(ctx)
	if err != nil {
		return fmt.Errorf("read prices_1m retention state: %w", err)
	}
	var failed []string
	for _, st := range plan {
		if slices.Contains(timescale.CAGGsOnPrices1m, st.View) && slices.Contains(failed, "prices_1m") {
			logger.Error("CAGG refresh skipped — prices_1m, which it is built on, failed", "view", st.View)
			failed = append(failed, st.View)
			continue
		}
		if err := timescale.RunCAGGRefreshStep(ctx, store, st, armed); err != nil {
			logCAGGRefreshFailure(logger, st.View, err)
			failed = append(failed, st.View)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("CAGG refresh failed for %d/%d view(s): %s — trade and oracle rows are still in their hypertables (not lost); re-run to retry",
			len(failed), len(plan), strings.Join(failed, ", "))
	}
	return nil
}

// caggRoot is a hypertable backfill writes, how to find the time span a
// ledger range wrote to it, and the aggregates rooted on it.
type caggRoot struct {
	table string
	span  func(ctx context.Context, from, to uint32) (time.Time, time.Time, error)
	views []timescale.CAGGSpec
}

// chunkCAGGRefreshPlan builds the chunk's refresh plan, one root at a
// time. A root the chunk wrote no rows to contributes nothing.
func chunkCAGGRefreshPlan(ctx context.Context, logger *slog.Logger, store caggRefresher, chunk chunkRange) ([]timescale.CAGGRefreshStep, error) {
	roots := []caggRoot{
		{table: "trades", span: store.LedgerRangeToTimeRange, views: timescale.TradesCAGGs},
		{table: "oracle_updates", span: store.LedgerRangeToOracleTimeRange, views: timescale.OracleCAGGs},
	}
	var plan []timescale.CAGGRefreshStep
	for _, r := range roots {
		tsFrom, tsTo, err := r.span(ctx, chunk.from, chunk.to)
		if errors.Is(err, timescale.ErrNotFound) {
			logger.Info("no rows in chunk — skipping the CAGGs rooted on it",
				"table", r.table, "from", chunk.from, "to", chunk.to)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("derive %s ts range: %w", r.table, err)
		}
		logger.Info("refreshing CAGGs for chunk",
			"table", r.table, "views", len(r.views),
			"from", chunk.from, "to", chunk.to,
			"ts_from", tsFrom.UTC().Format(time.RFC3339),
			"ts_to", tsTo.UTC().Format(time.RFC3339),
		)
		// Pad to each view's minimum so the procedure does not reject
		// with "refresh window too small"; padded buckets are cheap.
		plan = append(plan, timescale.PlanCAGGRefresh(r.views, func(c timescale.CAGGSpec) (time.Time, time.Time) {
			return timescale.PadRefreshWindow(tsFrom, tsTo, c.MinWindow)
		})...)
	}
	return plan, nil
}

// logCAGGRefreshFailure logs one failed view. W8-19: a per-CALL bound
// firing names the view, the window and the bound, because that case
// used to hold every `-parallel` worker behind caggRefreshMu until
// SIGINT. It is still fatal to the chunk (DAT-09 / REL-08).
func logCAGGRefreshFailure(logger *slog.Logger, view string, err error) {
	var tErr *timescale.CAGGRefreshTimeoutError
	if errors.As(err, &tErr) {
		logger.Error("CAGG refresh timed out — bound fired; chunk fails, run continues",
			"view", tErr.View,
			"ts_from", tErr.From.UTC().Format(time.RFC3339),
			"ts_to", tErr.To.UTC().Format(time.RFC3339),
			"timeout", tErr.Timeout.String(),
			"err", err)
		return
	}
	logger.Error("CAGG refresh failed", "view", view, "err", err)
}

// validateBackfillRangeFlags checks the flags that need no config load:
// the config path is present, the range is non-empty and does not default
// to genesis, and -parallel is sane. Split out of parseBackfillFlags purely
// so that function stays under the funlen ceiling; the checks and their
// messages are unchanged.
func validateBackfillRangeFlags(cfgPath string, from, to uint, parallel int) error {
	if cfgPath == "" {
		return errors.New("-config required")
	}
	if from == 0 {
		return errors.New("-from must be > 0 (refuse to default to genesis)")
	}
	if to <= from {
		return fmt.Errorf("-to (%d) must be > -from (%d)", to, from)
	}
	if parallel < 1 {
		return fmt.Errorf("-parallel (%d) must be >= 1", parallel)
	}
	return nil
}

// parseBackfillFlags parses CLI args, loads config, and validates the
// result. Returns the resolved opts + the loaded config so the entry
// point can wire them up. Returns a non-nil error on any validation
// failure, including the BackfillSafe gate.
//
// Split out from backfill() so unit tests can drive validation
// without spinning up postgres + galexie.
func parseBackfillFlags(args []string) (backfillOpts, config.Config, error) {
	var opts backfillOpts
	var cfg config.Config

	fs, gate := opsutil.NewMutatingFlagSet("backfill")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "starting ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "ending ledger sequence (inclusive, required)")
	sourceCSV := fs.String("source", "",
		"comma-separated source names; default = cfg.Ingestion.EnabledSources")
	bucketOverride := fs.String("bucket", "",
		"galexie bucket override; default = cfg.Storage.S3BucketArchive")
	resume := fs.Bool("resume", false,
		"continue from a prior backfill cursor (keyed on -from/-to/-source). "+
			"On a fresh range with no prior cursor, behaves the same as without "+
			"-resume. Idempotent: re-runs over already-processed ledgers are a "+
			"no-op via the trades hypertable's unique index.")
	refreshCAGGs := fs.Bool("refresh-caggs", true,
		"force-refresh the seven price continuous aggregates "+
			"(prices_1m / 15m / 1h / 4h / 1d / 1w / 1mo) over each "+
			"chunk's timestamp range immediately after the trade-"+
			"insert loop completes. Required for historical backfills "+
			"— the CAGG policies only roll forward, so buckets older "+
			"than their refresh window are never materialised on "+
			"their own cadence and every OHLC/VWAP read over the "+
			"range serves nothing. Disable only when debugging a "+
			"specific refresh failure.")
	heartbeatPath := fs.String("heartbeat", "",
		"node_exporter textfile path for the liveness/progress gauges (C6-020). "+
			"Empty = "+opsutil.DefaultTextfileDir+"/ops_job_backfill.prom when that "+
			"directory exists (r1), otherwise no heartbeat at all.")
	parallel := fs.Int("parallel", 1,
		"number of concurrent chunks (default 1 = sequential). The range is "+
			"split into N contiguous, non-overlapping sub-ranges; each chunk "+
			"runs its own dispatcher + ledgerstream + sink with a chunk-specific "+
			"cursor row, so -resume picks up per-chunk on restart. Throughput "+
			"scales linearly with cores until postgres max_connections or the "+
			"galexie bucket's S3 list throughput becomes the bottleneck "+
			"(typical safe range: 4-16 on a 16-core box).")
	if err := fs.Parse(args); err != nil {
		return opts, cfg, err
	}

	if err := validateBackfillRangeFlags(*cfgPath, *from, *to, *parallel); err != nil {
		return opts, cfg, err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return opts, cfg, fmt.Errorf("backfill: %w", err)
	}

	loaded, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return opts, cfg, fmt.Errorf("load config: %w", err)
	}
	cfg = loaded

	sources := cfg.Ingestion.EnabledSources
	if *sourceCSV != "" {
		sources = opsutil.SplitCSV(*sourceCSV)
	}
	if len(sources) == 0 {
		return opts, cfg, errors.New("no sources to backfill — set -source or cfg.Ingestion.EnabledSources")
	}

	if err := checkBackfillSourcePolicy(sources, cfg, uint32(*from), uint32(*to)); err != nil {
		return opts, cfg, err
	}

	// The soroban-events pseudo-source is exclusive: it captures
	// every event regardless of per-source decoder routing, so
	// running it alongside other sources would double-bill ledger
	// reads without changing what soroban_events sees. Refuse rather
	// than silently merge.
	if hasSorobanEventsPseudo(sources) && len(sources) > 1 {
		return opts, cfg, fmt.Errorf(
			"-source %q is exclusive — run it alone, not mixed with other source names %v",
			SorobanEventsPseudoSource, sources)
	}

	// Deliberately NOT opsutil.ResolveStreamBucket (RLT-266): that
	// policy's no-seam default is the LIVE bucket, kept for
	// ch-live-catchup.sh, and live cannot hold the historic ranges this
	// command exists to walk. The archive is the full history plus an
	// hourly mirror of live, so it is the right default on both sides of
	// a seam; its one weakness — the mirror lagging a -to near the tip —
	// now fails the chunk in backfillChunkCoverage instead of exiting 0.
	bucket := cfg.Storage.S3BucketArchive
	if *bucketOverride != "" {
		bucket = *bucketOverride
	}
	if bucket == "" {
		return opts, cfg, errors.New("no bucket — set -bucket or cfg.Storage.S3BucketArchive")
	}

	opts = backfillOpts{
		cfgPath:       *cfgPath,
		from:          uint32(*from),
		to:            uint32(*to),
		sources:       sources,
		bucket:        bucket,
		dryRun:        gate.DryRun(),
		resume:        *resume,
		parallel:      *parallel,
		refreshCAGGs:  *refreshCAGGs,
		heartbeatPath: *heartbeatPath,
	}
	return opts, cfg, nil
}

// hasSorobanEventsPseudo reports whether `sources` contains the
// `soroban-events` pseudo-source name. The pseudo-source is the
// catch-all raw-event capture mode; it must be filtered out of
// every BackfillSafe / KnownSources check because it isn't in any
// of those registries — it's the dispatcher's RawEventSink seam.
func hasSorobanEventsPseudo(sources []string) bool {
	for _, s := range sources {
		if strings.EqualFold(s, SorobanEventsPseudoSource) {
			return true
		}
	}
	return false
}

// filterOutSorobanEventsPseudo returns `sources` with any
// `soroban-events` entry removed. Used to feed the remaining
// real-source list to BuildDispatcher / BackfillSafe checks.
func filterOutSorobanEventsPseudo(sources []string) []string {
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		if strings.EqualFold(s, SorobanEventsPseudoSource) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// checkBackfillSourcePolicy is the single source-admission rule for every
// path that replays ledgers through runBackfillChunk: no projector-owned
// source (invariant [7]) and no source without a current WASM-audit
// attestation (BackfillSafe).
func checkBackfillSourcePolicy(sources []string, cfg config.Config, fromLedger, toLedger uint32) error {
	if err := checkBackfillNotProjected(sources, cfg); err != nil {
		return err
	}
	return checkBackfillSources(sources, fromLedger, toLedger)
}

// checkBackfillSources returns nil when every source in `sources`
// is BackfillSafe, otherwise an error explaining which subset
// blocked the run and why. Distinguishes supply-observer names
// from genuinely audit-pending Soroban sources so the operator
// gets a targeted message rather than the generic WASM-audit one.
// F-1243 (audit-2026-05-12).
//
// Special-case: the `soroban-events` pseudo-source is exempt from
// the BackfillSafe gate. It captures raw events to the
// soroban_events landing zone without per-source decoding, so the
// "Soroban DeFi contracts upgrade in place" concern doesn't apply —
// the raw XDR is stored as-is for FUTURE decoders to interpret
// against the right WASM-version mapping.
func checkBackfillSources(sources []string, fromLedger, toLedger uint32) error {
	// Strip the pseudo-source before the BackfillSafe lookup; it
	// isn't in external.Registry by design.
	realSources := filterOutSorobanEventsPseudo(sources)
	unsafeSources := unsafeBackfillSources(realSources)
	if len(unsafeSources) == 0 {
		return nil
	}
	var supplyObservers, sorobanPending []string
	for _, s := range unsafeSources {
		if isKnownSupplyObserverName(s) {
			supplyObservers = append(supplyObservers, s)
		} else {
			sorobanPending = append(sorobanPending, s)
		}
	}
	if len(supplyObservers) > 0 {
		return fmt.Errorf(
			"refusing to backfill — these are supply observers, not price/oracle sources, and don't run "+
				"through `stellarindex-ops backfill`: %v; supply observers plug into the indexer's "+
				"LedgerEntryChange / OpDecoder / SEP-41 event hooks (there's no historical replay path here). "+
				"Use the supply-snapshot systemd timer for current state, or open a wasm-audit ticket if "+
				"you actually need a historical SEP-41 supply window — that's the only one that has a "+
				"chance of working with a future supply-backfill command (F-1243)",
			supplyObservers)
	}
	return fmt.Errorf(
		"refusing to backfill — sources not BackfillSafe (per-WASM-hash audit pending): %v; "+
			"run stellarindex-ops wasm-history -from %d -to %d -contracts <CID> for each on-chain source, "+
			"review every emitted WASM hash against the current decoder, then flip BackfillSafe=true in "+
			"internal/sources/external/registry.go in the same PR (see docs/architecture/domain-traps.md, \"Soroban DeFi contracts "+
			"upgrade in place\")",
		sorobanPending, fromLedger, toLedger)
}

// checkBackfillNotProjected refuses any source the projector writes.
// backfill persists with SinkModeAll and builds gated decoders with an
// empty registry, so a projected source here is either a second writer
// to the projector's tables (invariant [7]) or, for blend, a run that
// writes nothing and exits 0.
func checkBackfillNotProjected(sources []string, cfg config.Config) error {
	var projected, rest []string
	for _, s := range sources {
		if projector.IsProjectedSource(s, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts) {
			projected = append(projected, s)
		} else {
			rest = append(rest, s)
		}
	}
	if len(projected) == 0 {
		return nil
	}
	return fmt.Errorf(
		"refusing to backfill — these sources are written only by the projector (AGENTS.md invariant [7]): %v; "+
			"re-derive them with `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger>` "+
			"(docs/operations/adr-0033-data-recovery.md). Pass -source with the remaining sources to backfill them: %v",
		projected, rest)
}

// unsafeBackfillSources filters `sources` to those whose registry
// entry has BackfillSafe=false. The intent is to fail fast on a list
// the operator can paste into a wasm-history audit ticket.
func unsafeBackfillSources(sources []string) []string {
	var out []string
	for _, s := range sources {
		if !external.BackfillSafe(s) {
			out = append(out, s)
		}
	}
	return out
}

// knownSupplyObserverNames is the closed set of supply-observer
// package names the indexer registers. None of these are in
// external.Registry (supply observers plug into a different
// dispatcher hook than price/oracle sources) — but we want a
// targeted error message when an operator tries to backfill one,
// rather than the generic "WASM-hash audit pending" message that
// drove the F-1243 audit finding.
//
// Update this set when a new supply observer ships under
// internal/supply/. Keeping the list local to backfill.go avoids
// a cross-cutting "supply registry" abstraction for this single
// error-message use case.
var knownSupplyObserverNames = map[string]struct{}{
	"accounts":           {},
	"trustlines":         {},
	"claimable_balances": {},
	"sac_balances":       {},
	"sep41_supply":       {},
	"liquidity_pools":    {},
}

// isKnownSupplyObserverName reports whether `name` matches one of
// the known supply observers. Used by the backfill flag-parser to
// emit a tailored error rather than the generic BackfillSafe one.
func isKnownSupplyObserverName(name string) bool {
	_, ok := knownSupplyObserverNames[name]
	return ok
}

// splitCSV (used here for -source parsing) is defined in
// cross_region_check.go — kept there so this binary has one
// canonical comma-splitting helper across subcommands.
