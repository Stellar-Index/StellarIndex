package chops

import (
	"flag"
	"fmt"
	"os"

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
// serialize, run under run-heavy-job.sh, resume with the printed -from.
func chInstanceBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-instance-backfill", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current lake tip)")
	window := fs.Uint("window", 2_000_000, "ledgers per INSERT…SELECT window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	last := uint32(*to)
	if last == 0 {
		tip, err := clickhouse.MaxLedger(ctx, *chAddr)
		if err != nil {
			return fmt.Errorf("resolve lake tip: %w", err)
		}
		last = tip
	}
	if last < uint32(*from) {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, *from)
	}

	fmt.Fprintf(os.Stderr, "ch-instance-backfill: filling stellar.contract_instance_changes for ledgers %d..%d (window %d) on %s\n",
		*from, last, *window, *chAddr)
	return clickhouse.BackfillContractInstanceChanges(ctx, *chAddr, uint32(*from), last, uint32(*window),
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-instance-backfill: "+format+"\n", a...)
		})
}
