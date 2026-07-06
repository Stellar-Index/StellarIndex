package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
)

// chParticipantBackfill fills stellar.operation_participants (the NON-source
// side of ADR-0038 Phase B account history) for historical ledgers by
// re-deriving participants from stellar.operations.body_xdr IN THE CLICKHOUSE
// LAKE — not a multi-day Galexie re-walk (this is what turns BACKLOG #59 into a
// runnable CH-internal job). operation_participants captures live-forward only;
// stellar.operations holds the full genesis→tip history with the op body, so
// every historical participant set is derivable without touching MinIO.
//
// The op's own source_account is already a full-history operations column (the
// account-history reader UNIONs it), so this backfill writes only the NON-source
// participants — byte-identical to the live extractor, with which it shares the
// derivation (clickhouse.operationParticipantRows). Writing source rows too
// would double-count each op for its own source account.
//
// Windowed + resumable like ch-txindex-backfill: each window prints its resume
// point (-from) and re-running a window is idempotent (ReplacingMergeTree keyed
// on (account, ledger_seq, tx_index, op_index)). -dry-run decodes + counts the
// participants that WOULD be written, writing nothing. On r1 run it under
// /usr/local/sbin/run-heavy-job.sh, serialized with other heavy CH jobs and the
// root-<2G watchdog (heavy CH load has wedged the log channel before —
// 2026-06-11 incident).
func chParticipantBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-participant-backfill", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = the live-capture floor − 1, i.e. exactly the gap operation_participants doesn't already cover)")
	window := fs.Uint("window", 500_000, "ledgers per read-decode-insert window (smaller = finer resume granularity)")
	dryRun := fs.Bool("dry-run", false, "decode + COUNT the participants that WOULD be written per window; write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}

	ctx, cancel := signalContext()
	defer cancel()

	last, err := resolveParticipantTo(ctx, *chAddr, uint32(*to))
	if err != nil {
		return err
	}
	if last < uint32(*from) {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, *from)
	}

	mode := "WRITE"
	if *dryRun {
		mode = "DRY-RUN (no writes)"
	}
	fmt.Fprintf(os.Stderr, "ch-participant-backfill: %s — filling stellar.operation_participants for ledgers %d..%d (window %d) on %s\n",
		mode, *from, last, *window, *chAddr)

	stats, berr := clickhouse.BackfillOperationParticipants(ctx, *chAddr, uint32(*from), last, uint32(*window), *dryRun,
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-participant-backfill: "+format+"\n", a...)
		})

	verb := "wrote"
	if *dryRun {
		verb = "would write"
	}
	fmt.Fprintf(os.Stderr, "ch-participant-backfill: done — scanned %d ops, %s %d participant rows (%d decode-errors)\n",
		stats.OpsScanned, verb, stats.Participants, stats.DecodeErrors)
	return berr
}

// resolveParticipantTo resolves the inclusive upper ledger. A non-zero toFlag is
// honoured as-is. toFlag==0 auto-targets the gap below live capture: the
// live-capture floor minus one (min ledger in operation_participants). If the
// table is empty (no live capture yet) it falls back to the lake tip.
func resolveParticipantTo(ctx context.Context, chAddr string, toFlag uint32) (uint32, error) {
	if toFlag != 0 {
		return toFlag, nil
	}
	floor, ok, err := clickhouse.MinParticipantLedger(ctx, chAddr)
	if err != nil {
		return 0, fmt.Errorf("resolve live-capture floor: %w", err)
	}
	switch {
	case ok && floor > 1:
		last := floor - 1
		fmt.Fprintf(os.Stderr, "ch-participant-backfill: live-capture floor is ledger %d; backfilling the gap up to %d\n", floor, last)
		return last, nil
	case ok: // floor is at or below the first ledger — nothing beneath it
		return 0, fmt.Errorf("operation_participants already starts at ledger %d; nothing to backfill (pass -to explicitly to force)", floor)
	default:
		tip, terr := clickhouse.MaxLedger(ctx, chAddr)
		if terr != nil {
			return 0, fmt.Errorf("resolve lake tip (operation_participants empty): %w", terr)
		}
		fmt.Fprintf(os.Stderr, "ch-participant-backfill: operation_participants is empty; backfilling to lake tip %d\n", tip)
		return tip, nil
	}
}
