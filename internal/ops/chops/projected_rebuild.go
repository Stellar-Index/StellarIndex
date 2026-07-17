// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// projectedRebuild is the ADR-0048 D3 bulk catch-up path for projected
// (Soroban-derived) sources: `stellarindex-ops projected-rebuild -config
// PATH -source NAME -from N [-to N] [-workers K] [-window N] [-resume]
// [-write]`.
//
// WHY THIS EXISTS: `projector-replay` rewinds the LIVE projector's cursor
// and lets its own tick-cadence catch-up walk the range — Interval-bound
// (one ≤BatchLimit=1,000-ledger window per 5s tick, PerSourceTimeout=60s
// deadline per cycle), a ceiling of roughly 720k ledgers/hour. That is
// fine for small rewinds but hopeless for a multi-million-ledger held job
// (the 2026-07 r1 backlog: blend_backstop from ledger 51.5M, blend_emitter
// from 51.5M, aquarius rewards from 52.7M — each ~11-12M ledgers, i.e.
// 15-17+ hours apiece at the projector's ceiling with ZERO parallelism).
// projected-rebuild removes the tick/deadline ceiling entirely and adds
// worker parallelism, at 10-20x the projector-replay rate (ADR-0048 D3).
//
// IDENTICAL ROWS: this tool builds the exact same decoder the live
// projector uses for the requested source (projector.BuildRegistry, the
// same registry.go the indexer's projector goroutine builds from) and
// writes through the exact same sink (pipeline.HandleEvent — the function
// the projector's own SinkFunc wraps in cmd/stellarindex-indexer/main.go).
// Every per-source table's ON CONFLICT DO NOTHING makes the two writers
// idempotent against each other, so overlap or a retried window is always
// safe at the ROW level.
//
// ONE-WRITER CONTRACT (ADR-0048 D3, loudly, because getting this wrong
// means two writers racing the SAME historical range — still row-safe via
// ON CONFLICT, but wasteful and confusing to operate): this tool does NOT
// touch the live projector's cursor. It requires, by default, that the
// live projector's cursor for this source already be AT OR ABOVE the
// requested rebuild's -to — i.e. the source is live-current and this bulk
// job only fills HISTORY BEHIND the live tail, a range the live tail will
// never walk into again. If the live cursor is still inside [-from,-to]
// the command REFUSES to run (see checkLiveCursorGuard) unless the
// operator passes -allow-live-overlap, which is an explicit "I have
// verified the live projector will not touch this range concurrently"
// override. The live projector keeps running at tip throughout — this
// tool never pauses, parks, or rewinds it.
//
// CHECKPOINTING: independent of the projector's own cursor. Each ledger
// window gets its own row in ingestion_cursors
// (source="projected-rebuild", sub_source="<name>:<wlo>-<whi>"),
// written only in -write mode after that window's stream completes.
// -resume consults these (see loadDoneWindows/pendingWindows) so a
// crashed or interrupted multi-hour run picks up exactly where it left
// off, regardless of which of the K workers happened to claim which
// window.
//
// See docs/architecture/ingest-pipeline.md's catch-up discussion and
// docs/operations/runbooks/projector-replay.md for when to use this vs
// projector-replay (rule of thumb: projector-replay for rewinds under
// ~1M ledgers, projected-rebuild for anything bigger).
func projectedRebuild(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse+validate, build the live decoder, the live-cursor guard, run, report — splitting scatters the guard rationale away from its call site.
	fs := flag.NewFlagSet("projected-rebuild", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	sourceName := fs.String("source", "", "projector source name to rebuild (required); see internal/projector/registry.go for the list")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive); 0 = default to the live projector cursor's CURRENT position, i.e. fill exactly the history behind the live tail (ADR-0048 D3)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	window := fs.Uint("window", projectedRebuildDefaultWindow, "ledger-window size per checkpoint/scheduling unit — smaller gives finer resume granularity and better load balance across workers on uneven-density ranges (e.g. aquarius rewards); does NOT bound memory (this tool streams, never buffers a window)")
	workers := fs.Int("workers", projectedRebuildDefaultWorkers, "concurrent ledger-window workers; soft-capped at 8 (see RunProjectedRebuild's PG pool-sizing note)")
	resume := fs.Bool("resume", true, "skip windows already checkpointed by a prior -write run for this source")
	write := fs.Bool("write", false, "actually write to Postgres via pipeline.HandleEvent (default: dry-run, count + report only, no checkpoints)")
	allowLiveOverlap := fs.Bool("allow-live-overlap", false, "DANGEROUS: bypass the live-cursor guard and run even though the live projector's cursor is inside [-from,-to]. Only pass this if you have independently verified the live projector will not process this range concurrently — see the ADR-0048 D3 one-writer contract in this command's doc comment.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *sourceName == "" || *from == 0 {
		return fmt.Errorf("-config, -source, and -from are required")
	}

	windowSize := uint32(*window) //nolint:gosec // operator-supplied window size; zero guarded below.
	if windowSize == 0 {
		windowSize = projectedRebuildDefaultWindow
	}
	numWorkers := *workers
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > projectedRebuildMaxWorkers {
		fmt.Fprintf(os.Stderr, "projected-rebuild: -workers=%d exceeds the soft cap of %d — clamping\n", numWorkers, projectedRebuildMaxWorkers)
		numWorkers = projectedRebuildMaxWorkers
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	// Long-lived: cancels only on SIGINT/SIGTERM, NOT on a fixed deadline —
	// the entire point of this tool vs projector-replay's 60s
	// PerSourceTimeout-bound cycles.
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()
	// Re-derive path (INV-3 / migration 0109): stamp a positive
	// derive_generation so this projection's corrected values UPDATE the
	// stored rows in place via pipeline.HandleEvent (which draws the
	// generation from this store) and win over the live gen-0 values.
	// Harmless in the default dry-run (no writes occur until -write).
	store.SetDeriveGeneration(time.Now().Unix())

	logger := opsutil.MkBackfillLogger()

	// Build the SAME decoder the live projector uses for this source.
	// withHook=false: a read-only gate warm, matching every other
	// read-only ClickHouse-lake audit/rebuild tool (verify-recognition,
	// ch-recognition, compute-completeness) — the bulk path must not
	// mutate the contractid registry's live-upsert hook mid-run; a
	// genuinely new factory child discovered while replaying history
	// converges on the next `seed-protocol-contracts` run or indexer
	// restart, same as those other read-only tools.
	gatedOpts, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		return fmt.Errorf("gated registry warm: %w", err)
	}
	// Soroswap carries its own pair-registry persistence options
	// (separate from the generic gated-registry seam); wired
	// unconditionally to match cmd/stellarindex-indexer/main.go's
	// production registry construction exactly. Inert for any
	// non-soroswap -source.
	soroswapOpts, err := pipeline.SoroswapPersistenceOptions(ctx, store, logger, ctx)
	if err != nil {
		return fmt.Errorf("soroswap persistence options: %w", err)
	}
	registry, err := projector.BuildRegistry([]string{*sourceName}, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts, gatedOpts, soroswapOpts...)
	if err != nil {
		return fmt.Errorf("build projector registry: %w", err)
	}
	if len(registry.Sources) == 0 {
		return fmt.Errorf("projected-rebuild: %q is not a projector source (see internal/projector/registry.go) — non-projected sources (sdex, band, soroswap-router, external CEX/FX) have their own catch-up paths, and a sep41 source name also resolves empty here when no contracts are watched (cfg.Supply.WatchedSEP41Contracts)", *sourceName)
	}
	src := registry.Sources[0]

	// ─── ADR-0048 D3 one-writer contract: the live-cursor guard ─────────
	liveCursor, gerr := store.GetCursor(ctx, "projector", *sourceName)
	haveLive := gerr == nil
	if gerr != nil && !errors.Is(gerr, timescale.ErrNotFound) {
		return fmt.Errorf("read live projector cursor: %w", gerr)
	}

	toLedger := uint32(*to) //nolint:gosec // ledger sequences fit uint32 in real usage.
	if toLedger == 0 {
		if !haveLive {
			return fmt.Errorf("projected-rebuild: no live projector cursor for source %q and -to not specified — pass -to explicitly, or start the live projector for this source first so -to can default to its current position", *sourceName)
		}
		toLedger = liveCursor.LastLedger
		fmt.Fprintf(os.Stderr, "projected-rebuild: -to not specified — defaulting to the live projector cursor (%d)\n", toLedger)
	}
	fromLedger := uint32(*from) //nolint:gosec // see above
	if toLedger < fromLedger {
		return fmt.Errorf("-to (%d) must be >= -from (%d)", toLedger, fromLedger)
	}

	if gerr := checkLiveCursorGuard(haveLive, liveCursor.LastLedger, toLedger, *allowLiveOverlap); gerr != nil {
		return gerr
	}

	fmt.Fprintf(os.Stderr, "projected-rebuild: source=%s range=[%d,%d] window=%d workers=%d mode=%s ch=%s\n",
		*sourceName, fromLedger, toLedger, windowSize, numWorkers, writeModeLabel(*write), *chAddr)

	result, runErr := RunProjectedRebuild(ctx, ProjectedRebuildOptions{
		Store:   store,
		ChAddr:  *chAddr,
		Source:  src,
		From:    fromLedger,
		To:      toLedger,
		Window:  windowSize,
		Workers: numWorkers,
		Write:   *write,
		Resume:  *resume,
		Logger:  logger,
	})
	printProjectedRebuildSummary(*sourceName, fromLedger, toLedger, *write, result)

	if runErr != nil {
		if ctx.Err() != nil {
			// SIGINT/SIGTERM mid-run — not a failure; the completed
			// windows are checkpointed (if -write) and a re-run with
			// -resume picks up the rest.
			fmt.Fprintf(os.Stderr, "projected-rebuild: interrupted — re-run with -resume to continue from the last completed window\n")
			return nil
		}
		return fmt.Errorf("projected-rebuild: %w", runErr)
	}
	return nil
}

const (
	// projectedRebuildCursorSource is the ingestion_cursors "source"
	// column value for this tool's own per-window checkpoints — distinct
	// from "projector" (the live tail's cursor, never touched here).
	projectedRebuildCursorSource = "projected-rebuild"

	// projectedRebuildDefaultWindow: unlike classic-movements-backfill's
	// 500k (which buffers a whole window's rows before one batch INSERT),
	// this tool streams — HandleEvent writes each decoded event as soon
	// as it's decoded, never buffering a window in memory. So the window
	// size here controls only checkpoint/scheduling granularity: smaller
	// windows give more resume points (less rework after a crash) and
	// spread better across workers when density is uneven (aquarius
	// rewards can be far denser per-ledger than blend_backstop).
	projectedRebuildDefaultWindow = 50_000

	// projectedRebuildDefaultWorkers / Max: see RunProjectedRebuild's
	// PG-pool-sizing doc note.
	projectedRebuildDefaultWorkers = 4
	projectedRebuildMaxWorkers     = 8

	// projectedRebuildProgressInterval is the default periodic
	// progress-log cadence.
	projectedRebuildProgressInterval = 15 * time.Second
)

// checkLiveCursorGuard is the ADR-0048 D3 one-writer contract, pulled out
// as a pure function so it's unit-testable without a database. Refuses
// (returns a non-nil error) unless the live projector's cursor for this
// source is AT OR ABOVE `to` — i.e. the live tail has already passed
// through the entire requested range and this bulk job only fills history
// strictly behind it. allowOverlap bypasses the check entirely (the
// operator's explicit "I have verified this is safe" override).
//
// haveLive=false (no live cursor row at all — the projector has never run
// for this source) is treated as "cursor at 0": conservative refusal by
// default, since a never-run live projector WILL eventually walk this
// range once it starts.
func checkLiveCursorGuard(haveLive bool, liveLastLedger, to uint32, allowOverlap bool) error {
	if allowOverlap {
		return nil
	}
	if haveLive && liveLastLedger >= to {
		return nil
	}
	cur := "none (projector has never run for this source)"
	if haveLive {
		cur = fmt.Sprintf("%d", liveLastLedger)
	}
	return fmt.Errorf("projected-rebuild: refusing to run — live projector cursor is %s, which is below the requested rebuild top %d. "+
		"ADR-0048 D3's one-writer contract: the bulk path only fills HISTORY BEHIND a live-current source — it must never write a range the live tail might still walk into concurrently. "+
		"Wait for the live projector to pass ledger %d, lower -to, or pass -allow-live-overlap if you have independently verified this is safe",
		cur, to, to)
}

// ProjectedRebuildOptions bundles projected-rebuild's already-resolved
// dependencies. Kept separate from the CLI flag parser (projectedRebuild)
// so the core executor is directly callable — by the CLI shell after it
// resolves config/flags/the live-cursor guard, and by tests without a TOML
// config file or a running live projector. Exported for the
// integration-test seam (test/integration), the same rationale as
// pipeline.HandleEvent / pipeline.GatedRegistryOptions being exported.
type ProjectedRebuildOptions struct {
	Store  *timescale.Store
	ChAddr string
	// Source is the ONE projector source to rebuild: the same Decoder +
	// ContractIDs/Topic0Syms/ExcludeTopic0Syms prefilter the live
	// projector.Registry carries for it. Build via projector.BuildRegistry
	// (as projectedRebuild does) so the bulk path and the live projector
	// decode byte-identically — ADR-0048 D3's "identical rows" property.
	Source projector.Source
	From   uint32
	To     uint32
	// Window is the ledger span per checkpoint/scheduling unit. 0 uses
	// projectedRebuildDefaultWindow.
	Window uint32
	// Workers is the number of concurrent window-claiming goroutines.
	// Values < 1 are treated as 1.
	//
	// PG pool-sizing note: every worker's pipeline.HandleEvent call draws
	// a connection from the SAME *timescale.Store's pool
	// (timescale.PoolMaxOpenConns = 25) for the duration of one row's
	// insert, then releases it — database/sql handles the sharing, no
	// per-worker pool needed. PersistEvents' own live-tail drain already
	// runs 8 such workers against this identical pool ceiling (see
	// internal/pipeline/sink.go's PersistWorkers doc), so the
	// projectedRebuildMaxWorkers=8 soft cap here is comfortably inside
	// the same budget it has always operated in — no pool bump required
	// for a single stellarindex-ops process, even run alongside a live
	// indexer/aggregator/api on the same host.
	Workers int
	// Write persists decoded events via pipeline.HandleEvent AND records
	// a per-window checkpoint on success. false = dry-run: counts and
	// reports only — no writes, no checkpoints (mirrors
	// classic-movements-backfill's -write gate).
	Write bool
	// Resume skips windows already checkpointed by a prior -write run for
	// this source. Consulted even in dry-run mode, so a dry-run preview
	// after a partial -write run reflects the real remaining work.
	Resume bool
	Logger *slog.Logger
	// ProgressInterval overrides the periodic progress-log cadence; <= 0
	// uses projectedRebuildProgressInterval.
	ProgressInterval time.Duration
}

// ProjectedRebuildResult summarises one RunProjectedRebuild call.
type ProjectedRebuildResult struct {
	WindowsPlanned   int
	WindowsSkipped   int // already checkpointed by a prior -write run (resume)
	WindowsProcessed int
	LedgersCovered   int64 // sum of processed windows' ledger spans, THIS run
	EventsRead       int64
	EventsEmitted    int64
	DecodeErrors     int64
	// KindCounts is emitted-event count by consumer.Event.EventKind() —
	// the "per-topic emitted counts" report so an operator can eyeball
	// against the census tables. ADR-0033 compute-completeness remains
	// the authoritative verdict.
	KindCounts map[string]int64
	Elapsed    time.Duration
}

// RunProjectedRebuild is the core executor: builds the window plan,
// filters already-done windows when Resume is set, then runs Workers
// goroutines that each repeatedly claim the next pending window off a
// shared scheduler (windowScheduler.claim — an atomic index, not a static
// per-worker split like ch-backfill's SplitRange, so uneven per-ledger
// density — e.g. a dense aquarius-rewards stretch next to a quiet one —
// doesn't strand one worker on a giant window while others idle) and
// stream it from the ClickHouse lake through the source's decoder into
// pipeline.HandleEvent.
//
// Never buffers a window's rows: HandleEvent is called inline from the
// ClickHouse stream callback, exactly like the live projector's own
// per-event sink — satisfies the "stream, don't buffer" requirement for
// running under run-heavy-job.sh on r1 (MemoryMax=20G).
//
// A window's stream error is fatal to the whole run (propagated through
// errgroup, cancelling sibling workers) except when ctx itself was
// canceled (SIGINT/SIGTERM) — the caller distinguishes the two via
// ctx.Err(). Either way, only FULLY completed windows are checkpointed;
// -resume picks up exactly where a crash or interrupt left off. A
// per-event decode error (Decoder.Decode returning non-nil) is instead a
// soft-fail — logged and counted, the window still completes and
// checkpoints — because a deterministically malformed row would just
// re-fail identically on retry (same policy as
// internal/projector.processEventSafely).
func RunProjectedRebuild(ctx context.Context, opts ProjectedRebuildOptions) (ProjectedRebuildResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	windowSize := opts.Window
	if windowSize == 0 {
		windowSize = projectedRebuildDefaultWindow
	}
	numWorkers := opts.Workers
	if numWorkers < 1 {
		numWorkers = 1
	}

	plan := buildWindowPlan(opts.From, opts.To, windowSize)
	result := ProjectedRebuildResult{WindowsPlanned: len(plan), KindCounts: map[string]int64{}}
	if len(plan) == 0 {
		return result, nil
	}

	pending := plan
	if opts.Resume {
		done, derr := loadDoneWindows(ctx, opts.Store, opts.Source.Name)
		if derr != nil {
			return result, fmt.Errorf("projected-rebuild: load resume checkpoints: %w", derr)
		}
		pending = pendingWindows(plan, opts.Source.Name, done)
	}
	result.WindowsSkipped = len(plan) - len(pending)
	if len(pending) == 0 {
		return result, nil
	}

	sched := newWindowScheduler(pending)
	counters := newProjectedRebuildCounters()
	start := time.Now()

	pendingLedgers := int64(0)
	for _, w := range pending {
		pendingLedgers += int64(w.To-w.From) + 1
	}

	progressCtx, stopProgress := context.WithCancel(ctx)
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		runProjectedRebuildProgressLoop(progressCtx, logger, opts.Source.Name, opts.ProgressInterval,
			start, len(pending), pendingLedgers, counters)
	}()

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			return runProjectedRebuildWorker(gctx, sched, opts, logger, counters)
		})
	}
	runErr := g.Wait()
	stopProgress()
	progressWG.Wait()

	result.WindowsProcessed = int(counters.windowsDone.Load())
	result.LedgersCovered = counters.completedLedgers.Load()
	result.EventsRead = counters.eventsRead.Load()
	result.EventsEmitted = counters.eventsEmitted.Load()
	result.DecodeErrors = counters.decodeErrors.Load()
	result.KindCounts = counters.kindCounts
	result.Elapsed = time.Since(start)
	return result, runErr
}

// projectedRebuildCounters holds the atomic progress counters
// RunProjectedRebuild's worker pool and progress-log goroutine share.
// Bundled into one type so both call sites take a single parameter
// instead of five separate atomic pointers.
type projectedRebuildCounters struct {
	windowsDone      atomic.Int64
	completedLedgers atomic.Int64
	eventsRead       atomic.Int64
	eventsEmitted    atomic.Int64
	decodeErrors     atomic.Int64
	kindMu           sync.Mutex
	kindCounts       map[string]int64
}

func newProjectedRebuildCounters() *projectedRebuildCounters {
	return &projectedRebuildCounters{kindCounts: map[string]int64{}}
}

func (c *projectedRebuildCounters) addKind(k string) {
	c.kindMu.Lock()
	c.kindCounts[k]++
	c.kindMu.Unlock()
}

// runProjectedRebuildWorker repeatedly claims the next pending window off
// sched — via windowScheduler.claim's shared atomic counter, so a worker
// that finishes a quiet window immediately picks up the next one instead
// of idling behind a sibling stuck on a dense stretch — streams it from
// the ClickHouse lake through opts.Source's decoder, and (in -write mode)
// sinks each decoded event through pipeline.HandleEvent and checkpoints
// the window on success. Returns the first fatal stream error (if any); a
// per-event decode error is instead a soft-fail (see decodeProjectedEvent)
// and does not abort the window.
func runProjectedRebuildWorker(ctx context.Context, sched *windowScheduler, opts ProjectedRebuildOptions, logger *slog.Logger, counters *projectedRebuildCounters) error {
	for {
		w, ok := sched.claim()
		if !ok {
			return nil
		}
		var windowRead, windowEmitted int64
		werr := clickhouse.StreamContractEventsFiltered(ctx, opts.ChAddr, w.From, w.To,
			opts.Source.ContractIDs, opts.Source.Topic0Syms, opts.Source.ExcludeTopic0Syms,
			false, // no FINAL: idempotent downstream writes absorb any duplicate (matches the live projector's CH feed-switch mode)
			true,  // withOpArgs: mirrors the live projector, which routes every source uniformly (redstone needs it; the window is bounded so the wide column is cheap)
			func(ev events.Event) error {
				windowRead++
				outs, softFail := decodeProjectedEvent(opts.Source.Name, opts.Source.Decoder, ev, logger)
				if softFail {
					counters.decodeErrors.Add(1)
					return nil
				}
				for _, out := range outs {
					windowEmitted++
					counters.addKind(out.EventKind())
					if opts.Write {
						pipeline.HandleEvent(ctx, logger, opts.Store, out)
					}
				}
				return nil
			})
		counters.eventsRead.Add(windowRead)
		counters.eventsEmitted.Add(windowEmitted)
		if werr != nil {
			return fmt.Errorf("window [%d,%d]: %w", w.From, w.To, werr)
		}
		if opts.Write {
			sub := windowCursorSub(opts.Source.Name, w)
			if cerr := opts.Store.UpsertCursor(ctx, projectedRebuildCursorSource, sub, w.To); cerr != nil {
				logger.Warn("projected-rebuild: checkpoint failed", "window_sub", sub, "err", cerr)
			}
		}
		counters.completedLedgers.Add(int64(w.To-w.From) + 1)
		counters.windowsDone.Add(1)
	}
}

// runProjectedRebuildProgressLoop prints one progress line every interval
// (default projectedRebuildProgressInterval) until progressCtx is done.
// Extracted so RunProjectedRebuild's main body stays about the run
// control-flow, not log formatting.
func runProjectedRebuildProgressLoop(
	progressCtx context.Context, logger *slog.Logger, sourceName string, interval time.Duration,
	start time.Time, totalWindows int, pendingLedgers int64,
	counters *projectedRebuildCounters,
) {
	if interval <= 0 {
		interval = projectedRebuildProgressInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-progressCtx.Done():
			return
		case <-t.C:
			elapsed := time.Since(start)
			done := counters.completedLedgers.Load()
			rate := float64(done) / elapsed.Seconds()
			eta := "unknown"
			if rate > 0 {
				remaining := pendingLedgers - done
				if remaining < 0 {
					remaining = 0
				}
				eta = time.Duration(float64(remaining) / rate * float64(time.Second)).Round(time.Second).String()
			}
			logger.Info("projected-rebuild progress",
				"source", sourceName,
				"windows_done", counters.windowsDone.Load(), "windows_total", totalWindows,
				"ledgers_covered", done, "ledgers_per_sec", fmt.Sprintf("%.1f", rate),
				"events_read", counters.eventsRead.Load(), "events_emitted", counters.eventsEmitted.Load(),
				"decode_errors", counters.decodeErrors.Load(), "eta", eta)
		}
	}
}

// buildWindowPlan divides [from,to] into contiguous, non-overlapping
// windows of at most `window` ledgers each, covering [from,to] exactly —
// same loop shape as classic-movements-backfill's per-window walk, but
// returning the WHOLE plan up front (rather than processing inline)
// because dynamic worker scheduling needs the full window set available
// before dispatch. Returns nil for an empty/invalid range.
func buildWindowPlan(from, to, window uint32) []opsutil.RangeChunk {
	if to < from || window == 0 {
		return nil
	}
	var out []opsutil.RangeChunk
	for wlo := from; ; {
		whi := wlo + window - 1
		if whi > to || whi < wlo { // whi<wlo guards uint32 overflow at the top of range
			whi = to
		}
		out = append(out, opsutil.RangeChunk{From: wlo, To: whi})
		if whi == to {
			break
		}
		wlo = whi + 1
	}
	return out
}

// windowCursorSub is this tool's per-window ingestion_cursors sub_source
// key: "<source>:<wlo>-<whi>". Self-describing and stable regardless of
// the overall run's -from/-to, so changing -window between runs only
// affects THAT run's own windows (old checkpoints for different
// boundaries simply go unmatched — harmless: ON CONFLICT DO NOTHING makes
// reprocessing a previously-covered sub-range safe, just not free).
func windowCursorSub(name string, w opsutil.RangeChunk) string {
	return fmt.Sprintf("%s:%d-%d", name, w.From, w.To)
}

// loadDoneWindows returns this source's already-checkpointed window
// sub_source keys mapped to their recorded last_ledger, reading
// ingestion_cursors ONCE (source=projected-rebuild) rather than one query
// per planned window — cheap even for a plan with hundreds of windows.
func loadDoneWindows(ctx context.Context, store *timescale.Store, name string) (map[string]uint32, error) {
	all, err := store.ListCursors(ctx)
	if err != nil {
		return nil, err
	}
	prefix := name + ":"
	out := make(map[string]uint32)
	for _, c := range all {
		if c.Source != projectedRebuildCursorSource || !strings.HasPrefix(c.Sub, prefix) {
			continue
		}
		out[c.Sub] = c.LastLedger
	}
	return out, nil
}

// pendingWindows filters plan down to the windows NOT already fully
// checkpointed in done (keyed by windowCursorSub). A window only counts
// as done when its recorded last_ledger is at or past its own upper
// bound — a partial/stale entry (shouldn't happen given windows are only
// checkpointed after their stream completes, but defensive) is retried
// rather than silently treated as complete.
func pendingWindows(plan []opsutil.RangeChunk, name string, done map[string]uint32) []opsutil.RangeChunk {
	if len(done) == 0 {
		return plan
	}
	out := make([]opsutil.RangeChunk, 0, len(plan))
	for _, w := range plan {
		if last, ok := done[windowCursorSub(name, w)]; ok && last >= w.To {
			continue
		}
		out = append(out, w)
	}
	return out
}

// windowScheduler hands out windows to concurrent workers off a SHARED
// atomic counter (task requirement: "each worker takes the next window
// off a shared counter") rather than a static per-worker split
// (ch-backfill's SplitRange) — so a worker that finishes a quiet window
// quickly immediately picks up the next pending one instead of idling
// while a sibling grinds through a dense stretch (aquarius rewards next
// to a quiet blend_backstop range is exactly this shape).
type windowScheduler struct {
	windows []opsutil.RangeChunk
	next    atomic.Int64
}

func newWindowScheduler(windows []opsutil.RangeChunk) *windowScheduler {
	return &windowScheduler{windows: windows}
}

// claim atomically reserves the next unclaimed window. Safe for
// concurrent use by any number of goroutines; each index 0..len-1 is
// returned to exactly one caller, and callers see ok=false once every
// window has been claimed.
func (s *windowScheduler) claim() (opsutil.RangeChunk, bool) {
	i := s.next.Add(1) - 1
	if i < 0 || int(i) >= len(s.windows) {
		return opsutil.RangeChunk{}, false
	}
	return s.windows[i], true
}

// decodeProjectedEvent runs one ClickHouse-lake event through src's
// decoder with the same Matches/Decode/panic-recover discipline as
// internal/projector.processEventSafely (this tool is the bulk-parallel
// twin of that function). A decode error is a data-level soft-fail —
// logged and counted, NOT propagated — because a deterministically
// malformed row would just re-fail identically on any retry (same policy
// the live projector applies). Kept as a small local twin here rather
// than exporting projector's unexported helper: the panic-recover wrapper
// is a few lines, not worth an exported cross-package contract for a
// single consumer.
func decodeProjectedEvent(name string, dec dispatcher.Decoder, ev events.Event, logger *slog.Logger) (outs []consumer.Event, softFail bool) {
	defer func() {
		if rec := recover(); rec != nil {
			outs, softFail = nil, true
			logger.Error("projected-rebuild: decode panicked; skipping row",
				"source", name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "panic", rec)
		}
	}()
	if !dec.Matches(ev) {
		return nil, false
	}
	o, derr := dec.Decode(ev)
	if derr != nil {
		return nil, true
	}
	return o, false
}

func writeModeLabel(write bool) string {
	if write {
		return "WRITE"
	}
	return "DRY-RUN (count only)"
}

// printProjectedRebuildSummary prints the completion report: per-topic
// emitted counts (task requirement, so an operator can eyeball against
// the census tables) plus the headline counters. ADR-0033
// compute-completeness remains the authoritative coverage verdict — this
// summary is an operator sanity check, not a substitute.
func printProjectedRebuildSummary(name string, from, to uint32, write bool, r ProjectedRebuildResult) {
	fmt.Printf("\n=== projected-rebuild %s [%d,%d] %s ===\n", name, from, to, writeModeLabel(write))
	fmt.Printf("windows: planned=%d skipped(resume)=%d processed=%d\n", r.WindowsPlanned, r.WindowsSkipped, r.WindowsProcessed)
	fmt.Printf("ledgers covered this run: %d\n", r.LedgersCovered)
	fmt.Printf("events read: %d   emitted: %d   decode errors: %d\n", r.EventsRead, r.EventsEmitted, r.DecodeErrors)
	if r.Elapsed > 0 {
		fmt.Printf("elapsed: %s   (%.1f ledgers/s, %.1f events/s)\n",
			r.Elapsed.Round(time.Second),
			float64(r.LedgersCovered)/r.Elapsed.Seconds(),
			float64(r.EventsEmitted)/r.Elapsed.Seconds())
	}
	if len(r.KindCounts) > 0 {
		fmt.Printf("\nper-topic emitted counts (cross-check against the census tables; ADR-0033 compute-completeness remains the authoritative verdict):\n")
		kinds := make([]string, 0, len(r.KindCounts))
		for k := range r.KindCounts {
			kinds = append(kinds, k)
		}
		sort.Slice(kinds, func(i, j int) bool { return r.KindCounts[kinds[i]] > r.KindCounts[kinds[j]] })
		for _, k := range kinds {
			fmt.Printf("  %-40s %12d\n", k, r.KindCounts[k])
		}
	}
	if !write {
		fmt.Println("\n(dry-run — re-run with -write to persist to Postgres)")
	}
}
