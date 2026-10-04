package chops

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chInstanceBackfill fills stellar.contract_instance_changes (the
// per-contract instance-executable timeline behind the explorer's
// code-history read, deploy/clickhouse/contract_instance_changes.sql)
// from ledger_entry_changes history in windowed, resumable
// INSERT…SELECT chunks. The contract_instance_changes_mv materialized
// view covers everything ingested after the DDL applies; this covers
// the history behind it. MUST run to completion promptly after the DDL
// — the reader trusts a present + non-empty index as complete (see the
// DDL's operator contract). Same r1 cautions as the sibling backfills:
// serialize, run under run-heavy-job.sh, resume with the printed -from —
// including the same implicit-full-run refusal (a bare invocation needs
// an explicit -from/-to or -full; GH-1192).
func chInstanceBackfill(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-instance-backfill")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = the contiguous lake tip from -from)")
	window := fs.Uint("window", 2_000_000, "ledgers per INSERT…SELECT window")
	table := fs.String("table", clickhouse.ContractInstanceChangesTable,
		"target table in the stellar database: "+clickhouse.ContractInstanceChangesTable+
			", or "+clickhouse.ContractInstanceChangesV2Table+
			" during deploy/clickhouse/contract_instance_changes_tx_key.sql")
	full := fs.Bool("full", false, "run the ENTIRE history (ledger 2 .. current lake tip). Required to run without an explicit -from/-to, so a bare invocation never starts the full backfill by accident.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}
	if err := opsutil.RequireExplicitRange(fs, *full, "ch-instance-backfill", "history"); err != nil {
		return err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-instance-backfill: %w", err)
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	return runInstanceBackfill(ctx, *chAddr, *table, uint32(*from), uint32(*to), uint32(*window),
		func(last uint32) bool {
			if !gate.Banner() {
				fmt.Fprintf(os.Stderr, "ch-instance-backfill: would fill stellar.%s for ledgers %d..%d (window %d) on %s\n",
					*table, *from, last, *window, *chAddr)
				return false
			}
			fmt.Fprintf(os.Stderr, "ch-instance-backfill: filling stellar.%s for ledgers %d..%d (window %d) on %s\n",
				*table, *from, last, *window, *chAddr)
			return true
		},
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-instance-backfill: "+format+"\n", a...)
		})
}

// instanceGenesisFrom is the highest -from that still counts as a genesis
// start (ledger 1 is never in a lake; the flag defaults to 2). A resumed -from N
// run never marks: nothing proves an earlier run filled [2, N), so re-run from 2.
const instanceGenesisFrom = 2

// instanceMVSettle is how long the view must have existed before the run. Sink
// writes stellar.ledgers last, so a view born mid-flush can miss a ledger the
// backfill's tip does not yet include.
const instanceMVSettle = 10 * time.Minute

// Seams so the watermark decision is testable without a live lake.
var (
	backfillInstanceChanges = clickhouse.BackfillContractInstanceChangesInto
	setInstanceGenesisMark  = clickhouse.SetContractInstanceChangesGenesisWatermark
	readInstanceStart       = clickhouse.ReadInstanceBackfillStart
	resolveInstanceTop      = resolveBackfillTop
)

// runInstanceBackfill fills [from, last] and records the genesis watermark
// through last only when that claim is provable; otherwise it records nothing
// and the reader keeps its scan (a wrong "no history" is worse than a slow
// one). The table's only sources are this backfill and its view, so the mark
// needs: a genesis start, no explicit -to, the same settled view before and
// after the run, and a contiguous tip at or above the raw lake tip read first
// (no hole below ledgers that predate the view). confirm gets the resolved top
// and reports whether to write.
func runInstanceBackfill(ctx context.Context, addr, table string, from, toFlag, window uint32,
	confirm func(last uint32) bool, logf func(string, ...any),
) error {
	start, startErr := readInstanceStart(ctx, addr, table)
	last, err := resolveInstanceTop(ctx, addr, from, toFlag)
	if err != nil {
		return err
	}
	if last < from {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, from)
	}
	if !confirm(last) {
		return nil
	}
	if err := backfillInstanceChanges(ctx, addr, table, from, last, window, logf); err != nil {
		return err
	}
	end, endErr := readInstanceStart(ctx, addr, table)
	switch {
	case from > instanceGenesisFrom:
		logf("no genesis watermark: run started at %d, not genesis", from)
	case toFlag != 0:
		logf("no genesis watermark: explicit -to %d", toFlag)
	case startErr != nil:
		logf("no genesis watermark: start state unreadable: %v", startErr)
	case !start.MVExists:
		logf("no genesis watermark: %s_mv did not exist before the run, so ledgers after %d are uncovered", table, last)
	case start.MVAge < instanceMVSettle:
		logf("no genesis watermark: %s_mv is %s old, under the %s settle margin", table, start.MVAge, instanceMVSettle)
	case last < start.RawMaxLedger:
		logf("no genesis watermark: contiguous tip %d is below the raw lake tip %d (a hole)", last, start.RawMaxLedger)
	case endErr != nil:
		logf("no genesis watermark: end state unreadable: %v", endErr)
	case !end.MVExists || end.MVUUID != start.MVUUID || !end.MVModified.Equal(start.MVModified):
		logf("no genesis watermark: %s_mv was dropped or recreated during the run", table)
	default:
		if err := setInstanceGenesisMark(ctx, addr, table, last); err != nil {
			return fmt.Errorf("record genesis watermark %d: %w", last, err)
		}
		logf("genesis watermark recorded through ledger %d", last)
	}
	return nil
}
