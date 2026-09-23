// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

//go:build t688evidence

package chops

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── T688: a -write run never EXECUTES its own CAGG follow-up ───────────
//
// restampCAGGFollowUp (usd_volume_restamp_xlmbase.go) renders the ordered
// `CALL refresh_continuous_aggregate(...)` block every finished restamp
// needs, and xlmBaseRestampRun.summary prints it after every run,
// including a -write run that actually changed `trades`. Nothing in this
// package ever sends that SQL: xlmBaseRestampStore (this file),
// xlmQuoteRestampStore / cexFiatRestampStore (usd_volume_restamp_mirrors.go)
// and estimatedChunkApply (usd_volume_restamp_chunks_estimated.go) carry
// only Plan/Apply methods — no ExecContext, no
// timescale.Store.RefreshContinuousAggregate. The SAME shared primitive
// is also printed (never executed) by the exact tier's in-place run
// (usd_volume_restamp.go:317) and its chunk walk
// (usd_volume_restamp_chunks_exact.go's finish -> exactRestampSummary),
// both OUTSIDE this unit's file fence.
//
// This is proven here by construction, not by a mock assertion: evidenceStore
// below satisfies xlmBaseRestampStore with ONLY Plan+Apply — the compiler
// itself confirms runXLMBaseRestamp needs no execution capability to
// finish a -write run, and the captured stdout shows the operator is
// handed SQL text instead of a completed refresh.
//
// NOT auto-fixed: restampCAGGFollowUp's print-only contract is the
// reviewed #372-F3 resolution (see TestXLMBaseRestampFollowUp_* in
// usd_volume_report_claims_test.go: "The report must therefore print the
// refresh sequence itself"), so turning it into an executor is a product
// decision (block a CLI invocation on a multi-hour refresh? new flag?
// failure/exit-code semantics?) that also requires widening the shared
// chunkRestampTier.finish contract the exact tier (outside this fence)
// implements. Filed as NEEDS-COORDINATION rather than guessed at here.
// Build-tagged evidence for whoever picks that up:
//
//	go test -tags t688evidence ./internal/ops/chops/ -run TestWriteRunNeverExecutesItsOwnCAGGFollowUp -v
type evidenceStore struct{}

func (evidenceStore) PlanXLMBaseUSDVolumeRestamp(_ context.Context, _ timescale.XLMBaseRestampParams) (*timescale.XLMBaseRestampPlan, error) {
	return &timescale.XLMBaseRestampPlan{Stats: timescale.NewXLMBaseRestampStats()}, nil
}

func (evidenceStore) ApplyXLMBaseUSDVolumeRestamp(_ context.Context, _ *timescale.XLMBaseRestampPlan, _ int64, _ int) (int64, error) {
	return 0, nil
}

func TestWriteRunNeverExecutesItsOwnCAGGFollowUp(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout = w
	os.Stderr = w
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from
	runErr := runXLMBaseRestamp(context.Background(), evidenceStore{}, "/etc/stellarindex.toml", from, to, xlmBaseRestampOptions{
		Slice: 24 * time.Hour, Batch: 2000, Write: true, SampleSize: 3, MaxGeneration: -1,
	})
	os.Stdout, os.Stderr = origOut, origErr
	_ = w.Close()
	out := <-done
	_ = r.Close()

	if runErr != nil {
		t.Fatalf("runXLMBaseRestamp: %v", runErr)
	}
	// The operator is handed the CALL text...
	if !bytes.Contains([]byte(out), []byte("CALL refresh_continuous_aggregate('prices_1m'")) {
		t.Fatalf("expected the printed CAGG follow-up in a -write run's output, got:\n%s", out)
	}
	// ...which is exactly the defect: a -write run that changed `trades`
	// leaves every served surface stale until a human pastes that SQL in
	// by hand. evidenceStore proves no execution path exists to do it
	// automatically: xlmBaseRestampStore, the only store seam this run
	// type is given, has no method that could have sent it.
}
