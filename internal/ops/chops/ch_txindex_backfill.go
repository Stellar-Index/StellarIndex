package chops

import (
	"flag"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chTxIndexBackfill fills stellar.tx_hash_index (the hash-ordered
// GET /v1/tx/{hash} lookup table, docs/operations/perf-todo.md §4) from
// stellar.transactions history in windowed, resumable INSERT…SELECT chunks.
// The tx_hash_index_mv materialized view indexes everything ingested after
// the schema deploy; this covers the ~10.2B rows behind it. Pure
// ClickHouse-side SQL — no galexie walk, no config file needed.
//
// Operator cautions for the full-history run on r1 (perf-todo §4): the
// operator serializes it (don't run alongside other heavy CH jobs), and runs
// it under the root-<2G watchdog — heavy CH load has wedged the CH log
// channel on the small root partition before (2026-06-11 incident). Each
// window prints a resume point; on interrupt/failure re-run with that -from.
func chTxIndexBackfill(args []string) error {
	fs := flag.NewFlagSet("ch-txindex-backfill", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current lake tip)")
	window := fs.Uint("window", 5_000_000, "ledgers per INSERT…SELECT window")
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

	fmt.Fprintf(os.Stderr, "ch-txindex-backfill: filling stellar.tx_hash_index for ledgers %d..%d (window %d) on %s\n",
		*from, last, *window, *chAddr)
	return clickhouse.BackfillTxHashIndex(ctx, *chAddr, uint32(*from), last, uint32(*window),
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-txindex-backfill: "+format+"\n", a...)
		})
}
