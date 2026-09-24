// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChHoldersRollupWiresHeartbeat proves T391's remaining gap is closed:
// the 30-minute timer's only prior signal was a systemd unit-failed state
// on an outright crash, so a cycle that fell behind without erroring was
// invisible. -write against an unreachable ClickHouse address fails
// (mirrors TestChHoldersRollupDryRunTouchesNoClickHouse), but the run must
// still publish the shared ops_job textfile with last_exit_ok=0 — the same
// last-success/failure signal ch-backfill and usd-volume-restamp already
// expose to the generic ops_job alert tree.
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
