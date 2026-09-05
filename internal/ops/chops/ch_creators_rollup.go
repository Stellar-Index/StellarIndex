package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ch-creators-rollup — #351: recompute the account-creator league table
// (funder → accounts created, with the created set's surviving accounts
// and current XLM) into staging and atomically exchange it live
// (deploy/clickhouse/account_creators_rollup.sql).
//
// The aggregation is scan-shaped over stellar.account_movements because
// movement_kind is not in that table's ORDER BY, so it is a cycle cost
// paid once, not a per-request cost: the endpoint reads a keyed board
// and seven metric rows.
//
// The same cycle writes the coverage span it aggregated, so the API
// never has to assume the board covers the whole chain.
func chCreatorsRollup(args []string) error {
	fs := flag.NewFlagSet("ch-creators-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	start := time.Now()
	if err := clickhouse.RunCreatorsRollup(ctx, *chAddr, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-creators-rollup: "+format+"\n", a...)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ch-creators-rollup: cycle complete in %s\n", time.Since(start).Round(time.Second))
	return nil
}
