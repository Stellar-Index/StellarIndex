// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireRollupLock takes an exclusive, non-blocking flock on path and
// returns the release function. job names the caller in every message so
// an operator who collides with a held lock can tell which rollup it
// belongs to.
//
// Every rollup cycle (ch-holders-rollup, ch-creators-rollup,
// ch-sponsors-rollup, ch-cohort-rollup) reaches its own TRUNCATE(staging)
// -> fill -> EXCHANGE TABLES the same way a manual invocation and its
// 30-minute timer both would: two processes racing through that sequence
// on the same global staging tables have no safe outcome, one can
// truncate an arm the other is mid-INSERT into, or both can issue the
// final EXCHANGE concurrently. systemd's single-instance guarantee only
// serializes the timer against ITSELF, not against a manual
// `stellarindex-ops <job>` run from a shell, so the guard belongs here.
//
// Falls back to a temp-dir path when the preferred directory does not
// exist (dev box, or before deploy has provisioned it), so the exclusion
// holds everywhere rather than only on hosts an operator remembered to
// create /var/lib/stellarindex on. Contention is reported as an error
// (fail closed) rather than blocked on: these are bounded-cadence jobs,
// not a queue, and an operator who sees "already running" can simply
// retry rather than the process sitting in an indefinite wait.
func acquireRollupLock(job, path string) (func(), error) {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if st, statErr := os.Stat(dir); statErr != nil || !st.IsDir() {
		path = filepath.Join(os.TempDir(), filepath.Base(path))
	}
	// path is an operator-supplied -lock-file flag; filepath.Clean above
	// resolves any ../ traversal before it reaches OpenFile (gosec G304).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%s: open lock file %s: %w", job, path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %s is already locked by another run (the timer or a concurrent invocation is mid-cycle): %w", job, path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
