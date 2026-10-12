// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcquireHoldersRollupLockSerializesConcurrentRuns: a manual ch-holders-rollup cannot run while another invocation
// (the 30-minute timer or a manual run) is mid-cycle. Without acquireHoldersRollupLock a second run enters
// RunHoldersRollup alongside the first.
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

// TestChHoldersRollupWiresHeartbeat: without a heartbeat the only signal is a systemd unit-failed state, so a cycle that
// falls behind silently is invisible. -write against an unreachable ClickHouse must still publish the shared ops_job
// textfile with last_exit_ok=0, as ch-backfill and usd-volume-restamp do.
func TestChHoldersRollupWiresHeartbeat(t *testing.T) {
	unreachable := "127.0.0.1:1"
	lockPath := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")
	hbPath := filepath.Join(t.TempDir(), "ops_job_ch-holders-rollup.prom")

	err := Run([]string{
		"ch-holders-rollup",
		"-ch-addr", unreachable,
		"-lock-file", lockPath,
		"-heartbeat", hbPath,
		"-write",
	})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}

	raw, readErr := os.ReadFile(hbPath) //nolint:gosec // test-controlled temp path
	if readErr != nil {
		t.Fatalf("heartbeat textfile was not written at %s: %v", hbPath, readErr)
	}
	body := string(raw)

	if !strings.Contains(body, `stellarindex_ops_job_running{ops_job="ch-holders-rollup"} 0`) {
		t.Errorf("running gauge missing/wrong for ops_job=ch-holders-rollup:\n%s", body)
	}
	if !strings.Contains(body, `stellarindex_ops_job_last_exit_ok{ops_job="ch-holders-rollup"} 0`) {
		t.Errorf("last_exit_ok must be 0 for a failed cycle (the freshness/failure signal T391 found missing):\n%s", body)
	}
	if !strings.Contains(body, "stellarindex_ops_job_last_finish_unix{ops_job=\"ch-holders-rollup\"}") {
		t.Errorf("last_finish_unix missing — no last-run timestamp published:\n%s", body)
	}
}
