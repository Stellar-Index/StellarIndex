package chops

import (
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
// it under the root-<2G watchdog — heavy CH load can wedge the CH log
// channel on the small root partition. Each
// window prints a resume point; on interrupt/failure re-run with that -from.
//
// Safe default: the flag defaults are -from 2 / -to 0(=tip), so, unguarded, a
// BARE `ch-txindex-backfill` with no arguments would silently start the
// entire ledger-2..tip (~10.2B row) backfill — an easy footgun for a heavy
// job the cautions above say must be babysat. The full-history run is a real
// operation, but it needs an explicit word: an explicit -from (a resume
// point / lower bound), an explicit -to (an upper bound), or -full to run the
// whole history from scratch. This mirrors trim-galexie-archive requiring
// --commit for its destructive path — the big operation must be intentional.
type txIndexBackfillPlan struct {
	chAddr string
	from   uint32
	to     uint32 // 0 = resolve to the contiguous lake tip at run time
	window uint32
	write  bool // -write; without it the run previews the range and fills nothing
}

// genesisLedger is the first ledger a lake holds; a run starting here and
// ending at the resolved tip proves the index covers all history.
const genesisLedger = 2

// coversHistory reports whether a completed run spans genesis→tip, which is
// what the reader's coverage marker asserts.
func (p txIndexBackfillPlan) coversHistory() bool {
	return p.from <= genesisLedger && p.to == 0
}

// parseTxIndexBackfillFlags parses the ch-txindex-backfill flags and enforces
// the safe default described on chTxIndexBackfill: no implicit full-history
// run. It does not touch ClickHouse (tip resolution happens in the runner),
// so the guard is unit-testable without a live lake.
func parseTxIndexBackfillFlags(args []string) (txIndexBackfillPlan, error) {
	fs, gate := opsutil.NewMutatingFlagSet("ch-txindex-backfill")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = the contiguous lake tip from -from)")
	window := fs.Uint("window", 5_000_000, "ledgers per INSERT…SELECT window")
	full := fs.Bool("full", false, "run the ENTIRE history (ledger 2 .. current lake tip, ~10.2B rows). Required to run without an explicit -from/-to, so a bare invocation never starts the full backfill by accident.")
	if err := fs.Parse(args); err != nil {
		return txIndexBackfillPlan{}, err
	}
	if *from == 0 || *window == 0 {
		return txIndexBackfillPlan{}, fmt.Errorf("-from and -window must be > 0")
	}

	if err := opsutil.RequireExplicitRange(fs, *full, "ch-txindex-backfill", "10.2B"); err != nil {
		return txIndexBackfillPlan{}, err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return txIndexBackfillPlan{}, fmt.Errorf("ch-txindex-backfill: %w", err)
	}

	return txIndexBackfillPlan{
		chAddr: *chAddr,
		from:   uint32(*from),
		to:     uint32(*to),
		window: uint32(*window),
		write:  gate.Enabled(),
	}, nil
}

func chTxIndexBackfill(args []string) error {
	plan, err := parseTxIndexBackfillFlags(args)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	last, err := resolveBackfillTop(ctx, plan.chAddr, plan.from, plan.to)
	if err != nil {
		return err
	}
	if last < plan.from {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, plan.from)
	}

	if !opsutil.PrintWriteBanner(plan.write) {
		fmt.Fprintf(os.Stderr, "ch-txindex-backfill: would fill stellar.tx_hash_index for ledgers %d..%d (window %d) on %s\n",
			plan.from, last, plan.window, plan.chAddr)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ch-txindex-backfill: filling stellar.tx_hash_index for ledgers %d..%d (window %d) on %s\n",
		plan.from, last, plan.window, plan.chAddr)
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-txindex-backfill: "+format+"\n", a...)
	}
	if err := clickhouse.BackfillTxHashIndex(ctx, plan.chAddr, plan.from, last, plan.window, logf); err != nil {
		return err
	}
	if !plan.coversHistory() {
		logf("partial range; coverage marker NOT written (needs a run from ledger %d to the tip)", genesisLedger)
		return nil
	}
	if err := clickhouse.MarkTxHashIndexCovered(ctx, plan.chAddr, plan.from, last); err != nil {
		return err
	}
	logf("coverage marker written for ledgers %d..%d", plan.from, last)
	return nil
}
