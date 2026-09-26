// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireHoldersRollupLockSerializesConcurrentRuns pins T572: a manual
// `stellarindex-ops ch-holders-rollup` invocation must not be able to run
// while another invocation (the 30-minute timer, or a second manual run) is
// still mid-cycle. Proven red against the pre-fix ch_holders_rollup.go,
// which called neither flock nor any other advisory-lock primitive —
// acquireHoldersRollupLock did not exist, so a second invocation always
// proceeded straight into RunHoldersRollup alongside the first.
func TestAcquireHoldersRollupLockSerializesConcurrentRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")

	release, err := acquireRollupLock("ch-holders-rollup", path)
	if err != nil {
		t.Fatalf("first acquireHoldersRollupLock: %v", err)
	}

	if _, err := acquireRollupLock("ch-holders-rollup", path); err == nil {
		t.Fatal("a second run acquired the lock while the first still holds it — runs are not serialized")
	} else if !strings.Contains(err.Error(), "already locked") {
		t.Errorf("error does not describe the contention: %v", err)
	}

	release()

	release2, err := acquireRollupLock("ch-holders-rollup", path)
	if err != nil {
		t.Fatalf("acquireHoldersRollupLock after release: %v", err)
	}
	release2()
}

// TestAcquireHoldersRollupLockFallsBackWhenDirMissing: a dev box (or a
// host before deploy has provisioned /var/lib/stellarindex) must still get
// a working exclusion, not a silently-disabled one.
func TestAcquireHoldersRollupLockFallsBackWhenDirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "ch-holders-rollup.lock")

	release, err := acquireRollupLock("ch-holders-rollup", missing)
	if err != nil {
		t.Fatalf("acquireHoldersRollupLock with a missing preferred dir: %v", err)
	}
	defer release()

	if _, err := acquireRollupLock("ch-holders-rollup", missing); err == nil {
		t.Fatal("a second run acquired the lock via the fallback path while the first still holds it")
	}
}
