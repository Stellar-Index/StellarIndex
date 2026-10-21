package chops

import (
	"flag"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chOpTypeRollup maintains the per-account counts rollups, stellar.account_op_type_counts and stellar.account_movement_counts
// (deploy/clickhouse/account_op_types.sql Step 6): each lake partition is one count() FINAL + atomic REPLACE PARTITION, the open one stopping an hour behind the contiguous
// tip. A recompute smaller than the live partition is refused unless -shrink-ok. -max-closed 0 is the backfill: heavy, serialize it
// under run-heavy-job.sh. The dry run reads the plan and writes nothing.
func chOpTypeRollup(args []string) error {
	fs := flag.NewFlagSet("ch-optype-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	maxClosed := fs.Int("max-closed", 4, "closed partitions rebuilt per run, newest first (0 = all: the backfill)")
	shrinkOK := fs.Bool("shrink-ok", false, "allow replacing a partition with a recompute that counts fewer rows")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := gate.Banner()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-optype-rollup: "+format+"\n", a...)
	}
	for _, rc := range clickhouse.CountsRollups {
		work, err := rc.Plan(ctx, *chAddr, *maxClosed)
		if err != nil {
			return err
		}
		if len(work) == 0 {
			logf("%s: up to date", rc.Name)
		}
		for _, w := range work {
			if !write {
				logf("DRY RUN: %s: would rebuild partition %d through %d (%s); pass -write to apply", rc.Name, w.Part, w.Through, w.Reason)
				continue
			}
			if err := rc.RunPartition(ctx, *chAddr, w, *shrinkOK, logf); err != nil {
				return err
			}
		}
	}
	return nil
}
