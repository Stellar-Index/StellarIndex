package ingest

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
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
// This is intentionally a one-shot SQL operation: the projector
// goroutine in `stellarindex-indexer` does the actual work. Operators
// don't need a dedicated subprocess for replay because the
// projector is already in steady-state. See
// docs/operations/runbooks/projector-replay.md.
func projectorReplay(args []string) error {
	fs := flag.NewFlagSet("projector-replay", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	source := fs.String("source", "", "Projector source name to rewind (required); see internal/projector/registry.go for the list")
	from := fs.Uint("from", 0, "Rewind the projector cursor to this ledger (inclusive); the projector tails forward to the live tip from here")
	dryRun := fs.Bool("dry-run", false, "Print intended rewind without writing the cursor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *source == "" || *from == 0 {
		return errors.New("-config, -source, and -from are required")
	}

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
	if *dryRun {
		_, _ = fmt.Fprintf(os.Stdout,
			"dry-run: would RecordProjectionDirtyWindow(%q, [%d,%d]) then UpsertCursor(projector, %q, %d)\n",
			*source, target, currentLedger, *source, rewindTo)
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
		Reason: fmt.Sprintf("projector-replay rewind %d -> %d", currentLedger, target),
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
	return nil
}
