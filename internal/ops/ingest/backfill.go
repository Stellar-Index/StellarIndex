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

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
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
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// SorobanEventsPseudoSource is the backfill-only source that populates the soroban_events landing zone (ADR-0029).
const SorobanEventsPseudoSource = "soroban-events"

// stellarindex-ops backfill replays a bounded ledger range through the live dispatcher + decoder + sink path. Unlike the indexer it writes no
// cursor on the shared `ledgerstream` row (the indexer would resume from a historical ledger) and refuses sources that are not BackfillSafe
// (contracts upgrade in place). Idempotency is the storage layer's: trades dedupes on (source, ledger, tx_hash, op_index, ts).

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
	// refreshCAGGs controls the post-chunk CAGG materialisation; leave on unless debugging a refresh failure.
	refreshCAGGs bool
	// heartbeat is shared by every chunk goroutine (mutex-guarded, atomic walkedTotal); an inert heartbeat makes every call a no-op.
	heartbeat   *opsutil.JobHeartbeat
	walkedTotal *atomic.Uint64
	// heartbeatPath is the -heartbeat flag: an explicit node_exporter
	// textfile path, or "" for the auto-resolved default.
	heartbeatPath string
	// wasmGate runs the per-WASM replay gate once for the run's whole
	// [from, to]; every chunk shares it. nil gates on each call.
	wasmGate *replayGateOnce
}

// replayGateOnce memoises one run's replay-gate verdict across its chunks.
type replayGateOnce struct {
	once sync.Once
	err  error
}

func (g *replayGateOnce) check(gate func() error) error {
	if g == nil {
		return gate()
	}
	g.once.Do(func() { g.err = gate() })
	return g.err
}

// chunkRange is one sub-range of a parallel backfill: [from, to]
// inclusive. Workers process distinct chunkRanges concurrently;
// each writes its own cursor row keyed on the chunk-specific
// (from, to) so resume-on-restart works per-chunk.
type chunkRange struct {
	from uint32
	to   uint32
}

// planBackfillChunks splits [from, to] into n contiguous sub-ranges; the last absorbs rounding so the union is exact. Callers guarantee n >= 1, to >= from.
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

// backfillCursorSub keys the cursor on (from, to, sorted sources) so concurrent or overlapping runs never share a cursor row.
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

	// Re-processing may overlap live ingest and ON CONFLICT UPDATE the same trade PKs: install the USD-volume resolvers and a positive generation
	// so the rewrite carries real values, not NULL (see reDeriveNullVolumeGuard).
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

	// The heartbeat reports liveness + progress for the whole walk, so a wedged backfill
	// (stalled S3 read, storage write that never returns, OOM-killed chunk
	// goroutine) is distinguishable from a working one without tailing the
	// journal. Inert off-r1 — see opsutil.NewJobHeartbeat.
	opts.wasmGate = &replayGateOnce{}
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

	// Parallel path: each chunk has its own dispatcher, channel, PersistEvents goroutine and cursor row; the shared pool is capped by postgres max_connections.
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

// runChunkGuarded recovers a panic in one chunk (it would otherwise kill the process), reports it via worker.Report and returns it through errCh so the backfill still fails.
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

// buildChunkDispatcher builds the per-chunk dispatcher and, for the soroban-events pseudo-source, wires the RawEventSink (ADR-0029); the sink is nil otherwise.
func buildChunkDispatcher(
	ctx context.Context,
	logger *slog.Logger,
	opts backfillOpts,
	cfg config.Config,
	store *timescale.Store,
	rec dirtyWindowRecorder,
	pseudo bool,
) (*dispatcher.Dispatcher, *sorobanevents.AsyncSink, error) {
	// Every entry point (backfill, resume-stalled) reaches the decoders
	// through here, so the source policy is enforced here, not only at
	// flag parse.
	if err := checkBackfillSourcePolicy(opts.sources, cfg, opts.from, opts.to); err != nil {
		return nil, nil, err
	}
	// Before the dispatcher exists, so no event can be persisted unrecorded.
	if err := recordBackfillDirtyWindows(ctx, rec, cfg, opts); err != nil {
		return nil, nil, err
	}
	if err := recordRawBackfillProjectorWindows(ctx, logger, rec, cfg, opts); err != nil {
		return nil, nil, err
	}
	realSources := filterOutSorobanEventsPseudo(opts.sources)
	if err := opts.wasmGate.check(func() error {
		return wasmaudit.GateReplay(ctx, cfg.Storage.ClickHouseAddr, cfg.Oracle, store.LoadProtocolContracts, realSources, opts.from, opts.to)
	}); err != nil {
		return nil, nil, err
	}

	var soroswapOpts []soroswap.DecoderOption
	if !pseudo && len(realSources) > 0 {
		// Each chunk loads and arms the Soroswap pair registry itself; the store is shared, so chunks see each other's upserts (pipeline/soroswap_registry.go).
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

// runBackfillChunk processes one chunk end to end; its cursor sub_source uses chunk.from/to, never the overall range, so chunks never share a cursor row.
//
//nolint:gocognit,funlen // chunk lifecycle is linear setup → stream → teardown; splitting reduces readability of dependency-construction order.
func runBackfillChunk(ctx context.Context, logger *slog.Logger, opts backfillOpts, cfg config.Config, store *timescale.Store, chunk chunkRange) error { //nolint:gocyclo // one cohesive backfill orchestration: chunk stream -> async drain -> post-drain durable cursor; splitting scatters the watermark narrative
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
	disp, rawSink, err := buildChunkDispatcher(ctx, logger, opts, cfg, store, store, pseudo)
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
		persistLoss = pipeline.PersistEvents(ctx, logger, store, events, pipeline.SinkModeAll, nil)
	}()

	// Ctx-cancel safety net (ADR-0029): PushEvent back-pressure has no ctx awareness, so early-Stop the sink on cancel to unblock the dispatcher.
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
	// Count actual LCM callbacks: a chunk that walked nothing must not log a range size as "ledgers=" and read as complete.
	var (
		walked uint64
		// lastFullyEnqueued is the last ledger fully enqueued onto the sink channel, not yet durable; the cursor advances only after the drain confirms it landed.
		lastFullyEnqueued uint32
	)
	streamErr := ledgerstream.Stream(ctx, streamCfg, startFrom, chunk.to,
		func(lcm sdkxdr.LedgerCloseMeta) error {
			walked++
			if err := pipeline.ProcessLedger(ctx, disp, events, logger, lcm, cfg.Stellar.Passphrase()); err != nil {
				return err
			}
			// Report into the run-wide heartbeat AFTER the ledger
			// was actually processed — a counter bumped before the work
			// would keep advancing while ProcessLedger wedges, which is
			// the exact hang the progress alert exists to catch.
			if opts.walkedTotal != nil {
				opts.heartbeat.Progress(opts.walkedTotal.Add(1), uint64(lcm.LedgerSequence()))
			}
			// Do not advance the cursor here: rows are still buffered, and a crash before the async commit would lose them behind the watermark. The durable advance follows <-sinkDone and rawSink.Stop().
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

	// The cursor advances only after the sink drained (ADR-0041), the rawSink flushed, and the CAGG refresh succeeded or was skipped: the resume gate
	// (`c.LastLedger >= chunk.to`) would otherwise trust a chunk whose trades are invisible to served views.
	streamFailed := streamErr != nil && !errors.Is(streamErr, context.Canceled)
	if streamFailed || (lossErr != nil && ctx.Err() == nil) {
		// Stream aborted or a sink left rows unlanded: still checkpoint what drained so a resume continues from the failure point. No CAGG refresh wait; a resume does its own before its checkpoint.
		checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment
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
		checkpointBackfillChunk(logger, store, cursorSub, interruptedCheckpoint(chunk, lastFullyEnqueued), startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment
		if lossErr != nil {
			ierr = errors.Join(ierr, lossErr)
		}
		return ierr
	}

	// Report the chunk's range and the walked count: a complete miss on a non-zero range is almost always a wrong bucket and must fail loudly, as must a partial walk
	// (backfillChunkCoverage). This runs BEFORE the CAGG refresh and completing checkpoint; drained rows are still checkpointed so -resume continues from the truncation point.
	if cerr := backfillChunkCoverage(chunk, startFrom, walked, opts.bucket); cerr != nil {
		logger.Error("chunk INCOMPLETE",
			"from", chunk.from,
			"to", chunk.to,
			"start_from", startFrom,
			"ledgers_walked", walked,
			"last_fully_enqueued", lastFullyEnqueued,
		)
		checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment
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
		// Force-materialise every CAGG rooted on a table the chunk wrote ([timescale.TradesCAGGs], [timescale.OracleCAGGs]); an omitted view keeps a permanent hole in served OHLC/VWAP.
		// A refresh failure is fatal to the chunk and the cursor does not advance, so a resume re-attempts it.
		if err := refreshCAGGsForChunk(ctx, logger, store, chunk); err != nil {
			return fmt.Errorf("post-chunk CAGG refresh: %w", err)
		}
	default:
		logger.Warn("skipping CAGG refresh (-refresh-caggs=false)",
			"impact", "the range's buckets in every trades aggregate (prices_*, twap_*, dex_volume_by_pair_1d, source_volume_1h, pools_per_source_1h) and every oracle_prices_* rung stay unmaterialised until a manual refresh_continuous_aggregate covers them — the CAGG policies only roll forward, so a historical range is never picked up on their own cadence. The trades and oracle rows are durable, but every OHLC/VWAP/TWAP, volume and oracle-history read over the range is short until then",
		)
	}

	checkpointBackfillChunk(logger, store, cursorSub, lastFullyEnqueued, startFrom) //nolint:contextcheck // deliberately does NOT use the caller's ctx — see checkpointBackfillChunk's doc comment
	return nil
}

// backfillChunkCoverage turns a chunk walk that did not cover its range into a hard error naming the bucket.
// Fail closed: ledgerstream tolerates a missing object anywhere in the chunk (TolerateTrailingMissing is measured against the chunk's own `to`) and
// stops short without error, so walked == 0 alone fails open and leaves a hole whose only evidence is a success. The archive mirrors live hourly, so a
// `-to` near the tip also comes up short; a `-resume` re-run is cheap (writes are idempotent). startFrom is the post-resume start, so earlier-banked ledgers are not re-charged.
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

// drainRawSink stops the soroban-events sink (nil when not wired), flushes its buffer and returns the lowest ledger it dropped or abandoned; a graceful SIGINT under back-pressure also drops rows.
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

// capCheckpointAtLoss lowers the resume checkpoint below the first row a sink failed to land and returns an error naming the loss. A loss with no ledger gives no safe cap, so the checkpoint falls below startFrom and a resume re-walks the remainder.
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
// would let `backfill` log "backfill complete" and exit 0 on a partial range.
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

// checkpointBackfillChunk durably advances the cursor to lastFullyEnqueued (no-op if nothing new). It uses a FRESH bounded context: on SIGINT the parent ctx is already cancelled and would discard the resume watermark.
func checkpointBackfillChunk(logger *slog.Logger, store *timescale.Store, cursorSub string, lastFullyEnqueued, startFrom uint32) {
	if lastFullyEnqueued == 0 || lastFullyEnqueued < startFrom {
		return
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := store.UpsertCursor(cctx, backfillCursorSource, cursorSub, lastFullyEnqueued); err != nil { //nolint:contextcheck // deliberate fresh ctx: the parent is already canceled on a graceful SIGINT — using it would silently drop the resume watermark; see the doc comment above
		logger.Warn("backfill cursor upsert (post-drain)",
			"ledger", lastFullyEnqueued,
			"err", err)
	}
}

// caggRefresher is the storage seam for refreshCAGGsForChunk, so its failure aggregation is testable with a fake; *timescale.Store satisfies it.
type caggRefresher interface {
	LedgerRangeToTimeRange(ctx context.Context, from, to uint32) (time.Time, time.Time, error)
	LedgerRangeToOracleTimeRange(ctx context.Context, from, to uint32) (time.Time, time.Time, error)
	Prices1mRetentionArmed(ctx context.Context) (bool, error)
	timescale.CAGGStepRefresher
}

// caggRefreshMu serialises CAGG refreshes across `-parallel` workers in this process. TimescaleDB rejects the loser of two concurrent
// refreshes of the same aggregate with 55P03, and the bounded retry is not enough for prices_1m; the lock is broader than the race on purpose.
// Decode + insert stay parallel outside it. Consider a per-view lock if the refresh tail ever dominates.
var caggRefreshMu sync.Mutex

// refreshCAGGsForChunk refreshes every aggregate rooted on a table the chunk wrote in [timescale.PlanCAGGRefresh] order. Idempotent.
// Every independent view is attempted after one fails, but any failure returns an error so the cursor does not advance; views built on prices_1m
// are skipped when its refresh failed, since recomputing from stale minute rows would overwrite good history.
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

// logCAGGRefreshFailure logs one failed view. A per-CALL bound firing
// names the view, the window and the bound, because an unbounded refresh
// would hold every `-parallel` worker behind caggRefreshMu until SIGINT.
// It is still fatal to the chunk.
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
// so that function stays under the funlen ceiling.
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

// parseBackfillFlags parses args, loads config and validates (including the BackfillSafe gate); split out so validation is testable without postgres + galexie.
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

	// The soroban-events pseudo-source is exclusive: combining it would double-bill ledger reads and change nothing, so refuse rather than merge.
	if hasSorobanEventsPseudo(sources) && len(sources) > 1 {
		return opts, cfg, fmt.Errorf(
			"-source %q is exclusive — run it alone, not mixed with other source names %v",
			SorobanEventsPseudoSource, sources)
	}

	// Deliberately NOT opsutil.ResolveStreamBucket: its no-seam default is the LIVE bucket, which cannot hold the historic ranges
	// backfill walks. The archive is full history plus an hourly mirror of live; a -to near the tip fails in backfillChunkCoverage.
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

// hasSorobanEventsPseudo reports whether sources contains the pseudo-source, which must be excluded from BackfillSafe/KnownSources checks (it is in no registry).
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

// dirtyWindowRecorder is the slice of the store that records a projection
// dirty window (migration 0125) and reads the cursor it is measured against.
type dirtyWindowRecorder interface {
	RecordProjectionDirtyWindow(ctx context.Context, w timescale.ProjectionDirtyWindow) error
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

// recordBackfillDirtyWindows records a dirty window over [opts.from, opts.to] for every audited source: backfill rewrites served rows below the verdict watermark, voiding any carried-over clean claim. Any error must stop the backfill before its first write.
func recordBackfillDirtyWindows(ctx context.Context, rec dirtyWindowRecorder, cfg config.Config, opts backfillOpts) error {
	if opts.dryRun {
		return nil
	}
	audited := completeness.AuditedSources(cfg)
	for _, src := range opts.sources {
		if !slices.Contains(audited, src) {
			continue
		}
		if err := rec.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
			Source: src, From: opts.from, To: opts.to,
			Reason: timescale.BackfillWriteReason(opts.from, opts.to),
		}); err != nil {
			return fmt.Errorf("record dirty window for %s before writing (refusing to backfill): %w", src, err)
		}
	}
	return nil
}

// recordRawBackfillProjectorWindows covers a soroban-events backfill landing raw rows for a projected source: the projector never reads behind its cursor,
// so those rows stay unprojected until a projector-replay rewinds it. The backfill must not project them itself (invariant [7]); it records a dirty window
// over the FULL [from,to] (the cursor is read once, before the run), which only a completeness verdict clears.
func recordRawBackfillProjectorWindows(ctx context.Context, logger *slog.Logger, store dirtyWindowRecorder, cfg config.Config, opts backfillOpts) error {
	if opts.dryRun || cfg.Storage.ClickHouseProjectorSource || !hasSorobanEventsPseudo(opts.sources) {
		return nil
	}
	for _, src := range completeness.AuditedSources(cfg) {
		if !projector.IsProjectedSource(src, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts) {
			continue
		}
		cur, err := store.GetCursor(ctx, "projector", src)
		if errors.Is(err, timescale.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read projector cursor for %s (refusing to backfill raw events it may never project): %w", src, err)
		}
		if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
			Source: src, From: opts.from, To: opts.to,
			Reason: timescale.BackfillWriteReason(opts.from, opts.to),
		}); err != nil {
			return fmt.Errorf("record dirty window for %s before writing (refusing to backfill): %w", src, err)
		}
		logger.Warn("soroban-events backfill may land at or below the projector cursor; the projector will not re-project it on its own — after this backfill completes, run the replay command",
			"source", src, "projector_cursor", cur.LastLedger, "unprojected_from", opts.from, "unprojected_to", opts.to,
			"command", fmt.Sprintf("stellarindex-ops projector-replay -config %s -source %s -from %d", opts.cfgPath, src, opts.from))
	}
	return nil
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

// checkBackfillSources returns nil when every source is BackfillSafe, else an error naming the blocked subset; supply-observer names get a targeted message.
// The soroban-events pseudo-source is exempt: raw XDR is stored as-is, so the upgrade-in-place concern does not apply.
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
			"review every emitted WASM hash against the current decoder, then audit each WASM hash under docs/operations/wasm-audits/, add it to internal/wasmaudit/audited_wasm.json and set Backfill: BackfillPerWASM in internal/sources/external/registry.go in the same PR (see docs/architecture/domain-traps.md, \"Soroban DeFi contracts "+
			"upgrade in place\")",
		sorobanPending, fromLedger, toLedger)
}

// checkBackfillNotProjected refuses any source the projector writes: it would be a second writer (invariant [7]) or, for blend, a run that writes nothing and exits 0.
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

// knownSupplyObserverNames is the closed set of supply-observer names the indexer registers, none in external.Registry; it gives a targeted error on backfill.
// Update it when a new observer ships under internal/supply/.
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
