package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// replayGenerationNote is printed on every rewind and dry-run: the live projector writes at derive_generation 0 and the writers' ON
// CONFLICT guard refuses to overwrite a row a re-derive stamped higher.
const replayGenerationNote = "note: the projector re-writes at derive_generation 0 — rows in this range that a re-derive " +
	"(projected-rebuild, ch-rebuild) already stamped with a higher generation keep their stored values; " +
	"to correct those, re-derive the range with projected-rebuild -write instead.\n"

const replayNotReachedMsg = "projector cursor for source=%q is at ledger %d, which has not yet passed requested ledger %d — " +
	"nothing to rewind; the live projector's forward pass will project it (%d ledgers still ahead of the cursor).\n"

// projectorReplay rewinds the projector's per-source cursor to re-project a range from soroban_events (replaces *-backfill,
// ADR-0032; gen-0 rows only), then STAYS to re-materialize the price CAGGs (refresh policy only rolls forward) and fails loudly
// if it cannot; -refresh-caggs=false opts out. See docs/operations/runbooks/projector.md#stellarindex_projector_replay_stalled.
func projectorReplay(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("projector-replay", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Projector source name to rewind (required); see internal/projector/registry.go for the list")
	from := fs.Uint("from", 0, "Rewind the projector cursor to this ledger (inclusive); the projector tails forward to the live tip from here")
	refreshCAGGs := fs.Bool("refresh-caggs", true, "After the projector re-walks the rewound range, re-materialize the price continuous aggregates over it. The CAGG policies only roll forward, so re-projected historical trades are invisible to every OHLC/VWAP read until this runs")
	catchUp := fs.Bool("wait", true, "Wait for the projector to re-walk the rewound range before refreshing the CAGGs. -wait=false returns as soon as the cursor is rewound and leaves the refresh to the operator")
	catchUpTimeout := fs.Duration("wait-timeout", 30*time.Minute, "How long to wait for the projector to re-walk the rewound range")
	refreshOnly := fs.Bool("refresh-only", false, "Recovery mode: re-materialize the price CAGGs over [-from,-refresh-to] WITHOUT rewinding the cursor. Use this after a -wait-timeout instead of re-running with the same -from — a plain re-run recomputes the rewind target from the now partially-advanced cursor and re-walks the whole range again, never refreshing the gap left by the timed-out run. Requires -refresh-to; refuses if the projector has not actually re-walked up to -refresh-to yet")
	refreshTo := fs.Uint("refresh-to", 0, "Upper bound (inclusive) of the ledger range to refresh when -refresh-only is set")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *source == "" || uint32(*from) == 0 {
		return errors.New("-config, -source, and -from are required")
	}
	if *refreshOnly {
		return projectorRefreshOnly(w, *cfgPath, *source, uint32(*from), uint32(*refreshTo), gate)
	}
	// Before the config load and before any store access: a refusal must not depend on a reachable database, and applies to
	// -dry-run too (a dry run that prints a rewind plan for a source the real run would refuse is a plan nobody can execute).
	if err := checkReplayBackfillSafe(*source, uint32(*from)); err != nil {
		return err
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := gateProjectorReplay(cfg, store, *source, uint32(*from)); err != nil { //nolint:gosec // ledger sequences fit uint32 in real usage.
		return err
	}

	cursor, err := store.GetCursor(ctx, "projector", *source)
	if err != nil && !errors.Is(err, timescale.ErrNotFound) {
		return fmt.Errorf("read projector cursor: %w", err)
	}
	if errors.Is(err, timescale.ErrNotFound) {
		// Fail LOUDLY on an unknown source: the cursor is keyed by projector name (blend_backstop), not the
		// hyphenated gap-detector table target, and falling through to currentLedger=0 would print "no
		// action" and exit 0 while the gap survives. A never-run source has no cursor row either, so refusing is right.
		return fmt.Errorf("no projector cursor for source %q — check the name against "+
			"internal/projector/registry.go (projector SOURCE names are underscored, e.g. "+
			"blend_backstop / sep41_transfers; the gap detector's hyphenated per-table "+
			"target names are NOT valid here), or the source has never run", *source)
	}
	currentLedger := cursor.LastLedger
	target := uint32(*from)
	if target >= currentLedger {
		// The cursor has NOT reached the requested ledger, so there is
		// nothing to rewind: the live projector's forward pass covers it.
		_, _ = fmt.Fprintf(w, replayNotReachedMsg, *source, currentLedger, target, target-currentLedger)
		return nil
	}

	_, _ = fmt.Fprintf(w,
		"rewind projector cursor source=%q from %d → %d (delta = %d ledgers)\n",
		*source, currentLedger, target, currentLedger-target)
	// Projector cursor is "last fully-processed ledger." Rewinding to (target - 1) makes the projector start its next cycle at
	// `target` inclusive (see projector.cycleOneSource:fromLedger = cursor.LastLedger + 1).
	rewindTo := target
	if rewindTo > 0 {
		rewindTo--
	}
	if dryRun {
		_, _ = fmt.Fprintf(w,
			"dry-run: would RecordProjectionDirtyWindow(%q, [%d,%d]) then RewindCursor(projector, %q, %d)\n",
			*source, target, currentLedger, *source, rewindTo)
		if *refreshCAGGs && *catchUp {
			_, _ = fmt.Fprintf(w,
				"dry-run: would then wait up to %s for the projector cursor to reach %d and refresh the price CAGGs over ledgers [%d,%d]\n",
				*catchUpTimeout, currentLedger, target, currentLedger)
		}
		printSEP41ReplayDryRunNote(w, *source)
		_, _ = fmt.Fprint(w, replayGenerationNote)
		return nil
	}
	rewoundFrom, err := rewindRecordingDirtyWindow(ctx, w, store, *source, target, currentLedger)
	if err != nil {
		return err
	}

	if err := reportSEP41RollupReset(ctx, w, store, *source); err != nil {
		return err
	}

	return rematerializeReplayedRange(
		w, slog.New(slog.NewTextHandler(w, nil)), store, *source,
		chunkRange{from: target, to: rewoundFrom},
		replayFollowUp{refreshCAGGs: *refreshCAGGs, wait: *catchUp, waitTimeout: *catchUpTimeout},
	)
}

// gateProjectorReplay runs the per-WASM audit gate on its own context: the
// caller's 30s one is too short for a lake instance-index read.
func gateProjectorReplay(cfg config.Config, store *timescale.Store, source string, from uint32) error {
	gctx, gcancel := opsutil.SignalContext()
	defer gcancel()
	return wasmaudit.GateReplay(gctx, cfg.Storage.ClickHouseAddr, cfg.Oracle, store.LoadProtocolContracts, []string{source}, from, 0)
}

// replayRewinder is the slice of the store the record-then-rewind step needs.
type replayRewinder interface {
	RecordProjectionDirtyWindow(ctx context.Context, w timescale.ProjectionDirtyWindow) error
	RewindCursor(ctx context.Context, source, sub string, lastLedger uint32) (uint32, error)
}

// rewindRecordingDirtyWindow records the dirty window [target, readLedger], rewinds the cursor to target-1 and returns the ledger it
// held. Record-then-rewind is fail-closed: a crash between leaves a spurious window, never a rewind with no record.
func rewindRecordingDirtyWindow(ctx context.Context, w io.Writer, store replayRewinder, source string, target, readLedger uint32) (uint32, error) {
	if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
		Source: source,
		From:   target,
		To:     readLedger,
		Reason: timescale.ProjectorReplayReason(readLedger, target),
	}); err != nil {
		return 0, fmt.Errorf("record dirty window (refusing to rewind without it — the completeness verifier would carry a stale claim over the rewritten range): %w", err)
	}
	_, _ = fmt.Fprintf(w,
		"recorded projection dirty window source=%q [%d,%d] — compute-completeness will force a re-reconcile of this range before carrying any projection claim over it\n",
		source, target, readLedger)
	// RewindCursor, NOT UpsertCursor: the upsert path's monotonic-forward
	// guard silently no-ops on a backward write.
	rewoundFrom, err := store.RewindCursor(ctx, "projector", source, target-1)
	if err != nil {
		return 0, fmt.Errorf("rewind cursor: %w", err)
	}
	// A projector cycle that committed after the read moved the cursor past
	// readLedger, so the re-walk rewrites more than the window recorded.
	if rewoundFrom > readLedger {
		if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
			Source: source,
			From:   target,
			To:     rewoundFrom,
			Reason: timescale.ProjectorReplayReason(rewoundFrom, target),
		}); err != nil {
			return 0, fmt.Errorf("widen dirty window to [%d,%d] (the cursor is ALREADY rewound; re-record it before the next compute-completeness run): %w", target, rewoundFrom, err)
		}
		_, _ = fmt.Fprintf(w,
			"projector advanced to %d during the replay; widened dirty window source=%q to [%d,%d]\n",
			rewoundFrom, source, target, rewoundFrom)
	}
	_, _ = fmt.Fprintf(w,
		"projector cursor rewound from %d — next projector cycle (≤ 5s) will start re-projecting from ledger %d\n",
		rewoundFrom, target)
	_, _ = fmt.Fprint(w, replayGenerationNote)
	return rewoundFrom, nil
}

// projectorRefreshOnly is the -refresh-only path: re-materialize CAGGs over [from,to] WITHOUT touching the cursor (-from would rewind
// and re-walk everything); skips checkReplayBackfillSafe and the rewind, and fails closed if the cursor is short of to.
func projectorRefreshOnly(w io.Writer, cfgPath, source string, from, to uint32, gate *opsutil.WriteGate) error {
	if to == 0 || to < from {
		return errors.New("-refresh-only requires -refresh-to >= -from")
	}
	gate.Banner()
	if gate.DryRun() {
		_, _ = fmt.Fprintf(w,
			"dry-run: would refresh the price CAGGs over ledgers [%d,%d] for source=%q (no cursor rewind)\n",
			from, to, source)
		return nil
	}
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := refreshOnlyRange(ctx, slog.New(slog.NewTextHandler(w, nil)), store, source, from, to); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "price CAGGs re-materialized over [%d,%d] (no cursor rewind)\n", from, to)
	return nil
}

// refreshOnlyRange fails closed if the projector cursor is short of to, then refreshes the CAGGs over [from,to].
// The refresh drops ctx's deadline: that 30s budget is for opening Postgres and the cursor read, and each refresh CALL carries its own bound.
func refreshOnlyRange(ctx context.Context, logger *slog.Logger, store replayFinisher, source string, from, to uint32) error {
	cursor, err := store.GetCursor(ctx, "projector", source)
	if err != nil {
		return fmt.Errorf("read projector cursor: %w", err)
	}
	if cursor.LastLedger < to {
		return fmt.Errorf("refusing to refresh [%d,%d]: projector cursor for %q is only at %d, %d ledger(s) short of %d — the range is not fully re-projected yet; wait for the projector to catch up and re-run",
			from, to, source, cursor.LastLedger, to-cursor.LastLedger, to)
	}
	if err := refreshCAGGsForChunk(context.WithoutCancel(ctx), logger, store, chunkRange{from: from, to: to}); err != nil {
		return fmt.Errorf("refresh price CAGGs over ledgers [%d,%d]: %w", from, to, err)
	}
	return nil
}

// checkReplayBackfillSafe refuses a replay of a source whose decoder is not audited against every WASM generation in its history, as a
// rewind feeds the CURRENT decoder every old event. No override flag; unknown names are refused via [external.ReplayBackfillSafe].
func checkReplayBackfillSafe(source string, from uint32) error {
	if external.ReplayBackfillSafe(source) {
		return nil
	}
	return fmt.Errorf(
		"refusing to replay source %q from ledger %d — it is not BackfillSafe (per-WASM-hash audit pending, or "+
			"not a known source): a rewind re-decodes every historical event with the CURRENT decoder, and "+
			"Soroban contracts upgrade in place, so an unaudited old WASM generation decodes to silently wrong "+
			"rows. Run stellarindex-ops wasm-history -from %d -to <tip> -contracts <CID> for the source's "+
			"contracts, review every emitted WASM hash against the current decoder, then audit each WASM hash under docs/operations/wasm-audits/, add it to internal/wasmaudit/audited_wasm.json and set Backfill: BackfillPerWASM in internal/sources/external/registry.go in the same PR (see docs/architecture/domain-traps.md, "+
			"\"Soroban DeFi contracts upgrade in place\"). If the name is simply wrong: projector SOURCE names "+
			"are underscored (blend_backstop, sep41_transfers — the gap detector's hyphenated per-table target "+
			"names are NOT valid here); see internal/projector/registry.go",
		source, from, from)
}

// sep41RollupResetter is the one-call slice of the store resetSEP41RollupAfterReplay needs, so a test can drive it without Postgres.
type sep41RollupResetter interface {
	ResetSEP41SupplyRollupFold(ctx context.Context, contractIDs []string) (int64, error)
}

// resetSEP41RollupAfterReplay resets the sep41_supply_rollup fold checkpoint when a replay re-walks sep41_supply: the fold only
// advances past last_ledger, so corrected rows below the rewound cursor would leave supply wrong. FULL reset; false otherwise.
func resetSEP41RollupAfterReplay(ctx context.Context, store sep41RollupResetter, source string) (reset bool, n int64, err error) {
	if source != sep41supply.SourceName {
		return false, 0, nil
	}
	n, err = store.ResetSEP41SupplyRollupFold(ctx, nil)
	if err != nil {
		return true, 0, err
	}
	return true, n, nil
}

// reportSEP41RollupReset calls resetSEP41RollupAfterReplay and prints the outcome or fails loudly (split out for the cognitive-complexity limit).
func reportSEP41RollupReset(ctx context.Context, w io.Writer, store sep41RollupResetter, source string) error {
	reset, n, err := resetSEP41RollupAfterReplay(ctx, store, source)
	if err != nil {
		return fmt.Errorf("reset sep41_supply_rollup fold after replay (the cursor rewind is already durable, but served SEP-41 supply stays wrong for any row this replay corrects at or below the old fold checkpoint until the fold is reset): %w", err)
	}
	if reset {
		_, _ = fmt.Fprintf(w,
			"reset %d sep41_supply_rollup fold row(s), re-folded in place up to the rewound cursor — the aggregator worker folds the replayed range as it lands (genesis baseline preserved)\n", n)
	}
	return nil
}

// printSEP41ReplayDryRunNote prints the dry-run line for the SEP-41 rollup reset a real `-source sep41_supply` run would perform.
func printSEP41ReplayDryRunNote(w io.Writer, source string) {
	if source != sep41supply.SourceName {
		return
	}
	_, _ = fmt.Fprintf(w,
		"dry-run: would then ResetSEP41SupplyRollupFold(nil) — the rewound range may already be behind the sep41_supply_rollup fold checkpoint, and the fold only ever looks ABOVE it\n")
}

// replayFollowUp carries projector-replay's post-rewind flags
// (-refresh-caggs, -wait, -wait-timeout) to rematerializeReplayedRange.
type replayFollowUp struct {
	refreshCAGGs bool
	wait         bool
	waitTimeout  time.Duration
}

// replayFinisher is the slice of the store the post-rewind tail needs:
// the cursor read the catch-up loop polls, and the CAGG refresh.
type replayFinisher interface {
	projectorCursorReader
	caggRefresher
}

// rematerializeReplayedRange is projectorReplay's post-rewind tail: wait for the projector to re-walk, then re-materialize CAGGs
// (projector_replay_refresh_test.go pins both hops). Opt-outs return nil since the rewind is durable.
func rematerializeReplayedRange(w io.Writer, logger *slog.Logger, store replayFinisher, source string, replayed chunkRange, opts replayFollowUp) error {
	switch {
	case !opts.refreshCAGGs:
		logger.Warn("skipping post-replay CAGG refresh (-refresh-caggs=false)",
			"from", replayed.from, "to", replayed.to,
			"impact", "trades re-projected into this range stay unmaterialised in every trades aggregate (timescale.TradesCAGGs: prices_*, twap_*, volume rollups), and re-projected oracle rows in every oracle_prices_* rung: the CAGG refresh policies only roll forward over their own start_offset window, so nothing picks a historical bucket up on its own cadence. The rows are durable, but /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception read the aggregates and will serve short over this range until a manual refresh_continuous_aggregate covers it",
		)
		return nil
	case !opts.wait:
		logger.Warn("not waiting for the projector to re-walk (-wait=false); the CAGG refresh is now the operator's",
			"from", replayed.from, "to", replayed.to,
			"follow_up", "once the projector cursor passes the pre-rewind ledger, refresh WITHOUT rewinding: projector-replay -source <source> -from <original -from> -refresh-only -refresh-to <pre-rewind ledger> (re-running with -from alone rewinds and re-walks again)",
		)
		return nil
	}

	// Fresh context: projectorReplay's 30s budget covers the cursor
	// statements, not a re-walk of the replayed range plus seven
	// materializations.
	rctx, rcancel := context.WithTimeout(context.Background(), opts.waitTimeout+caggRefreshGrace)
	defer rcancel()
	if err := awaitProjectorCursor(rctx, logger, store, source, replayed.to, opts.waitTimeout, projectorCatchUpPoll); err != nil {
		return err
	}
	if err := refreshCAGGsForChunk(rctx, logger, store, replayed); err != nil {
		return fmt.Errorf("post-replay CAGG refresh over ledgers [%d,%d]: %w — the re-projected trades are in the hypertable but no OHLC/VWAP read can reach them until a refresh covers this range; re-run this command with the same -from, or refresh the views by hand",
			replayed.from, replayed.to, err)
	}
	_, _ = fmt.Fprintf(w,
		"price CAGGs re-materialized over the replayed range [%d,%d]\n",
		replayed.from, replayed.to)
	return nil
}

// caggRefreshGrace is the head-room the post-replay context carries on
// top of the catch-up budget, for the refresh itself.
const caggRefreshGrace = 30 * time.Minute

// projectorCatchUpPoll is how often the catch-up loop re-reads the
// projector cursor — well inside the projector's own 5s cycle.
const projectorCatchUpPoll = 5 * time.Second

// projectorCursorReader is the slice of the store awaitProjectorCursor
// needs — one cursor read, so a test can drive the catch-up loop.
type projectorCursorReader interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

// awaitProjectorCursor blocks until the cursor re-walks to target: returning early would refresh CAGGs over unwritten rows and report
// green with the old answer. On timeout it fails naming what is left; the rewind is already durable.
func awaitProjectorCursor(ctx context.Context, logger *slog.Logger, r projectorCursorReader, source string, target uint32, budget, poll time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		cursor, err := r.GetCursor(ctx, "projector", source)
		if err != nil {
			return fmt.Errorf("read projector cursor while waiting for the re-walk: %w", err)
		}
		if cursor.LastLedger >= target {
			logger.Info("projector has re-walked the replayed range",
				"source", source, "cursor", cursor.LastLedger, "target", target)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("projector cursor for %q is at ledger %d after %s, still short of the pre-rewind ledger %d: the replayed range is not fully re-projected, so the price CAGGs were NOT refreshed over it. Do NOT re-run this command with the same -from — the rewind is durable, but a re-run recomputes the target from the now partially-advanced cursor (%d) and re-walks that whole range again, never refreshing the gap this run already covers. Instead let the projector keep catching up, then refresh what it re-walked without rewinding: projector-replay -source %s -from <original -from> -refresh-only -refresh-to %d (raise it once the cursor passes %d, or raise -wait-timeout and re-run the full wait)",
				source, cursor.LastLedger, budget, target, cursor.LastLedger, source, cursor.LastLedger, target)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the projector to re-walk %q: %w", source, ctx.Err())
		case <-time.After(poll):
		}
	}
}
