// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
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
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/version"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// projectedRebuild is the bulk catch-up path for projected sources: 10-20x projector-replay's rate; use replay under ~1M ledgers, this
// above (docs/operations/runbooks/projector.md#stellarindex_projector_replay_stalled). ONE WRITER: rows are identical to the live
// projector's and ON CONFLICT DO NOTHING keeps both idempotent, but it refuses a range the live cursor is inside (checkLiveCursorGuard)
// unless -allow-live-overlap. Per-window checkpoints make -resume exact.
func projectedRebuild(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse+validate, build the live decoder, the live-cursor guard, run, report — splitting scatters the guard rationale away from its call site.
	fs, gate := opsutil.NewMutatingFlagSet("projected-rebuild")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	sourceName := fs.String("source", "", "projector source name to rebuild (required); see internal/projector/registry.go for the list")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive); 0 = default to the live projector cursor's CURRENT position, i.e. fill exactly the history behind the live tail (ADR-0048 D3)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	window := fs.Uint("window", projectedRebuildDefaultWindow, "ledger-window size per checkpoint/scheduling unit — smaller gives finer resume granularity and better load balance across workers on uneven-density ranges (e.g. aquarius rewards); does NOT bound memory (this tool streams, never buffers a window)")
	workers := fs.Int("workers", projectedRebuildDefaultWorkers, "concurrent ledger-window workers; capped at 3 so workers x 2 ClickHouse queries stay inside the ops_batch limit of 8")
	resume := fs.Bool("resume", true, "skip windows already checkpointed by a prior -write run of THIS build for this source; another build's checkpoints are never trusted")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/last-exit gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_projected_rebuild_<source>.prom when that directory exists (r1), otherwise no heartbeat at all")
	refreshCAGGs := fs.Bool("refresh-caggs", true, "after a -write of a source that feeds trades or oracle_updates, re-materialise the price and volume aggregates over [from,to] (also after a partial or interrupted run); without it the served OHLC, VWAP, TWAP and volume keep the pre-rebuild figures. Leave on unless a wrapper refreshes afterwards")
	allowLiveOverlap := fs.Bool("allow-live-overlap", false, "DANGEROUS: bypass the live-cursor guard and run even though the live projector's cursor is inside [-from,-to]. Only pass this if you have independently verified the live projector will not process this range concurrently — see the ADR-0048 D3 one-writer contract in this command's doc comment.")
	onlyKindsFlag := fs.String("only-kinds", "", "comma list of decoder output kinds (EventKind(), e.g. aquarius.liquidity) to write; every other output is decoded, counted as skipped and NOT written. For a decoder fix that changes one output kind's stored shape; never for one that changes trades or cross-table derived state. Empty = every kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := gate.Enabled()
	if *cfgPath == "" || *sourceName == "" || *from == 0 {
		return fmt.Errorf("-config, -source, and -from are required")
	}
	// BackfillSafe gate, before the config load and long before the decoder is built: a refusal must not need a reachable
	// database, and it applies to the default dry-run too — see checkProjectedRebuildBackfillSafe.
	if gerr := checkProjectedRebuildBackfillSafe(*sourceName, uint32(*from)); gerr != nil { //nolint:gosec // ledger sequences fit uint32 in real usage.
		return gerr
	}
	onlyKinds, kerr := parseOnlyKinds(*sourceName, *onlyKindsFlag, flagPassed(fs, "only-kinds"))
	if kerr != nil {
		return kerr
	}

	windowSize := uint32(*window) //nolint:gosec // operator-supplied window size; zero guarded below.
	if windowSize == 0 {
		windowSize = projectedRebuildDefaultWindow
	}
	numWorkers := *workers
	if numWorkers < 1 {
		numWorkers = 1
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
	// Stamp a positive derive_generation so corrected values update stored rows in place via
	// pipeline.HandleEvent and win over live gen-0 values. Harmless in the dry run.
	store.SetDeriveGeneration(time.Now().Unix())

	// Every trade written wins the ON CONFLICT guard and assigns `usd_volume = EXCLUDED.usd_volume` unconditionally. Without
	// the USD-volume resolvers tradeUSDVolume returns nil for DEX and FX-priced CEX trades, so -write would overwrite correct
	// values with NULL. Install them as ch_rebuild.go does; even an aquarius rewards catch-up decodes trades in range.
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
		cfg.PricingGuard.FiatPeggedClassicAssets,
	); err != nil {
		return fmt.Errorf("install usd-volume resolution: %w", err)
	}

	logger := opsutil.MkBackfillLogger()

	// Build the same decoder the live projector uses. withHook=false keeps the gate warm read-only, like the other lake tools:
	// the bulk path must not mutate the contractid registry mid-run. A new factory child found in history converges on the next
	// `seed-protocol-contracts` run or indexer restart.
	gatedOpts, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		return fmt.Errorf("gated registry warm: %w", err)
	}
	// Soroswap's pair-registry persistence options are wired unconditionally to match the
	// indexer's production registry; inert for any other -source.
	soroswapOpts, err := pipeline.SoroswapPersistenceOptions(ctx, store, logger, ctx)
	if err != nil {
		return fmt.Errorf("soroswap persistence options: %w", err)
	}
	registry, err := projector.BuildRegistry([]string{*sourceName}, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts, gatedOpts, soroswapOpts...)
	if err != nil {
		return fmt.Errorf("build projector registry: %w", err)
	}
	src, err := selectProjectedSource(registry, *sourceName)
	if err != nil {
		return err
	}

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

	if gerr := gateProjectedRebuild(ctx, cfg, store, *sourceName, fromLedger, toLedger); gerr != nil {
		return gerr
	}

	if gerr := checkLiveCursorGuard(haveLive, liveCursor.LastLedger, toLedger, *allowLiveOverlap); gerr != nil {
		return gerr
	}

	fmt.Fprintf(os.Stderr, "projected-rebuild: source=%s range=[%d,%d] window=%d workers=%d mode=%s ch=%s\n",
		*sourceName, fromLedger, toLedger, windowSize, numWorkers, writeModeLabel(write), *chAddr)
	if onlyKinds != nil {
		fmt.Fprintf(os.Stderr, "projected-rebuild: only-kinds=%s derive_generation=%d\n", onlyKinds.label(), store.DeriveGeneration())
	}

	if write {
		needed, nerr := dirtyWindowNeeded(cfg, *sourceName, onlyKinds)
		if nerr != nil {
			return nerr
		}
		if derr := recordRebuildDirtyWindow(ctx, os.Stderr, store, needed, *sourceName, fromLedger, toLedger, onlyKinds); derr != nil {
			return derr
		}
	}

	hb := opsutil.NewJobHeartbeat("projected-rebuild-"+*sourceName, *heartbeat, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "projected-rebuild: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	result, runErr := RunProjectedRebuild(ctx, ProjectedRebuildOptions{
		Store:     store,
		ChAddr:    *chAddr,
		Source:    src,
		From:      fromLedger,
		To:        toLedger,
		Window:    windowSize,
		Workers:   numWorkers,
		Write:     write,
		Resume:    *resume,
		OnlyKinds: onlyKinds,
		Logger:    logger,
		Heartbeat: hb,
	})
	printProjectedRebuildSummary(*sourceName, fromLedger, toLedger, write, store.DeriveGeneration(), result)
	interrupted := ctx.Err() != nil
	// After the run, even a failed or interrupted one: any window it wrote is
	// below the fold checkpoint. A failed reset fails the job, interrupted or not.
	if rerr := resetSEP41RollupAfterRebuild(ctx, os.Stderr, store, *sourceName, write); rerr != nil {
		runErr, interrupted = errors.Join(runErr, rerr), false
	}
	runErr = errors.Join(runErr, sweepProjectedRebuildOracle(ctx, os.Stderr, store, *sourceName, fromLedger, toLedger, store.DeriveGeneration(), write, result, runErr, interrupted))
	// Any window it wrote is behind every refresh policy's lookback, so a partial or interrupted run refreshes too.
	if ferr := refreshAfterRebuild(ctx, os.Stderr,
		projectedRebuildRefresh(*cfgPath, *sourceName, write, *refreshCAGGs, fromLedger, toLedger, onlyKinds),
		func(rctx context.Context) error {
			return ingest.RefreshCAGGsForRange(rctx, logger, store, fromLedger, toLedger)
		}); ferr != nil {
		runErr, interrupted = errors.Join(runErr, ferr), false
	}
	return finishProjectedRebuild(hb, result, runErr, interrupted)
}

// projectedRebuildRefresh is the aggregate refresh a projected-rebuild of source owes over [from, to].
func projectedRebuildRefresh(cfgPath, source string, write, refresh bool, from, to uint32, only kindFilter) rebuildRefresh {
	r := rebuildRefresh{
		verb: "projected-rebuild", cfgPath: cfgPath, write: write, refresh: refresh,
		feeds: chRebuildFeedsAggregates(map[string]int{source: 1}), lo: from, hi: to,
	}
	if r.feeds && !onlyKindsFeedAggregates(source, only) {
		r.feeds = false
		r.skipped = fmt.Sprintf("aggregate refresh SKIPPED, nothing owed: -only-kinds %s writes no trades or oracle_updates row", only.label())
	}
	return r
}

// onlyKindsFeedAggregates reports whether an allow-list keeps a kind the aggregates are built on: a trade kind, or any kind
// of an oracle or unregistered source (both write oracle_updates or may).
func onlyKindsFeedAggregates(source string, only kindFilter) bool {
	if only == nil {
		return true
	}
	if c := external.Lookup(source).Class; c == external.ClassOracle || c == "" {
		return true
	}
	spec, ok := pipeline.SpecByName(source)
	if !ok {
		return true
	}
	for _, ev := range spec.Events {
		if _, trade := tradeOf(ev); trade && only.allows(ev.EventKind()) {
			return true
		}
	}
	return false
}

// kindFilter is the sorted, de-duplicated -only-kinds allow-list; nil allows every kind.
type kindFilter []string

func (f kindFilter) allows(kind string) bool { return f == nil || slices.Contains(f, kind) }

func (f kindFilter) label() string {
	if f == nil {
		return "all"
	}
	return strings.Join(f, ",")
}

// cursorSuffix keys a filtered run's checkpoints apart, so a window one filter completed is never skipped by a run
// with another filter or none.
func (f kindFilter) cursorSuffix() string {
	if f == nil {
		return ""
	}
	return "#only-kinds=" + strings.Join(f, ",")
}

// flagPassed reports whether name was set on the command line, so an explicitly empty value is told apart from absence.
func flagPassed(fs *flag.FlagSet, name string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) { passed = passed || f.Name == name })
	return passed
}

// parseOnlyKinds validates -only-kinds against the kinds the source's SourceSpec event types declare ([sourceKinds]). A
// kind outside that set is refused, and so is a flag passed empty: `-only-kinds "$UNSET"` must not become a full rewrite.
func parseOnlyKinds(source, raw string, passed bool) (kindFilter, error) {
	if strings.TrimSpace(raw) == "" {
		if passed {
			return nil, errors.New("projected-rebuild: -only-kinds was passed empty; omit the flag to rewrite every kind")
		}
		return nil, nil
	}
	known, ok := sourceKinds(source)
	if !ok {
		return nil, fmt.Errorf("projected-rebuild: -only-kinds: %q has no source spec to check kinds against", source)
	}
	var only kindFilter
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if !known[k] {
			return nil, fmt.Errorf("projected-rebuild: -only-kinds %q is not a kind source %s emits (known: %s)", k, source, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
		if !slices.Contains(only, k) {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only, nil
}

// sourceKinds is every kind the source's SourceSpec event types report: EventKinds() for a type whose kind depends on its
// value (aquarius.ReservesEvent), its zero value's EventKind() otherwise. A value-dependent kind a type does not declare is
// refused, which fails closed.
func sourceKinds(source string) (map[string]bool, bool) {
	spec, ok := pipeline.SpecByName(source)
	if !ok {
		return nil, false
	}
	known := map[string]bool{}
	for _, ev := range spec.Events {
		if multi, ok := ev.(interface{ EventKinds() []string }); ok {
			for _, k := range multi.EventKinds() {
				known[k] = true
			}
			continue
		}
		known[ev.EventKind()] = true
	}
	return known, true
}

// dirtyWindowNeeded decides the dirty window for a -write. Only a filtered run reads the catalogue, so an unfiltered one
// keeps its old path exactly: always recorded, with no catalogue build to fail or log.
func dirtyWindowNeeded(cfg config.Config, source string, only kindFilter) (bool, error) {
	if only == nil {
		return true, nil
	}
	scope, listed, err := reconcileScopeOf(cfg, source)
	if err != nil {
		return false, fmt.Errorf("reconciliation catalogue (needed to decide -only-kinds' dirty window): %w", err)
	}
	return rebuildNeedsDirtyWindow(scope, listed, only), nil
}

// rebuildNeedsDirtyWindow reports whether a -write owes a projection dirty window. An unfiltered run always does. A filtered
// one rewrites only its kinds' tables, so it does only when the reconcile counts one of them: a waived table
// (aquarius_liquidity, fan-out) is never reconciled, and a window over it would withdraw the claim on tables this run
// left untouched. A source the catalogue does not list, or one reconciled other than by kind, records it.
func rebuildNeedsDirtyWindow(scope reconcileScope, listed bool, only kindFilter) bool {
	if only == nil || !listed || scope.byOther {
		return true
	}
	return slices.ContainsFunc(scope.kinds, only.allows)
}

// recordRebuildDirtyWindow records the -write run's dirty window BEFORE the first write and refuses without it: a crash after
// the record leaves one spurious window, the reverse order leaves rewritten rows certified by stale evidence.
func recordRebuildDirtyWindow(ctx context.Context, w io.Writer, store *timescale.Store, needed bool, source string, from, to uint32, only kindFilter) error {
	if !needed {
		_, _ = fmt.Fprintf(w, "projected-rebuild: NO projection dirty window recorded — -only-kinds %s writes only tables the %s reconcile does not count, so the existing claim stands\n",
			only.label(), source)
		return nil
	}
	reason := timescale.ProjectedRebuildReason(from, to)
	if only != nil {
		reason += " only-kinds=" + only.label()
	}
	if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{Source: source, From: from, To: to, Reason: reason}); err != nil {
		return fmt.Errorf("record dirty window (refusing to write without it — the completeness verifier would carry a stale claim over the rewritten range): %w", err)
	}
	_, _ = fmt.Fprintf(w, "projected-rebuild: recorded projection dirty window [%d,%d] — compute-completeness will re-reconcile this range before carrying any projection claim over it\n",
		from, to)
	return nil
}

// sep41RollupResetter is the one store method resetSEP41RollupAfterRebuild needs.
type sep41RollupResetter interface {
	ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error)
}

// sep41RollupResetTimeout bounds the post-run reset, on a context detached from SIGINT so an interrupted run still
// resets what it wrote; it re-folds every watched contract's history, so it is sized for cold folds.
const sep41RollupResetTimeout = 10 * time.Minute

// resetSEP41RollupAfterRebuild fully resets the sep41_supply_rollup fold after a -write rebuild of sep41_supply:
// rewritten history sits at or below the checkpoint and the worker folds only `ledger > last_ledger`.
func resetSEP41RollupAfterRebuild(ctx context.Context, w io.Writer, store sep41RollupResetter, source string, write bool) error {
	if source != sep41supply.SourceName {
		return nil
	}
	if !write {
		_, _ = fmt.Fprintf(w, "projected-rebuild: dry-run: a -write run would then ResetSEP41SupplyRollupFold(nil) — the rebuilt range sits behind the sep41_supply_rollup fold checkpoint, and the fold only ever looks ABOVE it\n")
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sep41RollupResetTimeout)
	defer cancel()
	n, err := store.ResetSEP41SupplyRollupFold(rctx, nil)
	if err != nil {
		return fmt.Errorf("reset sep41_supply_rollup fold after rebuild (served SEP-41 supply misses every row this run wrote at or below the fold checkpoint until the fold is reset; re-run or call ResetSEP41SupplyRollupFold): %w", err)
	}
	_, _ = fmt.Fprintf(w, "projected-rebuild: reset %d sep41_supply_rollup fold row(s) [FULL], each re-folded from zero in place (genesis baseline preserved)\n", n)
	return nil
}

// finishProjectedRebuild records the verdict on the job heartbeat too: under run-heavy-job.sh nobody reads the exit code
// and stellarindex_ops_job_run_failed alerts on stellarindex_ops_job_last_exit_ok == 0. An interrupted run is not recorded clean.
func finishProjectedRebuild(hb *opsutil.JobHeartbeat, r ProjectedRebuildResult, runErr error, interrupted bool) error {
	err := projectedRebuildOutcome(r, runErr, interrupted)
	hb.Stop(err == nil && runErr == nil)
	return err
}

// errProjectedRebuildIncomplete marks a run that finished without a fatal
// error but did not land every row.
var errProjectedRebuildIncomplete = errors.New("projected-rebuild: run did not land every row")

// projectedRebuildOutcome maps a finished run to the exit status: a caller reading only the exit code must not see
// success for a run that lost rows.
func projectedRebuildOutcome(r ProjectedRebuildResult, runErr error, interrupted bool) error {
	if runErr != nil {
		if interrupted {
			// SIGINT/SIGTERM mid-run — not a failure; the completed
			// windows are checkpointed (if -write) and a re-run with
			// -resume picks up the rest.
			fmt.Fprintf(os.Stderr, "projected-rebuild: interrupted — re-run with -resume to continue from the last completed window\n")
			return nil
		}
		return fmt.Errorf("projected-rebuild: %w", runErr)
	}
	if r.WindowsHeld > 0 || r.PermanentDrops > 0 {
		return fmt.Errorf("%w: %d window(s) held after %d insert error(s), %d trade(s) permanently dropped — see the summary above",
			errProjectedRebuildIncomplete, r.WindowsHeld, r.InsertErrors, r.PermanentDrops)
	}
	// Only a walk of the whole plan proves a kind absent; a resumed run may have written it in an earlier invocation.
	if unseen := r.unseenKinds(); len(unseen) > 0 && r.WindowsPlanned > 0 && r.WindowsProcessed == r.WindowsPlanned {
		return fmt.Errorf("%w: -only-kinds %s matched no decoder output over the whole range (misspelled, or not this source's kind)",
			errProjectedRebuildKindUnseen, strings.Join(unseen, ","))
	}
	return nil
}

// errProjectedRebuildKindUnseen marks a filtered run that wrote nothing for a named kind.
var errProjectedRebuildKindUnseen = errors.New("projected-rebuild: a named kind was never emitted")

const (
	// projectedRebuildCursorSource is the ingestion_cursors "source" for this tool's per-window checkpoints, distinct from
	// "projector" (the live tail's cursor, never touched here).
	projectedRebuildCursorSource = "projected-rebuild"

	// projectedRebuildDefaultWindow: the tool streams and never buffers a window, so size sets only checkpoint and
	// scheduling granularity; smaller windows give more resume points and spread better when density is uneven.
	projectedRebuildDefaultWindow = 50_000

	// projectedRebuildDefaultWorkers / Max: see RunProjectedRebuild's
	// PG-pool-sizing doc note.
	projectedRebuildDefaultWorkers = 3

	// Each worker holds two ClickHouse queries (event stream + nested tx_index lookup); the ops_batch profile admits 8 per user
	// (clickhouse_ops_batch_max_concurrent_queries) and one slot stays free so a stray read cannot trip code 202.
	projectedRebuildQueriesPerWorker = 2
	projectedRebuildQueryBudget      = 8
	projectedRebuildMaxWorkers       = (projectedRebuildQueryBudget - 1) / projectedRebuildQueriesPerWorker

	// projectedRebuildProgressInterval is the default periodic
	// progress-log cadence.
	projectedRebuildProgressInterval = 15 * time.Second
)

// gateProjectedRebuild is the per-WASM lake gate over the rebuild's range.
func gateProjectedRebuild(ctx context.Context, cfg config.Config, store *timescale.Store, source string, from, to uint32) error {
	return wasmaudit.GateReplay(ctx, cfg.Storage.ClickHouseAddr, cfg.Oracle, store.LoadProtocolContracts, []string{source}, from, to)
}

// checkProjectedRebuildBackfillSafe refuses a source whose decoder was not audited against every WASM generation in its history
// ([external.ReplayBackfillSafe]; unregistered is refused). Armed in dry-run too, unlike ch-rebuild: a multi-hour preview of a refused run
// is a plan nobody can execute. No override.
func checkProjectedRebuildBackfillSafe(source string, from uint32) error {
	if external.ReplayBackfillSafe(source) {
		return nil
	}
	return fmt.Errorf(
		"projected-rebuild: refusing to rebuild source %q from ledger %d — it is not BackfillSafe (per-WASM-hash audit "+
			"pending, or not a known source): this tool decodes history with the CURRENT decoder and its rows overwrite "+
			"the stored ones, and Soroban contracts upgrade in place, so an unaudited old WASM generation decodes to "+
			"silently wrong rows. Run stellarindex-ops wasm-history -from %d -to <tip> -contracts <CID> for the source's "+
			"contracts, review every emitted WASM hash against the current decoder, then audit each WASM hash under docs/operations/wasm-audits/, add it to internal/wasmaudit/audited_wasm.json and set Backfill: BackfillPerWASM in internal/sources/external/registry.go in the same PR (see docs/architecture/domain-traps.md, \"Soroban DeFi contracts upgrade in place\"). The dry-run is "+
			"gated too; to evaluate an unaudited decoder against history use ch-rebuild's default dry-run. If the name "+
			"is simply wrong: projector SOURCE names are underscored (blend_backstop, sep41_transfers); see "+
			"internal/projector/registry.go",
		source, from, from)
}

// liveCursorCovers is the ADR-0048 D3 one-writer predicate: the live projector has already walked
// the whole range. It is shared with checkCHRebuildLiveOverlap so the tools agree on safe overlap.
func liveCursorCovers(haveLive bool, liveLastLedger, to uint32) bool {
	return haveLive && liveLastLedger >= to
}

// checkLiveCursorGuard is the one-writer contract as a pure function: errors unless the live cursor is at or above `to`; allowOverlap
// overrides. haveLive=false (projector never ran for this source) counts as cursor 0 and is refused, since it WILL eventually walk this
// range.
func checkLiveCursorGuard(haveLive bool, liveLastLedger, to uint32, allowOverlap bool) error {
	if allowOverlap {
		return nil
	}
	if liveCursorCovers(haveLive, liveLastLedger, to) {
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

// ProjectedRebuildOptions bundles projected-rebuild's resolved dependencies apart from the flag parser, so the executor is callable from tests without a config file or live projector.
type ProjectedRebuildOptions struct {
	Store  *timescale.Store
	ChAddr string
	// Source is the one projector source to rebuild, with the live registry's decoder and
	// prefilter. Build it via projector.BuildRegistry so both paths decode identically (ADR-0048 D3).
	Source projector.Source
	From   uint32
	To     uint32
	// Window is the ledger span per checkpoint/scheduling unit. 0 uses
	// projectedRebuildDefaultWindow.
	Window uint32
	// Workers is the number of concurrent window-claiming goroutines.
	// Values < 1 are treated as 1.
	//
	// Every worker's HandleEvent draws a connection from the one Store pool per row insert
	// (timescale.PoolMaxOpenConns = 25), so projectedRebuildMaxWorkers is capped to fit it.
	Workers int
	// Write persists decoded events via pipeline.HandleEvent AND records a per-window checkpoint on success. false = dry-run:
	// counts and reports only — no writes, no checkpoints (mirrors classic-movements-backfill's -write gate).
	Write bool
	// Resume skips windows already checkpointed by a prior -write run for
	// this source. Consulted even in dry-run mode, so a dry-run preview
	// after a partial -write run reflects the real remaining work.
	Resume bool
	// OnlyKinds restricts the sink to these output kinds; nil writes every kind. It is part of the checkpoint key.
	OnlyKinds kindFilter
	Logger    *slog.Logger
	// ProgressInterval overrides the periodic progress-log cadence; <= 0
	// uses projectedRebuildProgressInterval.
	ProgressInterval time.Duration
	// Heartbeat receives covered-ledger progress each progress tick, so
	// stellarindex_ops_job_no_progress sees a healthy run move. Nil is inert.
	Heartbeat *opsutil.JobHeartbeat
}

// applyProjectedEvent decodes one lake event and writes its outputs, returning rows emitted and insert failures; failures a re-run can land
// mean the caller must not checkpoint ([checkpointWindow]). A *[pipeline.TradeDroppedError] is counted apart and does not hold the window
// (fails identically on re-run).
func applyProjectedEvent(
	ctx context.Context,
	opts ProjectedRebuildOptions,
	logger *slog.Logger,
	counters *projectedRebuildCounters,
	ev events.Event,
) (emitted, insertErrs int64) {
	outs, softFail := decodeProjectedEvent(opts.Source.Name, opts.Source.Decoder, ev, logger)
	if softFail {
		counters.decodeErrors.Add(1)
		return 0, 0
	}
	return writeProjectedOutputs(ctx, opts, logger, counters, outs)
}

// writeProjectedOutputs counts and (in -write mode) persists decoded outputs;
// see [applyProjectedEvent] for the returned counts.
func writeProjectedOutputs(
	ctx context.Context,
	opts ProjectedRebuildOptions,
	logger *slog.Logger,
	counters *projectedRebuildCounters,
	outs []consumer.Event,
) (emitted, insertErrs int64) {
	for _, out := range outs {
		emitted++
		if !opts.OnlyKinds.allows(out.EventKind()) {
			counters.addSkippedKind(out.EventKind())
			continue
		}
		counters.addKind(out.EventKind())
		if !opts.Write {
			continue
		}
		err := pipeline.HandleEvent(ctx, logger, opts.Store, out)
		if err == nil {
			continue
		}
		counters.addFailedKind(out.EventKind())
		var dropped *pipeline.TradeDroppedError
		if errors.As(err, &dropped) {
			// Counted, never held — see the godoc. The pipeline already
			// logged the store's error at ERROR; this line adds the fact an
			// operator of THIS tool needs: no re-run will land it.
			counters.permanentDrops.Add(1)
			logger.Error("projected-rebuild: trade PERMANENTLY rejected by the store — counted, window still checkpoints; "+
				"a re-run cannot land it (fix the defect, then re-run the range with -resume=false)",
				"source", dropped.Source, "ledger", dropped.Ledger, "tx_hash", dropped.TxHash,
				"op_index", dropped.OpIndex, "err", dropped.Cause)
			continue
		}
		insertErrs++
	}
	return emitted, insertErrs
}

// checkpointWindow records a completed window, or deliberately not when rows were lost: -resume skips checkpointed windows, so
// checkpointing would lose the row for good. insertErrs excludes rejected trades; re-running is the only unattended recovery (completeness
// registers only `trades` for some sources).
func checkpointWindow(
	ctx context.Context,
	opts ProjectedRebuildOptions,
	logger *slog.Logger,
	counters *projectedRebuildCounters,
	w opsutil.RangeChunk,
	insertErrs int64,
) {
	sub := windowCursorSub(opts.Source.Name, w, opts.OnlyKinds)
	if insertErrs > 0 {
		counters.insertErrors.Add(insertErrs)
		counters.windowsHeld.Add(1)
		logger.Error("projected-rebuild: window NOT checkpointed — inserts failed, "+
			"re-run to retry it (writes are idempotent)",
			"window_sub", sub, "from", w.From, "to", w.To,
			"insert_errors", insertErrs)
		return
	}
	if cerr := opts.Store.UpsertCursor(ctx, projectedRebuildCursorSource, sub, w.To); cerr != nil {
		logger.Warn("projected-rebuild: checkpoint failed", "window_sub", sub, "err", cerr)
	}
}

// ProjectedRebuildResult summarises one RunProjectedRebuild call.
type ProjectedRebuildResult struct {
	WindowsPlanned int
	WindowsSkipped int // already checkpointed by a prior -write run of this build (resume)
	// WindowsOtherBuild counts pending windows a different build had checkpointed: re-walked, never skipped.
	WindowsOtherBuild int
	WindowsProcessed  int
	LedgersCovered    int64 // sum of processed windows' ledger spans, THIS run
	EventsRead        int64
	EventsEmitted     int64
	DecodeErrors      int64
	// InsertErrors counts HandleEvent failures a re-run can still land (see
	// [applyProjectedEvent]) and WindowsHeld the windows left un-checkpointed because of them.
	// Non-zero means the range is not covered; re-run, as writes are idempotent.
	InsertErrors int64
	WindowsHeld  int64
	// PermanentDrops counts trades the store permanently rejected (*pipeline.TradeDroppedError).
	// They hold no window and a re-run cannot land them: rows stay missing from the served tier
	// until the defect is fixed and the range re-run with -resume=false.
	PermanentDrops int64
	// EvictedOrphans is the stateful decoder's count of correlation-buffer
	// entries aged out without their partner event (e.g. a Soroswap SwapEvent
	// whose SyncEvent never arrived) — trades lost before reaching the store.
	EvictedOrphans int64
	// KindCounts is emitted-event count by consumer.Event.EventKind() — the "per-topic emitted counts" report so an operator
	// can eyeball against the census tables. ADR-0033 compute-completeness remains the authoritative verdict.
	KindCounts map[string]int64
	// KindsSkipped counts outputs by kind that -only-kinds kept out of the sink; OnlyKinds is the filter (nil = none).
	KindsSkipped map[string]int64
	// KindsFailed counts outputs by kind the sink refused (insert errors and permanent drops); KindCounts includes them.
	KindsFailed map[string]int64
	OnlyKinds   kindFilter
	// UnlayeredPools names gated aquarius pools in neither in-code layout list ([unlayeredAquariusPools]).
	UnlayeredPools []string
	Elapsed        time.Duration
}

// unlayeredAquariusPools names the gated aquarius pools in neither in-code layout list. protocol_contracts does not carry a
// layout, so such a pool's deposits decode only if this run saw its add_pool first: windows run out of order across
// workers, so its liquidity can be refused in one run and written in another. Empty for any other source.
func unlayeredAquariusPools(src projector.Source) []string {
	if src.Name != aquarius.SourceName {
		return nil
	}
	gated, ok := src.Decoder.(interface{ GatedContractSet() []string })
	if !ok {
		return nil
	}
	pools := slices.DeleteFunc(gated.GatedContractSet(), func(id string) bool { return id == aquarius.MainnetRouter })
	out := aquarius.PoolsWithoutLayout(pools)
	sort.Strings(out)
	return out
}

func unlayeredPoolLines(pools []string) []string {
	if len(pools) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("!! %d aquarius pool(s) have no in-code liquidity layout: %s", len(pools), strings.Join(pools, ",")),
		"!! their deposit/withdraw decode depended on window order; add each to aquarius.MainnetPools or",
		"!! MainnetConcentratedPools by its add_pool type and re-run, or compare against a -workers 1 run.",
	}
}

func sumKindCounts(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// unseenKinds lists the -only-kinds entries no output of this run carried.
func (r ProjectedRebuildResult) unseenKinds() []string {
	var out []string
	for _, k := range r.OnlyKinds {
		if r.KindCounts[k] == 0 {
			out = append(out, k)
		}
	}
	return out
}

// RunProjectedRebuild streams each planned window unbuffered through the decoder into pipeline.HandleEvent with Workers goroutines, skipping done windows under Resume.
// Only completed windows checkpoint; decode errors and permanently rejected trades are counted soft-fails, any other insert failure holds the window.
func RunProjectedRebuild(ctx context.Context, opts ProjectedRebuildOptions) (ProjectedRebuildResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	windowSize := opts.Window
	if windowSize == 0 {
		windowSize = projectedRebuildDefaultWindow
	}
	numWorkers := capProjectedRebuildWorkers(opts.Workers)
	if opts.Workers > numWorkers {
		logger.Warn("projected-rebuild: workers lowered to stay inside the ClickHouse ops_batch query budget",
			"requested_workers", opts.Workers, "workers", numWorkers, "max_concurrent_queries", projectedRebuildQueryBudget)
	}
	// A correlation-buffer decoder (the EvictedOrphans interface) emits a group only when a later event completes or sweeps it.
	// Workers claiming windows out of ledger order starve those triggers and silently drop groups (phoenix 7-field era:
	// workers=4 lost ~650 of 5,154 trades, workers=1 none), so clamp rather than trust the operator.
	if numWorkers > 1 {
		if _, stateful := opts.Source.Decoder.(interface{ EvictedOrphans() int }); stateful {
			logger.Warn("projected-rebuild: workers clamped to 1 — this source's decoder correlates events across the stream; out-of-order windows silently drop sweep-emitted groups",
				"source", opts.Source.Name, "requested_workers", numWorkers)
			numWorkers = 1
		}
	}

	plan := buildWindowPlan(opts.From, opts.To, windowSize)
	result := ProjectedRebuildResult{WindowsPlanned: len(plan), KindCounts: map[string]int64{}, OnlyKinds: opts.OnlyKinds}
	if len(plan) == 0 {
		return result, nil
	}

	pending, otherBuild, err := resumePending(ctx, opts, plan)
	if err != nil {
		return result, err
	}
	result.WindowsOtherBuild = otherBuild
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
			start, len(pending), pendingLedgers, counters, opts.Heartbeat)
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
	if runErr == nil {
		// Groups still open at the range end have no later event to flush them.
		emitted, insertErrs := writeProjectedOutputs(ctx, opts, logger, counters, dispatcher.Drain(opts.Source.Decoder))
		counters.eventsEmitted.Add(emitted)
		counters.insertErrors.Add(insertErrs)
	}

	fillProjectedRebuildResult(&result, counters, opts.Source, logger)
	result.Elapsed = time.Since(start)
	return result, runErr
}

// fillProjectedRebuildResult copies a finished run's counters into its result; every worker must have exited.
func fillProjectedRebuildResult(result *ProjectedRebuildResult, counters *projectedRebuildCounters, src projector.Source, logger *slog.Logger) {
	result.WindowsProcessed = int(counters.windowsDone.Load())
	result.LedgersCovered = counters.completedLedgers.Load()
	result.EventsRead = counters.eventsRead.Load()
	result.EventsEmitted = counters.eventsEmitted.Load()
	result.DecodeErrors = counters.decodeErrors.Load()
	result.InsertErrors = counters.insertErrors.Load()
	result.WindowsHeld = counters.windowsHeld.Load()
	result.PermanentDrops = counters.permanentDrops.Load()
	result.EvictedOrphans = collectEvictedOrphans(src, logger)
	result.KindCounts = counters.kindCounts
	result.KindsSkipped = counters.skippedKinds
	result.KindsFailed = counters.failedKinds
	result.UnlayeredPools = unlayeredAquariusPools(src)
}

// capProjectedRebuildWorkers clamps n to [1, projectedRebuildMaxWorkers].
func capProjectedRebuildWorkers(n int) int {
	return max(1, min(n, projectedRebuildMaxWorkers))
}

// selectProjectedSource returns the registry entry named by -source; BuildRegistry always appends the sep41 sources
// when contracts are watched, so a non-projected name must not fall through to the first entry.
func selectProjectedSource(registry projector.Registry, name string) (projector.Source, error) {
	want := strings.TrimSpace(name)
	for _, s := range registry.Sources {
		if strings.EqualFold(s.Name, want) {
			return s, nil
		}
	}
	return projector.Source{}, fmt.Errorf("projected-rebuild: %q is not a projector source (see internal/projector/registry.go) — non-projected sources (sdex, band, soroswap-router, external CEX/FX) have their own catch-up paths, and a sep41 source name also resolves empty here when no contracts are watched (cfg.Supply.WatchedSEP41Contracts)", name)
}

// collectEvictedOrphans must run after every worker has exited; the decoder
// is built fresh per invocation, so the count is this run's alone.
func collectEvictedOrphans(src projector.Source, logger *slog.Logger) int64 {
	reporter, ok := src.Decoder.(interface{ EvictedOrphans() int })
	if !ok {
		return 0
	}
	n := int64(reporter.EvictedOrphans())
	if n > 0 {
		logger.Warn("projected-rebuild: decoder evicted orphaned correlation entries — those trades were never emitted",
			"source", src.Name, "evicted_orphans", n)
	}
	return n
}

// projectedRebuildCounters bundles the atomic progress counters shared by RunProjectedRebuild's worker pool and
// progress-log goroutine.
type projectedRebuildCounters struct {
	windowsDone      atomic.Int64
	completedLedgers atomic.Int64
	eventsRead       atomic.Int64
	eventsEmitted    atomic.Int64
	decodeErrors     atomic.Int64
	// insertErrors counts the HandleEvent failures a re-run can still land.
	// Non-zero means at least one window was deliberately NOT checkpointed,
	// so a resumed run will redo it — see runProjectedRebuildWorker.
	insertErrors atomic.Int64
	windowsHeld  atomic.Int64
	// permanentDrops counts trades the store permanently rejected.
	// Counted, never held — see applyProjectedEvent.
	permanentDrops atomic.Int64
	kindMu         sync.Mutex
	kindCounts     map[string]int64
	skippedKinds   map[string]int64
	failedKinds    map[string]int64
}

func newProjectedRebuildCounters() *projectedRebuildCounters {
	return &projectedRebuildCounters{kindCounts: map[string]int64{}, skippedKinds: map[string]int64{}, failedKinds: map[string]int64{}}
}

func (c *projectedRebuildCounters) addFailedKind(k string) {
	c.kindMu.Lock()
	c.failedKinds[k]++
	c.kindMu.Unlock()
}

func (c *projectedRebuildCounters) addKind(k string) {
	c.kindMu.Lock()
	c.kindCounts[k]++
	c.kindMu.Unlock()
}

func (c *projectedRebuildCounters) addSkippedKind(k string) {
	c.kindMu.Lock()
	c.skippedKinds[k]++
	c.kindMu.Unlock()
}

// runProjectedRebuildWorker claims windows off sched, streams each through opts.Source's decoder and, under -write, sinks via
// pipeline.HandleEvent and checkpoints. It returns the first fatal stream error; a decode error is a soft-fail (decodeProjectedEvent).
func runProjectedRebuildWorker(ctx context.Context, sched *windowScheduler, opts ProjectedRebuildOptions, logger *slog.Logger, counters *projectedRebuildCounters) error {
	for {
		w, ok := sched.claim()
		if !ok {
			return nil
		}
		var windowRead, windowEmitted, windowInsertErrs int64
		werr := clickhouse.StreamContractEventsFiltered(ctx, opts.ChAddr, w.From, w.To,
			opts.Source.PrefilterContractIDs(), opts.Source.Topic0Syms, opts.Source.ExcludeTopic0Syms,
			false,                           // no FINAL: idempotent downstream writes absorb any duplicate (matches the live projector's CH feed-switch mode)
			true,                            // withOpArgs: mirrors the live projector, which routes every source uniformly (redstone needs it; the window is bounded so the wide column is cheap)
			opts.Source.NeedsStateWriteKeys, // mirrors the live projector: redstone's exact subset attribution reads written contract-data keys
			opts.Source.NeedsTxIndex,        // mirrors the live projector: blnt persists each event's apply position
			func(ev events.Event) error {
				windowRead++
				emitted, insertErrs := applyProjectedEvent(ctx, opts, logger, counters, ev)
				windowEmitted += emitted
				windowInsertErrs += insertErrs
				return nil
			})
		counters.eventsRead.Add(windowRead)
		counters.eventsEmitted.Add(windowEmitted)
		if werr != nil {
			return fmt.Errorf("window [%d,%d]: %w", w.From, w.To, werr)
		}
		if opts.Write {
			checkpointWindow(ctx, opts, logger, counters, w, windowInsertErrs)
		}
		counters.completedLedgers.Add(int64(w.To-w.From) + 1)
		counters.windowsDone.Add(1)
	}
}

// runProjectedRebuildProgressLoop prints one progress line per interval (default projectedRebuildProgressInterval)
// until progressCtx is done.
func runProjectedRebuildProgressLoop(
	progressCtx context.Context, logger *slog.Logger, sourceName string, interval time.Duration,
	start time.Time, totalWindows int, pendingLedgers int64,
	counters *projectedRebuildCounters, hb *opsutil.JobHeartbeat,
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
			hb.Progress(uint64(done), 0)
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

// buildWindowPlan divides [from,to] into contiguous, non-overlapping windows of at most `window` ledgers, up front
// since dynamic scheduling needs the full set; nil for an empty or invalid range.
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

// windowCursorSub is the per-window ingestion_cursors sub_source key "<source>:<wlo>-<whi>@<build>[#only-kinds=<a,b>]",
// independent of -from/-to. The build is part of the key so a resume trusts only windows THIS build wrote: a re-derive after
// a decoder fix runs a new build, matches nothing and re-walks every window instead of skipping the history it was run to
// correct. The sorted filter is part of it for the same reason: a filtered window did not write the other kinds.
func windowCursorSub(name string, w opsutil.RangeChunk, only kindFilter) string {
	return windowCursorPrefix(name, w) + version.Version + only.cursorSuffix()
}

func windowCursorPrefix(name string, w opsutil.RangeChunk) string {
	return fmt.Sprintf("%s:%d-%d@", name, w.From, w.To)
}

// otherBuildWindows counts the planned windows another build or -only-kinds filter checkpointed in full (or a build that
// predates the build in the key): they are re-walked, and the summary says so.
func otherBuildWindows(plan []opsutil.RangeChunk, name string, only kindFilter, done map[string]uint32) int {
	n := 0
	for _, w := range plan {
		prefix := windowCursorPrefix(name, w)
		own := windowCursorSub(name, w, only)
		for sub, last := range done {
			if sub != own && last >= w.To && (strings.HasPrefix(sub, prefix) || sub+"@" == prefix) {
				n++
				break
			}
		}
	}
	return n
}

// loadDoneWindows returns this source's checkpointed window sub_source keys mapped to last_ledger, reading
// ingestion_cursors (source=projected-rebuild) ONCE rather than one query per planned window.
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

// resumePending returns the windows still to walk and how many of them another build had checkpointed.
func resumePending(ctx context.Context, opts ProjectedRebuildOptions, plan []opsutil.RangeChunk) ([]opsutil.RangeChunk, int, error) {
	if !opts.Resume {
		return plan, 0, nil
	}
	done, err := loadDoneWindows(ctx, opts.Store, opts.Source.Name)
	if err != nil {
		return nil, 0, fmt.Errorf("projected-rebuild: load resume checkpoints: %w", err)
	}
	pending := pendingWindows(plan, opts.Source.Name, opts.OnlyKinds, done)
	return pending, otherBuildWindows(pending, opts.Source.Name, opts.OnlyKinds, done), nil
}

// pendingWindows filters plan to windows not fully checkpointed in done. A window is done only
// when its last_ledger reaches its upper bound; a partial or stale entry is retried, not trusted.
func pendingWindows(plan []opsutil.RangeChunk, name string, only kindFilter, done map[string]uint32) []opsutil.RangeChunk {
	if len(done) == 0 {
		return plan
	}
	out := make([]opsutil.RangeChunk, 0, len(plan))
	for _, w := range plan {
		if last, ok := done[windowCursorSub(name, w, only)]; ok && last >= w.To {
			continue
		}
		out = append(out, w)
	}
	return out
}

// windowScheduler hands windows to workers off a shared atomic counter rather than a static split,
// so a worker done with a quiet window never idles behind a sibling on a dense stretch.
type windowScheduler struct {
	windows []opsutil.RangeChunk
	next    atomic.Int64
}

func newWindowScheduler(windows []opsutil.RangeChunk) *windowScheduler {
	return &windowScheduler{windows: windows}
}

// claim atomically reserves the next unclaimed window; safe for concurrent use, each window goes to exactly one
// caller, and ok=false once all are claimed.
func (s *windowScheduler) claim() (opsutil.RangeChunk, bool) {
	i := s.next.Add(1) - 1
	if i < 0 || int(i) >= len(s.windows) {
		return opsutil.RangeChunk{}, false
	}
	return s.windows[i], true
}

// decodeProjectedEvent runs one lake event through src's decoder via [dispatcher.DecodeRow] with the live projector's
// Matches/Decode/panic-recover discipline. A decode error or panic is a soft-fail the caller counts, as a malformed row re-fails
// identically.
func decodeProjectedEvent(name string, dec dispatcher.Decoder, ev events.Event, logger *slog.Logger) (outs []consumer.Event, softFail bool) {
	o, matched, derr := dispatcher.DecodeRow(name, dec, ev, logger)
	if !matched {
		return nil, false
	}
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

// projectedRebuildLossLines renders the two ways a -write run can finish without landing every row, kept apart because the operator's next
// step is OPPOSITE: held windows (a re-run retries; compute-completeness cannot see non-trade tables) vs permanent drops (a re-run cannot
// land them; windows are checkpointed).
func projectedRebuildLossLines(r ProjectedRebuildResult) []string {
	var lines []string
	if r.WindowsHeld > 0 {
		lines = append(lines,
			fmt.Sprintf("\n!! %d window(s) NOT checkpointed — %d insert(s) failed and those rows were LOST.",
				r.WindowsHeld, r.InsertErrors),
			"!! This run did NOT cover its full range. RE-RUN the same command to retry the held",
			"!! windows (writes are idempotent; -resume will skip the windows that did succeed).",
			"!! A window that re-fails identically on every re-run is a deterministic store rejection:",
			"!! read the insert error logged above and fix that — another re-run will not clear it.")
	}
	if r.PermanentDrops > 0 {
		lines = append(lines,
			fmt.Sprintf("\n!! %d trade(s) PERMANENTLY rejected by the store — those rows are NOT in the served tier.",
				r.PermanentDrops),
			"!! Their windows ARE checkpointed: the rejection is deterministic, so a re-run CANNOT land them.",
			"!! Each is logged above with its ledger/tx/op and the store's error. Fix the defect, then",
			"!! re-run the affected range with -resume=false.")
	}
	if r.EvictedOrphans > 0 {
		lines = append(lines,
			fmt.Sprintf("\n!! %d orphaned event(s) EVICTED by the decoder — their partner event never arrived within the correlation window,",
				r.EvictedOrphans),
			"!! so those trades were never emitted and are NOT in the served tier.")
	}
	return lines
}

// projectedRebuildKindLines renders the per-kind table: emitted counts, or under -only-kinds written (would-write in a dry
// run) beside skipped-by-filter, plus any named kind the run never saw.
func projectedRebuildKindLines(r ProjectedRebuildResult, write bool) []string {
	if len(r.KindCounts) == 0 && len(r.KindsSkipped) == 0 && r.OnlyKinds == nil {
		return nil
	}
	if r.OnlyKinds == nil {
		lines := []string{"\nper-topic emitted counts (cross-check against the census tables; ADR-0033 compute-completeness remains the authoritative verdict):"}
		for _, k := range kindsByCount(r.KindCounts) {
			lines = append(lines, fmt.Sprintf("  %-40s %12d", k, r.KindCounts[k]))
		}
		return lines
	}
	lines := []string{fmt.Sprintf("\nper-kind event counts under -only-kinds %s (events, not rows; skipped = decoded but kept out of the sink):", r.OnlyKinds.label())}
	if write {
		lines = append(lines, fmt.Sprintf("  %-40s %12s %12s %12s", "kind", "written", "failed", "skipped"))
	} else {
		lines = append(lines, fmt.Sprintf("  %-40s %12s %12s", "kind", "would-write", "skipped"))
	}
	all := map[string]int64{}
	for k, n := range r.KindCounts {
		all[k] += n
	}
	for k, n := range r.KindsSkipped {
		all[k] += n
	}
	for _, k := range kindsByCount(all) {
		if write {
			lines = append(lines, fmt.Sprintf("  %-40s %12d %12d %12d", k, r.KindCounts[k]-r.KindsFailed[k], r.KindsFailed[k], r.KindsSkipped[k]))
		} else {
			lines = append(lines, fmt.Sprintf("  %-40s %12d %12d", k, r.KindCounts[k], r.KindsSkipped[k]))
		}
	}
	if unseen := r.unseenKinds(); len(unseen) > 0 {
		lines = append(lines, fmt.Sprintf("!! -only-kinds %s: no output of this run carried it", strings.Join(unseen, ",")))
	}
	return lines
}

// kindsByCount orders kinds by descending count, then name, so the table is stable.
func kindsByCount(m map[string]int64) []string {
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if m[kinds[i]] != m[kinds[j]] {
			return m[kinds[i]] > m[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	return kinds
}

// printProjectedRebuildSummary prints per-topic emitted counts and the headline counters. It is a
// sanity check; compute-completeness (ADR-0033) stays the authoritative coverage verdict.
func printProjectedRebuildSummary(name string, from, to uint32, write bool, generation int64, r ProjectedRebuildResult) {
	fmt.Printf("\n=== projected-rebuild %s [%d,%d] %s ===\n", name, from, to, writeModeLabel(write))
	if write {
		fmt.Printf("derive_generation: %d (every row this run wrote carries it)\n", generation)
	}
	fmt.Printf("windows: planned=%d skipped(resume)=%d processed=%d\n", r.WindowsPlanned, r.WindowsSkipped, r.WindowsProcessed)
	if r.WindowsOtherBuild > 0 && r.OnlyKinds == nil {
		fmt.Printf("windows: %d were checkpointed by another build and were not skipped (resume trusts only build %s)\n",
			r.WindowsOtherBuild, version.Version)
	} else if r.WindowsOtherBuild > 0 {
		fmt.Printf("windows: %d were checkpointed by another build or -only-kinds filter and were not skipped (resume trusts only build %s, only-kinds=%s)\n",
			r.WindowsOtherBuild, version.Version, r.OnlyKinds.label())
	}
	fmt.Printf("ledgers covered this run: %d\n", r.LedgersCovered)
	if r.OnlyKinds == nil {
		fmt.Printf("events read: %d   emitted: %d   decode errors: %d\n", r.EventsRead, r.EventsEmitted, r.DecodeErrors)
	} else {
		fmt.Printf("events read: %d   decoder outputs: %d (skipped by -only-kinds: %d)   decode errors: %d\n",
			r.EventsRead, r.EventsEmitted, sumKindCounts(r.KindsSkipped), r.DecodeErrors)
	}
	for _, line := range unlayeredPoolLines(r.UnlayeredPools) {
		fmt.Println(line)
	}
	for _, line := range projectedRebuildLossLines(r) {
		fmt.Println(line)
	}
	if r.Elapsed > 0 {
		fmt.Printf("elapsed: %s   (%.1f ledgers/s, %.1f events/s)\n",
			r.Elapsed.Round(time.Second),
			float64(r.LedgersCovered)/r.Elapsed.Seconds(),
			float64(r.EventsEmitted)/r.Elapsed.Seconds())
	}
	for _, line := range projectedRebuildKindLines(r, write) {
		fmt.Println(line)
	}
	if !write {
		fmt.Println("\n(dry-run — re-run with -write to persist to Postgres)")
	}
}
