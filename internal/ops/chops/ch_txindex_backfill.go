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
//
// Safe default (W8.15): the flag defaults are -from 2 / -to 0(=tip), so a
// BARE `ch-txindex-backfill` with no arguments used to silently start the
// entire ledger-2..tip (~10.2B row) backfill — an easy footgun for a heavy
// job the cautions above say must be babysat. The full-history run is a real
// operation, but it now needs an explicit word: an explicit -from (a resume
// point / lower bound), an explicit -to (an upper bound), or -full to run the
// whole history from scratch. This mirrors trim-galexie-archive requiring
// --commit for its destructive path — the big operation must be intentional.
type txIndexBackfillPlan struct {
	chAddr string
	from   uint32
	to     uint32 // 0 = resolve to the current lake tip at run time
	window uint32
}

// parseTxIndexBackfillFlags parses the ch-txindex-backfill flags and enforces
// the safe default described on chTxIndexBackfill: no implicit full-history
// run. It does not touch ClickHouse (tip resolution happens in the runner),
// so the guard is unit-testable without a live lake.
func parseTxIndexBackfillFlags(args []string) (txIndexBackfillPlan, error) {
	fs := flag.NewFlagSet("ch-txindex-backfill", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current lake tip)")
	window := fs.Uint("window", 5_000_000, "ledgers per INSERT…SELECT window")
	full := fs.Bool("full", false, "run the ENTIRE history (ledger 2 .. current lake tip, ~10.2B rows). Required to run without an explicit -from/-to, so a bare invocation never starts the full backfill by accident.")
	if err := fs.Parse(args); err != nil {
		return txIndexBackfillPlan{}, err
	}
	if *from == 0 || *window == 0 {
		return txIndexBackfillPlan{}, fmt.Errorf("-from and -window must be > 0")
	}

	// Which bounds did the operator name explicitly? A bare run leaves all
	// unset, which is the footgun we refuse below.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !*full && !set["from"] && !set["to"] {
		return txIndexBackfillPlan{}, fmt.Errorf(
			"refusing an implicit full-history backfill (ledger 2..tip, ~10.2B rows on r1): pass -from (a resume point / lower bound), -to (an upper bound), or -full to run the entire history from scratch")
	}

	return txIndexBackfillPlan{
		chAddr: *chAddr,
		from:   uint32(*from),
		to:     uint32(*to),
		window: uint32(*window),
	}, nil
}

func chTxIndexBackfill(args []string) error {
	plan, err := parseTxIndexBackfillFlags(args)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	last := plan.to
	if last == 0 {
		tip, err := clickhouse.MaxLedger(ctx, plan.chAddr)
		if err != nil {
			return fmt.Errorf("resolve lake tip: %w", err)
		}
		last = tip
	}
	if last < plan.from {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, plan.from)
	}

	fmt.Fprintf(os.Stderr, "ch-txindex-backfill: filling stellar.tx_hash_index for ledgers %d..%d (window %d) on %s\n",
		plan.from, last, plan.window, plan.chAddr)
	return clickhouse.BackfillTxHashIndex(ctx, plan.chAddr, plan.from, last, plan.window,
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-txindex-backfill: "+format+"\n", a...)
		})
}
