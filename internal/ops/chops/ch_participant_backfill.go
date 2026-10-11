package chops

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chParticipantBackfill fills the non-source side of stellar.operation_participants (ADR-0038
// Phase B) for historical ledgers by re-deriving from stellar.operations.body_xdr inside the lake.
// It writes only non-source participants, sharing the live derivation
// (clickhouse.operationParticipantRows); source rows too would double-count each op.
// Windowed and resumable; re-running a window is idempotent (ReplacingMergeTree). On r1 run it
// under /usr/local/sbin/run-heavy-job.sh, as heavy CH load can wedge the log channel.
func chParticipantBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-participant-backfill", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = the live-capture floor − 1, i.e. exactly the gap operation_participants doesn't already cover)")
	window := fs.Uint("window", 500_000, "ledgers per read-decode-insert window (smaller = finer resume granularity)")
	maxDecodeErrs := registerDecodeBudget(fs)
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	last, err := resolveParticipantTo(ctx, *chAddr, uint32(*from), uint32(*to))
	if err != nil {
		return err
	}
	if last < uint32(*from) {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, *from)
	}

	mode := "WRITE"
	if dryRun {
		mode = "DRY-RUN (no writes)"
	}
	fmt.Fprintf(os.Stderr, "ch-participant-backfill: %s — filling stellar.operation_participants for ledgers %d..%d (window %d) on %s\n",
		mode, *from, last, *window, *chAddr)

	stats, berr := backfillOperationParticipants(ctx, *chAddr, uint32(*from), last, uint32(*window), dryRun,
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-participant-backfill: "+format+"\n", a...)
		})

	fmt.Fprintln(os.Stderr, participantBackfillSummary(stats, dryRun, berr))
	if berr != nil {
		return berr
	}
	return enforceDecodeBudget("ch-participant-backfill", stats.DecodeErrors, *maxDecodeErrs)
}

// participantBackfillSummary is the run's final stderr line. Participants
// counts rows derived at scan time, so it is only a written count on a
// clean run; a failed run says so and defers to the error's resume point.
func participantBackfillSummary(stats clickhouse.ParticipantBackfillStats, dryRun bool, berr error) string {
	if berr != nil {
		return fmt.Sprintf("ch-participant-backfill: FAILED — scanned %d ops, derived %d participant rows, not all confirmed written (%d decode-errors); re-run with the -from and -to resume point in the error",
			stats.OpsScanned, stats.Participants, stats.DecodeErrors)
	}
	verb := "wrote"
	if dryRun {
		verb = "would write"
	}
	return fmt.Sprintf("ch-participant-backfill: done — scanned %d ops, %s %d participant rows (%d decode-errors)",
		stats.OpsScanned, verb, stats.Participants, stats.DecodeErrors)
}

// backfillOperationParticipants is the lake pass, a var so the exit-status
// contract is testable without a live ClickHouse.
var backfillOperationParticipants = clickhouse.BackfillOperationParticipants

// minParticipantLedger is clickhouse.MinParticipantLedger, a var so the
// bound resolution is testable without a live lake.
var minParticipantLedger = clickhouse.MinParticipantLedger

// resolveParticipantTo resolves the inclusive upper ledger, always within the lake's contiguous
// tip from `from` (resolveBackfillTop), since a hole read across is never derived. toFlag==0
// targets the live-capture floor minus one, or the tip when the table is empty. That floor is
// live only on a first run, so a resumed run must carry the -to its first run printed.
func resolveParticipantTo(ctx context.Context, chAddr string, from, toFlag uint32) (uint32, error) {
	if toFlag != 0 {
		return resolveBackfillTop(ctx, chAddr, from, toFlag)
	}
	floor, ok, err := minParticipantLedger(ctx, chAddr)
	if err != nil {
		return 0, fmt.Errorf("resolve live-capture floor: %w", err)
	}
	switch {
	case ok && floor > from:
		last := floor - 1
		fmt.Fprintf(os.Stderr, "ch-participant-backfill: live-capture floor is ledger %d; backfilling the gap up to %d\n", floor, last)
		return resolveBackfillTop(ctx, chAddr, from, last)
	case ok:
		return 0, fmt.Errorf("operation_participants already holds ledger %d, at or below -from %d: live capture covers it, or this resumes an interrupted run and those are that run's own rows — pass the -to the first run printed (or -to explicitly to force)",
			floor, from)
	default:
		tip, terr := resolveBackfillTop(ctx, chAddr, from, 0)
		if terr != nil {
			return 0, fmt.Errorf("resolve lake tip (operation_participants empty): %w", terr)
		}
		fmt.Fprintf(os.Stderr, "ch-participant-backfill: operation_participants is empty; backfilling to contiguous lake tip %d\n", tip)
		return tip, nil
	}
}
