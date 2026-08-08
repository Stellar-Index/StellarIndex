package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ch-holders-rollup — inventory #4: recompute every asset's top-500
// holders board + holder count into staging tables and atomically
// EXCHANGE them live (deploy/clickhouse/asset_holders_rollup.sql). The
// two ledger_entries_current FINAL scans this runs are exactly what
// GET /v1/assets/{id}/holders used to run PER REQUEST; a 30-minute
// timer runs them once per cycle instead, and the API serves keyed
// sub-millisecond reads (sub-second page goal, 2026-08-08).
func chHoldersRollup(args []string) error {
	fs := flag.NewFlagSet("ch-holders-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	start := time.Now()
	if err := clickhouse.RunHoldersRollup(ctx, *chAddr, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-holders-rollup: "+format+"\n", a...)
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "ch-holders-rollup: cycle complete in %s\n", time.Since(start).Round(time.Second))
	return nil
}
