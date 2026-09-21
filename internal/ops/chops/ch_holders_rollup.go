package chops

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
// it entirely. See acquireHoldersRollupLock for the failure that closes.
func chHoldersRollup(args []string) error {
	fs := flag.NewFlagSet("ch-holders-rollup", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	lockPath := fs.String("lock-file", holdersRollupLockPath,
		"path to the exclusive advisory lock serializing this run against the 30-minute timer or a second concurrent invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	unlock, err := acquireHoldersRollupLock(*lockPath)
	if err != nil {
		return err
	}
	defer unlock()

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

// acquireHoldersRollupLock takes an exclusive, non-blocking flock on path
// and returns the release function.
//
// Two processes racing through TRUNCATE→fill→EXCHANGE on the same staging
// tables have no safe outcome: one can truncate a staging arm the other is
// mid-INSERT into, or both can issue the final EXCHANGE TABLES concurrently.
// A manual `stellarindex-ops ch-holders-rollup` invocation and the
// 30-minute timer both reach RunHoldersRollup the same way, so the guard
// belongs here rather than in the systemd unit, which only serializes
// against ITSELF.
//
// Falls back to a temp-dir path when the preferred directory does not
// exist (dev box, or before deploy has provisioned it), so the exclusion
// holds everywhere rather than only on hosts an operator remembered to
// create /var/lib/stellarindex on. Contention is reported as an error
// (fail closed) rather than blocked on: this is a bounded 30-minute-cadence
// job, not a queue, and an operator who sees "already running" can simply
// retry rather than the process sitting in an indefinite wait.
func acquireHoldersRollupLock(path string) (func(), error) {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
		path = filepath.Join(os.TempDir(), filepath.Base(path))
	}
	// path is an operator-supplied -lock-file flag (default
	// holdersRollupLockPath); filepath.Clean above resolves any ../
	// traversal before it reaches OpenFile (gosec G304).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ch-holders-rollup: open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ch-holders-rollup: %s is already locked by another run (the 30-minute timer or a concurrent invocation is mid-cycle): %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
