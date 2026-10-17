// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireRollupLock takes an exclusive, non-blocking flock on path and returns the release; job names the caller so a colliding operator
// can tell who holds it.
//
// Every rollup cycle runs TRUNCATE(staging) -> fill -> EXCHANGE TABLES on global staging tables; two racing processes can truncate an arm
// the other is mid-INSERT into, or both EXCHANGE. systemd only serializes the timer against itself, not a manual `stellarindex-ops <job>`.
// Contention fails closed, never blocks.
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
	if err := handLockToDirOwner(f, filepath.Dir(path)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: chown lock file %s: %w", job, path, err)
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

// handLockToDirOwner gives a lock file a root run created to its directory's owner; left root-owned 0600, the unit's
// non-root user could never open it again.
func handLockToDirOwner(f *os.File, dir string) error {
	dst, err := os.Stat(dir)
	if err != nil {
		return err
	}
	fst, err := f.Stat()
	if err != nil {
		return err
	}
	d, dok := dst.Sys().(*syscall.Stat_t)
	l, lok := fst.Sys().(*syscall.Stat_t)
	if !dok || !lok {
		return nil
	}
	uid, gid, need := lockChownTarget(os.Geteuid(), d.Uid, d.Gid, l.Uid)
	if !need {
		return nil
	}
	return f.Chown(uid, gid)
}

// lockChownTarget reports whether a process with euid must chown a lock file owned by fileUID to its directory's owner.
func lockChownTarget(euid int, dirUID, dirGID, fileUID uint32) (int, int, bool) {
	if euid != 0 || fileUID == dirUID {
		return 0, 0, false
	}
	return int(dirUID), int(dirGID), true
}
