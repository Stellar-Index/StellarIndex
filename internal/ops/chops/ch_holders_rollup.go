package chops

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// holdersRollupLockPath is where chHoldersRollup takes its exclusive
// advisory lock (see acquireHoldersRollupLock). /var/lib/stellarindex is
// the r1 state directory every long-running stellarindex-* unit already
// runs from (WorkingDirectory=/var/lib/stellarindex on the sibling
// systemd units), so the lock survives a reboot at the same place an
// operator already looks.
const holdersRollupLockPath = "/var/lib/stellarindex/ch-holders-rollup.lock"

// ch-holders-rollup — inventory #4: recompute every asset's top-500
// holders board + holder count into staging tables and atomically
// EXCHANGE them live (deploy/clickhouse/asset_holders_rollup.sql). The
// two ledger_entries_current FINAL scans this runs are exactly what
// GET /v1/assets/{id}/holders used to run PER REQUEST; a 30-minute
// timer runs them once per cycle instead, and the API serves keyed
// sub-millisecond reads (sub-second page goal, 2026-08-08).
//
// Takes an exclusive advisory lock before it runs anything: the timer
// and a manually-invoked run both resolve to this same function, and
// systemd's own single-instance guarantee only covers the FIRST —
// `stellarindex-ops ch-holders-rollup` run by hand from a shell bypasses
// it entirely. See acquireRollupLock (rollup_lock.go) for the failure
// that closes.
//
// Fail-closed DRY RUN by default (opsutil.WriteGate): without -write
// this reports the recompute it would run and takes no lock, opens no
// connection. The holders-rollup systemd unit passes -write explicitly.
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

	// T391: the timer runs this unattended every 30 minutes with no other
	// success/failure signal — a cycle that silently falls behind (no
	// crash, just no progress) previously had no metric at all. The same
	// heartbeat primitive ch-backfill and usd-volume-restamp already use
	// publishes stellarindex_ops_job_last_finish_unix /
	// _last_exit_ok, which the existing generic ops_job alert tree covers
	// without a new alert.
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
