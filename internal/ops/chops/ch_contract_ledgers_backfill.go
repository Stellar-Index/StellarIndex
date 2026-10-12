package chops

import (
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chContractLedgersBackfill fills stellar.contract_active_ledgers from stellar.contract_events history in windowed, resumable
// INSERT...SELECT chunks (the MV covers later rows). MUST run to completion promptly after the DDL: the reader trusts a non-empty index as
// complete. Cautions as ch-txindex-backfill: serialize under run-heavy-job.sh; a bare invocation is refused (needs -from/-to or -full).
func chContractLedgersBackfill(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-contract-ledgers-backfill")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 2, "first ledger (inclusive; resume point from a previous run's output)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = the contiguous lake tip from -from)")
	window := fs.Uint("window", 1_000_000, "ledgers per INSERT…SELECT window")
	full := fs.Bool("full", false, "run the ENTIRE history (ledger 2 .. current lake tip). Required to run without an explicit -from/-to, so a bare invocation never starts the full backfill by accident.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}
	if err := opsutil.RequireExplicitRange(fs, *full, "ch-contract-ledgers-backfill", "10B"); err != nil {
		return err
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-contract-ledgers-backfill: %w", err)
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	last, err := resolveBackfillTop(ctx, *chAddr, uint32(*from), uint32(*to))
	if err != nil {
		return err
	}
	if last < uint32(*from) {
		return fmt.Errorf("-to (%d) is below -from (%d)", last, *from)
	}

	if !gate.Banner() {
		fmt.Fprintf(os.Stderr, "ch-contract-ledgers-backfill: would fill stellar.contract_active_ledgers for ledgers %d..%d (window %d) on %s\n",
			*from, last, *window, *chAddr)
		return nil
	}
	fmt.Fprintf(os.Stderr, "ch-contract-ledgers-backfill: filling stellar.contract_active_ledgers for ledgers %d..%d (window %d) on %s\n",
		*from, last, *window, *chAddr)
	return clickhouse.BackfillContractActiveLedgers(ctx, *chAddr, uint32(*from), last, uint32(*window),
		func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "ch-contract-ledgers-backfill: "+format+"\n", a...)
		})
}
