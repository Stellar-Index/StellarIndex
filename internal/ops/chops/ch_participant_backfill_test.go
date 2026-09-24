// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestChParticipantBackfill_FailureIsNotReportedDone — a run whose
// backfill returns an error must not print the "done — wrote N rows"
// completion line; the operator reading stderr would take a failed run
// for a finished one. ClickHouse on a closed port fails the run at once;
// the bound resolves from a stub so the failure lands in the pass itself.
func TestChParticipantBackfill_FailureIsNotReportedDone(t *testing.T) {
	stubLakeContiguousThrough(t, 10)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	runErr := chParticipantBackfill([]string{"-ch-addr", "127.0.0.1:1", "-from", "2", "-to", "10"})
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)

	if runErr == nil {
		t.Fatal("expected an error against an unreachable ClickHouse")
	}
	if strings.Contains(string(out), "done —") {
		t.Errorf("failed run printed the completion line:\n%s", out)
	}
	if !strings.Contains(string(out), "FAILED") {
		t.Errorf("failed run must say it FAILED:\n%s", out)
	}
}
