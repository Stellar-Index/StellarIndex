// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestChCensusRollupDryRunTouchesNoClickHouse proves the behavioural half:
// against an unreachable ClickHouse address, the default (no -write) run
// must return nil — it never dials out — while -write must fail trying to
// connect. -from-day sidesteps the incremental path's own CensusMaxDay
// read so the loop body is the only thing under test.
func TestChCensusRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1" // reserved port, nothing ever listens here
	today := time.Now().UTC().Format("2006-01-02")

	if err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}

// TestChHoldersRollupDryRunTouchesNoClickHouse mirrors the census case: a
// dry run must neither take the advisory lock nor dial ClickHouse, and
// -write must fail against an unreachable address.
func TestChHoldersRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1"
	lockPath := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")

	if err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}
