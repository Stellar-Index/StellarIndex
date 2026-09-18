package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// projectorReplay rewinds the projector's per-source cursor so the
// projector goroutine re-projects a historical range from
// `soroban_events`. Replaces the family of `*-backfill` subcommands
// (cctp-backfill, rozo-backfill, soroswap-skim-backfill,
// comet-liquidity-backfill, phoenix-backfill, blend-backfill,
// sep41-transfers-backfill, drain-cascade-window) per ADR-0032 Phase 5.
//
// Mechanism:
//   - Read the projector's per-source cursor: (projector, <name>).
//   - If the requested `-from` is less than the current cursor,
//     rewind it. The projector's next cycle picks up at that lower
//     bound and tails forward to the live tip.
//   - If `-from` is already at or below the cursor, no-op (operator
//     is asking for ground that's already been re-walked).
//   - `INSERT … ON CONFLICT DO NOTHING` in every per-source table
//     makes the re-walk idempotent.
//
// The rewind itself is one SQL operation — the projector goroutine in
// `stellarindex-indexer` does the re-walk. The command then STAYS to
// finish the job: a replay writes trades (aquarius/soroswap/phoenix/
// comet all persist trades through the projector) into a historical
// time range, and every continuous aggregate over `trades` only ever
// rolls its refresh policy FORWARD over its own start_offset window —
// prices_1m's is five minutes. Rows re-projected into a range older
// than that are durable in the hypertable and invisible to every read:
// /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception all
// serve from the aggregates. So once the projector has re-walked past
// the original cursor, this command re-materializes the price CAGGs
// over the replayed range and fails loudly if it cannot — rather than
// leaving that as a sentence in a runbook (K006). `-refresh-caggs=false`
// opts out explicitly and says what it costs. See
// docs/operations/runbooks/projector-replay.md.
func projectorReplay(args []string) error {
	fs := flag.NewFlagSet("projector-replay", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Projector source name to rewind (required); see internal/projector/registry.go for the list")
	from := fs.Uint("from", 0, "Rewind the projector cursor to this ledger (inclusive); the projector tails forward to the live tip from here")
	refreshCAGGs := fs.Bool("refresh-caggs", true, "After the projector re-walks the rewound range, re-materialize the price continuous aggregates over it. The CAGG policies only roll forward, so re-projected historical trades are invisible to every OHLC/VWAP read until this runs")
	catchUp := fs.Bool("wait", true, "Wait for the projector to re-walk the rewound range before refreshing the CAGGs. -wait=false returns as soon as the cursor is rewound and leaves the refresh to the operator")
	catchUpTimeout := fs.Duration("wait-timeout", 30*time.Minute, "How long to wait for the projector to re-walk the rewound range")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *source == "" || *from == 0 {
		return errors.New("-config, -source, and -from are required")
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

	cursor, err := store.GetCursor(ctx, "projector", *source)
	if err != nil && !errors.Is(err, timescale.ErrNotFound) {
		return fmt.Errorf("read projector cursor: %w", err)
	}
	if errors.Is(err, timescale.ErrNotFound) {
		// Fail LOUDLY on an unknown source rather than falling through
		// to currentLedger=0, where `target >= 0` prints "no action"
		// and exits 0.
		//
		// This is not a theoretical typo. The projector writes its
		// cursor under the projector SOURCE name (blend_backstop,
		// sep41_transfers, …), but `find-data-gaps` prints a
		// remediation command using the gap detector's per-TABLE
		// target names, which are hyphenated (blend-backstop,
		// sep41-transfers, soroswap-skim …) and mostly do not match —
		// and the runbook lists several of those hyphenated names as
		// valid under a heading claiming they match the registry. So
		// an operator pasting the generated command for a real
		// projection hole got a green exit code and a "no action"
		// line, while nothing was rewound and the gap survived (cold
		// audit 2026-08-03).
		//
		// A genuinely never-run source has no cursor row either, but
		// there is nothing to rewind in that case, so refusing is
		// correct for both.
		return fmt.Errorf("no projector cursor for source %q — check the name against "+
			"internal/projector/registry.go (projector SOURCE names are underscored, e.g. "+
			"blend_backstop / sep41_transfers; the gap detector's hyphenated per-table "+
			"target names are NOT valid here), or the source has never run", *source)
	}
	currentLedger := cursor.LastLedger
	target := uint32(*from)
	if target == 0 {
		return fmt.Errorf("invalid -from %d", *from)
	}
	if target >= currentLedger {
		_, _ = fmt.Fprintf(os.Stdout,
			"projector cursor for source=%q is already at ledger %d ≤ requested rewind point %d — no action.\n",
			*source, currentLedger, target)
		return nil
	}

	_, _ = fmt.Fprintf(os.Stdout,
		"rewind projector cursor source=%q from %d → %d (delta = %d ledgers)\n",
		*source, currentLedger, target, currentLedger-target)
	// Projector cursor is "last fully-processed ledger." Rewinding
	// to (target - 1) makes the projector start its next cycle at
	// `target` inclusive (see projector.cycleOneSource:fromLedger =
	// cursor.LastLedger + 1).
	rewindTo := target
	if rewindTo > 0 {
		rewindTo--
	}
	if dryRun {
		_, _ = fmt.Fprintf(os.Stdout,
			"dry-run: would RecordProjectionDirtyWindow(%q, [%d,%d]) then UpsertCursor(projector, %q, %d)\n",
			*source, target, currentLedger, *source, rewindTo)
		if *refreshCAGGs && *catchUp {
			_, _ = fmt.Fprintf(os.Stdout,
				"dry-run: would then wait up to %s for the projector cursor to reach %d and refresh the price CAGGs over ledgers [%d,%d]\n",
				*catchUpTimeout, currentLedger, target, currentLedger)
		}
		return nil
	}
	// Record the dirty window BEFORE the rewind, and FAIL the replay if the
	// record cannot be written. This closes the carried-claim invalidation
	// gap (2026-07-31): the daily compute-completeness driver reconciles
	// only [watermark, tip] and CARRIES the prior clean projection claim for
	// the older prefix — a rewind that rewrites served rows below the
	// watermark silently invalidates that carried claim, which is exactly
	// how the 07-30 cctp replay's 19,366 event_index-0 twins at
	// 62.27M–63.55M escaped the verifier. The window [target, currentLedger]
	// is the below-cursor range about to be rewritten; compute-completeness
	// extends its reconcile floor to cover it and clears it only on a clean
	// verdict. Record-then-rewind is the fail-closed order: a crash between
	// the two leaves a spurious window (one clean verify clears it), never a
	// rewind with no record.
	if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
		Source: *source,
		From:   target,
		To:     currentLedger,
		Reason: timescale.ProjectorReplayReason(currentLedger, target),
	}); err != nil {
		return fmt.Errorf("record dirty window (refusing to rewind without it — the completeness verifier would carry a stale claim over the rewritten range): %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout,
		"recorded projection dirty window source=%q [%d,%d] — compute-completeness will force a re-reconcile of this range before carrying any projection claim over it\n",
		*source, target, currentLedger)
	// RewindCursor, NOT UpsertCursor: the upsert path carries a
	// monotonic-forward guard (F-0020) that silently no-ops on a
	// backward write — which made this whole subcommand a no-op that
	// printed success (caught 2026-06-12).
	if err := store.RewindCursor(ctx, "projector", *source, rewindTo); err != nil {
		return fmt.Errorf("rewind cursor: %w", err)
	}
	_, _ = fmt.Fprintf(os.Stdout,
		"projector cursor rewound — next projector cycle (≤ 5s) will start re-projecting from ledger %d\n",
		target)

	replayed := chunkRange{from: target, to: currentLedger}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	switch {
	case !*refreshCAGGs:
		logger.Warn("skipping post-replay CAGG refresh (-refresh-caggs=false)",
			"from", replayed.from, "to", replayed.to,
			"impact", "trades re-projected into this range stay unmaterialised in prices_1m/15m/1h/4h/1d/1w/1mo: the CAGG refresh policies only roll forward over their own start_offset window, so nothing picks a historical bucket up on its own cadence. The rows are durable, but /v1/ohlc, /v1/chart, /v1/vwap and /v1/history/since-inception read the aggregates and will serve short over this range until a manual refresh_continuous_aggregate covers it",
		)
		return nil
	case !*catchUp:
		logger.Warn("not waiting for the projector to re-walk (-wait=false); the CAGG refresh is now the operator's",
			"from", replayed.from, "to", replayed.to,
			"follow_up", "once the projector cursor passes the pre-rewind ledger, re-run with -from the same value, or refresh the price CAGGs over the range by hand",
		)
		return nil
	}

	// Fresh context: the 30s budget above covers the cursor statements,
	// not a re-walk of the replayed range plus seven materializations.
	rctx, rcancel := context.WithTimeout(context.Background(), *catchUpTimeout+caggRefreshGrace)
	defer rcancel()
	if err := awaitProjectorCursor(rctx, logger, store, *source, currentLedger, *catchUpTimeout, projectorCatchUpPoll); err != nil {
		return err
	}
	if err := refreshCAGGsForChunk(rctx, logger, store, replayed); err != nil {
		return fmt.Errorf("post-replay CAGG refresh over ledgers [%d,%d]: %w — the re-projected trades are in the hypertable but no OHLC/VWAP read can reach them until a refresh covers this range; re-run this command with the same -from, or refresh the views by hand",
			replayed.from, replayed.to, err)
	}
	_, _ = fmt.Fprintf(os.Stdout,
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

// awaitProjectorCursor blocks until the projector's cursor for `source`
// has re-walked back up to `target` (the ledger it sat at before the
// rewind), i.e. until the replayed range has actually been re-projected.
//
// Returning early would refresh the continuous aggregates over rows that
// are not written yet — a refresh that reports success and materializes
// the old, short answer, which is worse than not refreshing at all
// because the operator has a green run to point at. On timeout it fails,
// naming what is left to do: the rewind is already durable at that point,
// so a silent return would leave the range invisible to every read with
// nothing to say so.
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
			return fmt.Errorf("projector cursor for %q is at ledger %d after %s, still short of the pre-rewind ledger %d: the replayed range is not fully re-projected, so the price CAGGs were NOT refreshed over it. Let the projector catch up and re-run this command with the same -from (the rewind is already durable and idempotent), or raise -wait-timeout",
				source, cursor.LastLedger, budget, target)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the projector to re-walk %q: %w", source, ctx.Err())
		case <-time.After(poll):
		}
	}
}
