// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// TestFinishProjectedRebuild_HeldWindowReachesTheHeartbeat pins that a run
// which lost rows is visible to stellarindex_ops_job_run_failed, not only
// to a caller that reads the exit code.
func TestFinishProjectedRebuild_HeldWindowReachesTheHeartbeat(t *testing.T) {
	cases := []struct {
		name        string
		result      ProjectedRebuildResult
		runErr      error
		interrupted bool
		wantErr     error
		wantExitOK  string
	}{
		{name: "held window", result: ProjectedRebuildResult{WindowsHeld: 1, InsertErrors: 3}, wantErr: errProjectedRebuildIncomplete, wantExitOK: "0"},
		{name: "permanent drop", result: ProjectedRebuildResult{PermanentDrops: 2}, wantErr: errProjectedRebuildIncomplete, wantExitOK: "0"},
		{name: "interrupted", runErr: context.Canceled, interrupted: true, wantExitOK: "0"},
		{name: "clean", wantExitOK: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hb.prom")
			hb := opsutil.NewJobHeartbeat("projected-rebuild-blend", path, nil)
			hb.Start()
			err := finishProjectedRebuild(hb, tc.result, tc.runErr, tc.interrupted)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("want exit 0, got %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			want := `stellarindex_ops_job_last_exit_ok{ops_job="projected-rebuild-blend"} ` + tc.wantExitOK
			if !strings.Contains(string(body), want) {
				t.Fatalf("heartbeat missing %q:\n%s", want, body)
			}
		})
	}
}

// TestProjectedRebuildProgressLoop_FeedsTheHeartbeat pins that a heartbeat
// run publishes covered ledgers, so stellarindex_ops_job_no_progress does not
// ticket a healthy rebuild whose progress_total would otherwise sit at 0.
func TestProjectedRebuildProgressLoop_FeedsTheHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb.prom")
	hb := opsutil.NewJobHeartbeat("projected-rebuild-blend", path, nil)
	hb.Start()
	counters := newProjectedRebuildCounters()
	counters.completedLedgers.Store(50000)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runProjectedRebuildProgressLoop(ctx, logger, "blend", 5*time.Millisecond, time.Now(), 1, 50000, counters, hb)
	hb.Stop(true)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `stellarindex_ops_job_progress_total{ops_job="projected-rebuild-blend"} 50000`
	if !strings.Contains(string(body), want) {
		t.Fatalf("heartbeat missing %q:\n%s", want, body)
	}
}
