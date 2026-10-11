package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// holdersRollupLockPath is where chHoldersRollup takes its exclusive advisory lock (see
// acquireHoldersRollupLock), in the state directory the sibling systemd units already run from.
const holdersRollupLockPath = "/var/lib/stellarindex/ch-holders-rollup.lock"

// ch-holders-rollup recomputes every asset's top-500 holders board and holder count into staging
// tables and atomically EXCHANGEs them live (deploy/clickhouse/asset_holders_rollup.sql). The two
// ledger_entries_current FINAL scans are too heavy to run PER REQUEST, so a 30-minute timer runs
// them once per cycle and the API serves keyed sub-millisecond reads. An exclusive advisory lock
// (acquireRollupLock in rollup_lock.go) is taken first, since systemd's single-instance guarantee
// covers only the timer and a hand-run `stellarindex-ops ch-holders-rollup` bypasses it. Dry run
// by default (opsutil.WriteGate): without -write it takes no lock and opens no connection; the
// systemd unit passes -write explicitly.
func chHoldersRollup(args []string) error {
	fs := flag.NewFlagSet("ch-holders-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	lockPath := fs.String("lock-file", holdersRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the 30-minute timer or a second concurrent invocation")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/last-success gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_ch-holders-rollup.prom when that directory exists (r1), otherwise no heartbeat at all")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !gate.Banner() {
		fmt.Fprintln(os.Stderr, "ch-holders-rollup: DRY RUN — would recompute every asset's top-500 holders board + holder count and EXCHANGE it live; pass -write to apply")
		return nil
	}

	unlock, err := acquireRollupLock("ch-holders-rollup", *lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	// The timer runs unattended, so a cycle that silently falls behind (no crash, no progress)
	// needs its own signal: the shared heartbeat publishes stellarindex_ops_job_last_finish_unix
	// and _last_exit_ok, which the generic ops_job alerts already cover.
	hb := opsutil.NewJobHeartbeat("ch-holders-rollup", *heartbeat, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "ch-holders-rollup: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	ok := false
	defer func() { hb.Stop(ok) }()

	start := time.Now()
	if err := clickhouse.RunHoldersRollup(ctx, *chAddr, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "ch-holders-rollup: "+format+"\n", a...)
	}); err != nil {
		return err
	}
	ok = true
	fmt.Fprintf(os.Stderr, "ch-holders-rollup: cycle complete in %s\n", time.Since(start).Round(time.Second))
	return nil
}
