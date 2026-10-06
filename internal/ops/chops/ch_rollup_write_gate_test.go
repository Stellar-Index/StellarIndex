// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
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

// TestBoardRollupsRequireAStatedMode pins the timer-driven board rollups to
// the shared gate with RequireStatedMode: a unit still calling them flagless
// must fail, not preview and exit 0 while the boards silently go stale. A
// dry run neither takes the lock nor dials ClickHouse; -write gets past the
// gate and fails on the unreachable store.
func TestBoardRollupsRequireAStatedMode(t *testing.T) {
	unreachable := "127.0.0.1:1"
	missingConfig := filepath.Join(t.TempDir(), "absent.toml")
	for _, tc := range []struct {
		verb  string
		extra []string
	}{
		{"ch-creators-rollup", nil},
		{"ch-sponsors-rollup", nil},
		// The cohort rollup loads its config before any store; an absent file
		// fails there, which is past the gate.
		{"ch-cohort-rollup", []string{"-config", missingConfig}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), tc.verb+".lock")
			args := append([]string{tc.verb, "-ch-addr", unreachable, "-lock-file", lockPath}, tc.extra...)

			if err := Run(args); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
				t.Errorf("flagless run returned %v, want ErrWriteModeUnstated", err)
			}
			if err := Run(append(args, "-dry-run")); err != nil {
				t.Errorf("-dry-run returned %v, want nil: it must not touch the lock, config or ClickHouse", err)
			}
			if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a gated-out run created the rollup lock %s (stat err %v)", lockPath, err)
			}
			err := Run(append(args, "-write"))
			if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) {
				t.Fatalf("-write returned %v, want a store or config error: the write path was never reached", err)
			}
		})
	}
}

// TestBoardRollupUnitsPassWrite pins the unit side: every rollup timer runs
// a write-gated command, so a unit missing -write would only ever preview.
func TestBoardRollupUnitsPassWrite(t *testing.T) {
	for _, unit := range []string{
		"census-rollup.service.j2",
		"holders-rollup.service.j2",
		"creators-rollup.service.j2",
		"sponsors-rollup.service.j2",
		"cohort-rollup.service.j2",
	} {
		body := readRepoFile(t, "configs/ansible/roles/archival-node/templates/systemd/"+unit)
		start := strings.Index(body, "ExecStart=")
		if start < 0 {
			t.Fatalf("%s has no ExecStart", unit)
		}
		execStart := body[start:]
		if end := strings.Index(execStart, "\n\n"); end >= 0 {
			execStart = execStart[:end]
		}
		if !strings.Contains(execStart, "\n  -write \\\n") {
			t.Errorf("%s ExecStart does not pass -write; the timer would fail on every run:\n%s", unit, execStart)
		}
	}
}
